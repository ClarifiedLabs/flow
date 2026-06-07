package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
	flowtoken "github.com/ClarifiedLabs/flow/internal/token"
)

type WebBootstrap struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type WebSession struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type WebSessionService struct {
	db  *sql.DB
	now func() time.Time
}

func NewWebSessionService(database *sql.DB) *WebSessionService {
	return &WebSessionService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

func (s *WebSessionService) CreateBootstrap(ctx context.Context, ttl time.Duration) (WebBootstrap, error) {
	if ttl <= 0 {
		return WebBootstrap{}, errors.New("bootstrap ttl must be positive")
	}
	token, err := randomWebToken()
	if err != nil {
		return WebBootstrap{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(ttl)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO web_bootstrap_tokens (
	token_hash,
	expires_at,
	created_at
) VALUES (?, ?, ?)`,
		HashToken(token),
		formatTime(expiresAt),
		formatTime(now),
	); err != nil {
		return WebBootstrap{}, fmt.Errorf("create web bootstrap token: %w", err)
	}

	return WebBootstrap{Token: token, ExpiresAt: expiresAt}, nil
}

func (s *WebSessionService) ConsumeBootstrap(ctx context.Context, token string, ttl time.Duration) (WebSession, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return WebSession{}, ErrInvalidCredential
	}
	if ttl <= 0 {
		return WebSession{}, errors.New("web session ttl must be positive")
	}
	now := s.now().UTC()
	nowText := formatTime(now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebSession{}, fmt.Errorf("begin web login transaction: %w", err)
	}
	defer tx.Rollback()

	var expiresAtText string
	var usedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT expires_at, used_at
FROM web_bootstrap_tokens
WHERE token_hash = ?`, HashToken(token)).Scan(&expiresAtText, &usedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebSession{}, ErrInvalidCredential
		}
		return WebSession{}, fmt.Errorf("load web bootstrap token: %w", err)
	}
	expiresAt, err := parseTime(expiresAtText)
	if err != nil {
		return WebSession{}, err
	}
	if usedAt.Valid || !now.Before(expiresAt) {
		return WebSession{}, ErrInvalidCredential
	}
	result, err := tx.ExecContext(ctx, `
UPDATE web_bootstrap_tokens
SET used_at = ?
WHERE token_hash = ?
	AND used_at IS NULL`,
		nowText,
		HashToken(token),
	)
	if err != nil {
		return WebSession{}, fmt.Errorf("consume web bootstrap token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return WebSession{}, fmt.Errorf("read consumed web bootstrap token rows affected: %w", err)
	}
	if affected != 1 {
		return WebSession{}, ErrInvalidCredential
	}
	sessionToken, err := randomWebToken()
	if err != nil {
		return WebSession{}, err
	}
	csrfToken, err := randomWebToken()
	if err != nil {
		return WebSession{}, err
	}
	sessionID, err := randomPrefixedID("web")
	if err != nil {
		return WebSession{}, err
	}
	sessionExpiresAt := now.Add(ttl)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO web_sessions (
	id,
	token_hash,
	csrf_token_hash,
	expires_at,
	created_at,
	last_seen_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID,
		HashToken(sessionToken),
		HashToken(csrfToken),
		formatTime(sessionExpiresAt),
		nowText,
		nowText,
	); err != nil {
		return WebSession{}, fmt.Errorf("create web session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WebSession{}, fmt.Errorf("commit web login transaction: %w", err)
	}

	return WebSession{
		ID:        sessionID,
		Token:     sessionToken,
		CSRFToken: csrfToken,
		ExpiresAt: sessionExpiresAt,
	}, nil
}

func (s *WebSessionService) Authenticate(ctx context.Context, token string, csrfToken string, requireCSRF bool) (WebSession, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return WebSession{}, ErrInvalidCredential
	}
	now := s.now().UTC()
	var sessionID string
	var expiresAtText string
	var csrfHash string
	if err := s.db.QueryRowContext(ctx, `
SELECT id, expires_at, csrf_token_hash
FROM web_sessions
WHERE token_hash = ?`, HashToken(token)).Scan(&sessionID, &expiresAtText, &csrfHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WebSession{}, ErrInvalidCredential
		}
		return WebSession{}, fmt.Errorf("load web session: %w", err)
	}
	expiresAt, err := parseTime(expiresAtText)
	if err != nil {
		return WebSession{}, err
	}
	if !now.Before(expiresAt) {
		return WebSession{}, ErrInvalidCredential
	}
	if requireCSRF && HashToken(csrfToken) != csrfHash {
		return WebSession{}, ErrInvalidCredential
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE web_sessions
SET last_seen_at = ?
WHERE id = ?`, formatTime(now), sessionID); err != nil {
		return WebSession{}, fmt.Errorf("touch web session: %w", err)
	}

	return WebSession{
		ID:        sessionID,
		ExpiresAt: expiresAt,
	}, nil
}

func randomWebToken() (string, error) {
	return flowtoken.Generate()
}
