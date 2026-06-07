package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

const DefaultReviewAuthorCycleLimit = 5

var ErrReviewCycleLimitReached = errors.New("review-author cycle limit reached")

type ReviewCycleBudget struct {
	IssueID            string     `json:"issue_id"`
	GrantedCycles      int        `json:"granted_cycles"`
	UsedCycles         int        `json:"used_cycles"`
	RemainingCycles    int        `json:"remaining_cycles"`
	Exhausted          bool       `json:"exhausted"`
	ExhaustedAt        *time.Time `json:"exhausted_at,omitempty"`
	LastApprovedAt     *time.Time `json:"last_approved_at,omitempty"`
	LastApprovedBy     string     `json:"last_approved_by,omitempty"`
	LastInstructions   string     `json:"last_instructions,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DefaultGrantCycles int        `json:"default_grant_cycles"`
}

type ApproveReviewCyclesInput struct {
	IssueID      string
	Cycles       int
	Instructions string
	Actor        string
}

type ReviewCycleService struct {
	db           *sql.DB
	defaultGrant int
	now          func() time.Time
}

func NewReviewCycleService(database *sql.DB, defaultGrant int) *ReviewCycleService {
	if defaultGrant <= 0 {
		defaultGrant = DefaultReviewAuthorCycleLimit
	}
	return &ReviewCycleService{
		db:           database,
		defaultGrant: defaultGrant,
		now:          sqlitex.UTCNow,
	}
}

func (s *ReviewCycleService) Get(ctx context.Context, issueID string) (ReviewCycleBudget, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ReviewCycleBudget{}, errors.New("issue id is required")
	}
	budget, err := s.get(ctx, s.db, issueID)
	if errors.Is(err, sql.ErrNoRows) {
		now := s.now().UTC()
		return ReviewCycleBudget{
			IssueID:            issueID,
			GrantedCycles:      s.defaultGrant,
			RemainingCycles:    s.defaultGrant,
			UpdatedAt:          now,
			DefaultGrantCycles: s.defaultGrant,
		}, nil
	}
	return budget, err
}

func (s *ReviewCycleService) Consume(ctx context.Context, issueID string, actor string) (ReviewCycleBudget, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ReviewCycleBudget{}, errors.New("issue id is required")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "system"
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ReviewCycleBudget{}, err
	}
	defer tx.Rollback()

	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_cycle_budgets (
	issue_id,
	granted_cycles,
	used_cycles,
	updated_at
) VALUES (?, ?, 0, ?)
ON CONFLICT(issue_id) DO NOTHING`,
		issueID,
		s.defaultGrant,
		nowText,
	); err != nil {
		return ReviewCycleBudget{}, fmt.Errorf("ensure review cycle budget: %w", err)
	}

	budget, err := s.get(ctx, tx, issueID)
	if err != nil {
		return ReviewCycleBudget{}, err
	}
	if budget.UsedCycles >= budget.GrantedCycles {
		firstExhaustion := budget.ExhaustedAt == nil
		if _, err := tx.ExecContext(ctx, `
UPDATE review_cycle_budgets
SET exhausted_at = COALESCE(exhausted_at, ?),
	updated_at = ?
WHERE issue_id = ?`,
			nowText,
			nowText,
			issueID,
		); err != nil {
			return ReviewCycleBudget{}, fmt.Errorf("mark review cycle budget exhausted: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ReviewCycleBudget{}, err
		}
		budget, loadErr := s.Get(ctx, issueID)
		if loadErr != nil {
			return ReviewCycleBudget{}, loadErr
		}
		if firstExhaustion {
			_, statusErr := NewStatusService(s.db).Write(ctx, WriteStatusInput{
				IssueID: issueID,
				Actor:   actor,
				Kind:    StatusKindBlocker,
				Message: fmt.Sprintf("Review-author cycle limit reached after %d automated send-backs. A human must approve another run before Flow starts another fix author session.", budget.UsedCycles),
			})
			if statusErr != nil {
				return budget, statusErr
			}
		}
		return budget, ErrReviewCycleLimitReached
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE review_cycle_budgets
SET used_cycles = used_cycles + 1,
	updated_at = ?
WHERE issue_id = ?`,
		nowText,
		issueID,
	); err != nil {
		return ReviewCycleBudget{}, fmt.Errorf("consume review cycle budget: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewCycleBudget{}, err
	}

	return s.Get(ctx, issueID)
}

func (s *ReviewCycleService) ApproveMore(ctx context.Context, input ApproveReviewCyclesInput) (ReviewCycleBudget, error) {
	input.IssueID = strings.TrimSpace(input.IssueID)
	input.Actor = strings.TrimSpace(input.Actor)
	input.Instructions = strings.TrimSpace(input.Instructions)
	if input.IssueID == "" {
		return ReviewCycleBudget{}, errors.New("issue id is required")
	}
	if input.Cycles == 0 {
		input.Cycles = s.defaultGrant
	}
	if input.Cycles < 0 {
		return ReviewCycleBudget{}, errors.New("review cycle approval cycles must not be negative")
	}
	if input.Instructions == "" {
		return ReviewCycleBudget{}, errors.New("review cycle approval instructions are required")
	}
	if input.Actor == "" {
		input.Actor = "owner"
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return ReviewCycleBudget{}, err
	}
	defer tx.Rollback()

	now := s.now().UTC()
	nowText := formatTime(now)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO review_cycle_budgets (
	issue_id,
	granted_cycles,
	used_cycles,
	exhausted_at,
	last_approved_at,
	last_approved_by,
	last_instructions,
	updated_at
) VALUES (?, ?, 0, NULL, ?, ?, ?, ?)
ON CONFLICT(issue_id) DO UPDATE SET
	granted_cycles = review_cycle_budgets.granted_cycles + ?,
	exhausted_at = NULL,
	last_approved_at = excluded.last_approved_at,
	last_approved_by = excluded.last_approved_by,
	last_instructions = excluded.last_instructions,
	updated_at = excluded.updated_at`,
		input.IssueID,
		s.defaultGrant+input.Cycles,
		nowText,
		input.Actor,
		input.Instructions,
		nowText,
		input.Cycles,
	); err != nil {
		return ReviewCycleBudget{}, fmt.Errorf("approve review cycles: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReviewCycleBudget{}, err
	}

	budget, err := s.Get(ctx, input.IssueID)
	if err != nil {
		return ReviewCycleBudget{}, err
	}
	_, err = NewStatusService(s.db).Write(ctx, WriteStatusInput{
		IssueID: input.IssueID,
		Actor:   input.Actor,
		Kind:    StatusKindProgress,
		Message: fmt.Sprintf("Approved %d more review-author cycles.\n\nInstructions:\n%s", input.Cycles, input.Instructions),
	})
	if err != nil {
		return ReviewCycleBudget{}, err
	}

	return budget, nil
}

func (s *ReviewCycleService) get(ctx context.Context, q queryer, issueID string) (ReviewCycleBudget, error) {
	row := q.QueryRowContext(ctx, `
SELECT issue_id, granted_cycles, used_cycles, exhausted_at, last_approved_at, last_approved_by, last_instructions, updated_at
FROM review_cycle_budgets
WHERE issue_id = ?`, issueID)
	return scanReviewCycleBudget(row, s.defaultGrant)
}

func reviewCycleBudgetExhausted(ctx context.Context, db *sql.DB, issueID string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM review_cycle_budgets
WHERE issue_id = ?
	AND exhausted_at IS NOT NULL
	AND used_cycles >= granted_cycles`, strings.TrimSpace(issueID)).Scan(&count); err != nil {
		return false, fmt.Errorf("check review cycle exhaustion: %w", err)
	}
	return count > 0, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanReviewCycleBudget(scanner issueScanner, defaultGrant int) (ReviewCycleBudget, error) {
	var budget ReviewCycleBudget
	var exhaustedAt sql.NullString
	var lastApprovedAt sql.NullString
	var updatedAt string
	if err := scanner.Scan(
		&budget.IssueID,
		&budget.GrantedCycles,
		&budget.UsedCycles,
		&exhaustedAt,
		&lastApprovedAt,
		&budget.LastApprovedBy,
		&budget.LastInstructions,
		&updatedAt,
	); err != nil {
		return ReviewCycleBudget{}, fmt.Errorf("scan review cycle budget: %w", err)
	}
	if exhaustedAt.Valid {
		parsed, err := parseTime(exhaustedAt.String)
		if err != nil {
			return ReviewCycleBudget{}, err
		}
		budget.ExhaustedAt = &parsed
	}
	if lastApprovedAt.Valid {
		parsed, err := parseTime(lastApprovedAt.String)
		if err != nil {
			return ReviewCycleBudget{}, err
		}
		budget.LastApprovedAt = &parsed
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return ReviewCycleBudget{}, err
	}
	budget.UpdatedAt = parsedUpdatedAt
	budget.RemainingCycles = budget.GrantedCycles - budget.UsedCycles
	if budget.RemainingCycles < 0 {
		budget.RemainingCycles = 0
	}
	budget.Exhausted = budget.ExhaustedAt != nil && budget.UsedCycles >= budget.GrantedCycles
	budget.DefaultGrantCycles = defaultGrant
	return budget, nil
}
