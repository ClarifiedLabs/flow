package coordinator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowtoken "github.com/ClarifiedLabs/flow/internal/token"
)

type TokenScope string

const (
	TokenScopeOwner   TokenScope = "owner"
	TokenScopeWorker  TokenScope = "worker"
	TokenScopeSession TokenScope = "session"
	TokenScopeConsole TokenScope = "console"
	TokenScopeHook    TokenScope = "hook"
)

var ErrInvalidCredential = errors.New("invalid bearer token")

type CredentialInput struct {
	Token         string
	Scope         TokenScope
	Subject       string
	ProjectID     *string
	SourceIssueID *string
	ExpiresAt     *time.Time
}

type Principal struct {
	Scope         TokenScope
	Subject       string
	TokenHash     string
	ProjectID     *string
	SourceIssueID *string
	WebSessionID  string
}

func (p Principal) IdempotencyPrincipalKey() string {
	return string(p.Scope) + ":" + p.Subject + ":" + p.TokenHash
}

// IsProjectBound reports whether the token is confined to one project
// database. Session tokens are issue/session credentials; console tokens are
// project-manager credentials for a single persistent console session.
func (p Principal) IsProjectBound() bool {
	if p.ProjectID == nil || strings.TrimSpace(*p.ProjectID) == "" {
		return false
	}
	return p.Scope == TokenScopeSession || p.Scope == TokenScopeConsole
}

// Actor renders the principal as the canonical "scope" or "scope:subject" actor
// string recorded in status and transition logs.
func (p Principal) Actor() string {
	subject := strings.TrimSpace(p.Subject)
	if subject == "" {
		return string(p.Scope)
	}

	return string(p.Scope) + ":" + subject
}

type CredentialService struct {
	db  *sql.DB
	now func() time.Time
}

func NewCredentialService(database *sql.DB) *CredentialService {
	return &CredentialService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *CredentialService) CreateToken(ctx context.Context, input CredentialInput) (string, error) {
	token, err := randomCredentialToken()
	if err != nil {
		return "", err
	}
	input.Token = token
	if err := s.EnsureToken(ctx, input); err != nil {
		return "", err
	}

	return token, nil
}

// ReplaceSubjectToken revokes every live token for the credential scope/subject
// and stores a fresh token. It is used for worker joins where a new process for
// the same worker id should replace any earlier process credential.
func (s *CredentialService) ReplaceSubjectToken(ctx context.Context, input CredentialInput) (string, error) {
	token, err := randomCredentialToken()
	if err != nil {
		return "", err
	}
	input.Token = token
	if err := s.ReplaceSubjectCredential(ctx, input); err != nil {
		return "", err
	}

	return token, nil
}

// ReplaceSubjectCredential revokes every live token for the credential
// scope/subject and stores the supplied token as the active replacement.
func (s *CredentialService) ReplaceSubjectCredential(ctx context.Context, input CredentialInput) error {
	input, err := normalizeCredentialInput(input)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin credential replacement: %w", err)
	}
	defer tx.Rollback()

	now := formatTime(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `
UPDATE tokens
SET revoked_at = COALESCE(revoked_at, ?)
WHERE scope = ?
	AND subject = ?`,
		now,
		string(input.Scope),
		input.Subject,
	); err != nil {
		return fmt.Errorf("revoke previous credentials: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO tokens (
	token_hash,
	scope,
	subject,
	project_id,
	source_issue_id,
	expires_at,
	revoked_at,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(token_hash) DO UPDATE SET
	scope = excluded.scope,
	subject = excluded.subject,
	project_id = excluded.project_id,
	source_issue_id = excluded.source_issue_id,
	expires_at = excluded.expires_at,
	revoked_at = NULL`,
		HashToken(input.Token),
		string(input.Scope),
		input.Subject,
		nullableStringValue(input.ProjectID),
		nullableStringValue(input.SourceIssueID),
		nullableTimeValue(input.ExpiresAt),
		now,
	); err != nil {
		return fmt.Errorf("store replacement credential: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit credential replacement: %w", err)
	}

	return nil
}

func (s *CredentialService) EnsureToken(ctx context.Context, input CredentialInput) error {
	input, err := normalizeCredentialInput(input)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO tokens (
	token_hash,
	scope,
	subject,
	project_id,
	source_issue_id,
	expires_at,
	revoked_at,
	created_at
) VALUES (?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT(token_hash) DO UPDATE SET
	scope = excluded.scope,
	subject = excluded.subject,
	project_id = excluded.project_id,
	source_issue_id = excluded.source_issue_id,
	expires_at = excluded.expires_at`,
		HashToken(input.Token),
		string(input.Scope),
		input.Subject,
		nullableStringValue(input.ProjectID),
		nullableStringValue(input.SourceIssueID),
		nullableTimeValue(input.ExpiresAt),
		formatTime(s.now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("store credential: %w", err)
	}

	return nil
}

func (s *CredentialService) Authenticate(ctx context.Context, token string) (Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Principal{}, ErrInvalidCredential
	}

	var principal Principal
	var scope string
	var projectID sql.NullString
	var sourceIssueID sql.NullString
	var expiresAt sql.NullString
	var revokedAt sql.NullString
	if err := s.db.QueryRowContext(ctx, `
SELECT scope, subject, token_hash, project_id, source_issue_id, expires_at, revoked_at
FROM tokens
WHERE token_hash = ?`, HashToken(token)).Scan(
		&scope,
		&principal.Subject,
		&principal.TokenHash,
		&projectID,
		&sourceIssueID,
		&expiresAt,
		&revokedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Principal{}, ErrInvalidCredential
		}
		return Principal{}, fmt.Errorf("authenticate credential: %w", err)
	}

	if revokedAt.Valid {
		return Principal{}, ErrInvalidCredential
	}
	if expiresAt.Valid {
		expires, err := parseTime(expiresAt.String)
		if err != nil {
			return Principal{}, err
		}
		if !s.now().UTC().Before(expires) {
			return Principal{}, ErrInvalidCredential
		}
	}

	principal.Scope = TokenScope(scope)
	principal.ProjectID = nullableStringPointer(projectID)
	principal.SourceIssueID = nullableStringPointer(sourceIssueID)
	return principal, nil
}

// RevokeTokenHash marks the credential with the given hash revoked. Revoking
// an unknown hash is a no-op so callers can revoke best-effort.
func (s *CredentialService) RevokeTokenHash(ctx context.Context, tokenHash string) error {
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return errors.New("token hash is required")
	}

	if _, err := s.db.ExecContext(ctx, `
UPDATE tokens
SET revoked_at = COALESCE(revoked_at, ?)
WHERE token_hash = ?`,
		formatTime(s.now().UTC()),
		tokenHash,
	); err != nil {
		return fmt.Errorf("revoke credential: %w", err)
	}

	return nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func normalizeCredentialInput(input CredentialInput) (CredentialInput, error) {
	input.Token = strings.TrimSpace(input.Token)
	input.Subject = strings.TrimSpace(input.Subject)
	if input.Token == "" {
		return CredentialInput{}, errors.New("credential token is required")
	}
	switch input.Scope {
	case TokenScopeOwner:
		if input.Subject == "" {
			input.Subject = "owner"
		}
	case TokenScopeHook:
		if input.Subject == "" {
			input.Subject = "hook"
		}
	case TokenScopeSession, TokenScopeConsole:
		if input.Subject == "" {
			return CredentialInput{}, fmt.Errorf("%s credential subject is required", input.Scope)
		}
		if input.ProjectID == nil || strings.TrimSpace(*input.ProjectID) == "" {
			return CredentialInput{}, fmt.Errorf("%s credential project is required", input.Scope)
		}
	case TokenScopeWorker:
		if input.Subject == "" {
			return CredentialInput{}, errors.New("worker credential subject is required")
		}
	default:
		return CredentialInput{}, fmt.Errorf("invalid credential scope: %s", input.Scope)
	}
	if input.ProjectID != nil {
		projectID := strings.TrimSpace(*input.ProjectID)
		if projectID == "" {
			input.ProjectID = nil
		} else {
			input.ProjectID = &projectID
		}
	}
	if input.SourceIssueID != nil {
		sourceIssueID := strings.TrimSpace(*input.SourceIssueID)
		if sourceIssueID == "" {
			input.SourceIssueID = nil
		} else {
			input.SourceIssueID = &sourceIssueID
		}
	}

	return input, nil
}

func randomCredentialToken() (string, error) {
	return flowtoken.Generate()
}

func nullableTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}

	return formatTime(value.UTC())
}
