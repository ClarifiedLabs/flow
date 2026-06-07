package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/handoff"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

type ReconcileResult struct {
	ProjectsScanned int      `json:"projects_scanned"`
	ProjectsSkipped int      `json:"projects_skipped"`
	BranchesScanned int      `json:"branches_scanned"`
	ChangesCreated  int      `json:"changes_created"`
	ChangesUpdated  int      `json:"changes_updated"`
	SkippedProjects []string `json:"skipped_projects"`
	UnknownBranches []string `json:"unknown_branches"`
	// UpdatedChanges lists the changes whose stored head was created or moved
	// to match the actual branch tip during this pass, so callers (the git
	// event consumer) can reset stale per-issue state.
	UpdatedChanges []ReconciledChange `json:"updated_changes,omitempty"`
}

type ReconciledChange struct {
	IssueID  string `json:"issue_id"`
	ChangeID string `json:"change_id"`
}

type HandoffSnapshot struct {
	ChangeID  string    `json:"change_id"`
	HeadSHA   string    `json:"head_sha"`
	Present   bool      `json:"present"`
	Valid     bool      `json:"valid"`
	Summary   string    `json:"summary"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ReconcileService struct {
	db  *sql.DB
	now func() time.Time
}

func NewReconcileService(database *sql.DB) *ReconcileService {
	return &ReconcileService{
		db:  database,
		now: sqlitex.UTCNow,
	}
}

// Reconcile projects the given project's exchange branches into this
// project database. The project metadata comes from the coordinator's global
// registry; this service only ever sees its own project.
func (s *ReconcileService) Reconcile(ctx context.Context, project Project) (ReconcileResult, error) {
	var result ReconcileResult
	var joinedErr error
	if strings.TrimSpace(project.ExchangePath) == "" {
		result.ProjectsSkipped++
		result.SkippedProjects = append(result.SkippedProjects, project.ID)
		return result, nil
	}
	refs, err := flowgit.ListIssueBranchRefs(ctx, project.ExchangePath)
	if err != nil {
		result.ProjectsSkipped++
		result.SkippedProjects = append(result.SkippedProjects, project.ID)
		return result, fmt.Errorf("list issue branch refs for project %s: %w", project.ID, err)
	}
	result.ProjectsScanned++
	for _, ref := range refs {
		result.BranchesScanned++
		issueID := issueIDForBranch(ref.Branch)
		if issueID == "" || !s.issueExists(ctx, issueID) {
			result.UnknownBranches = append(result.UnknownBranches, ref.Branch)
			continue
		}

		change, created, updated, err := s.ensureChangeProjection(ctx, issueID, ref.Branch, project.BaseBranch, ref.SHA)
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("ensure change projection for %s: %w", ref.Branch, err))
			continue
		}
		if created {
			result.ChangesCreated++
		} else if updated {
			result.ChangesUpdated++
		}
		if created || updated {
			result.UpdatedChanges = append(result.UpdatedChanges, ReconciledChange{IssueID: issueID, ChangeID: change.ID})
		}
	}

	return result, joinedErr
}

// Merge folds another reconcile result (typically from a sibling project)
// into this one so the API can aggregate a coordinator-wide pass.
func (r *ReconcileResult) Merge(other ReconcileResult) {
	r.ProjectsScanned += other.ProjectsScanned
	r.ProjectsSkipped += other.ProjectsSkipped
	r.BranchesScanned += other.BranchesScanned
	r.ChangesCreated += other.ChangesCreated
	r.ChangesUpdated += other.ChangesUpdated
	r.SkippedProjects = append(r.SkippedProjects, other.SkippedProjects...)
	r.UnknownBranches = append(r.UnknownBranches, other.UnknownBranches...)
	r.UpdatedChanges = append(r.UpdatedChanges, other.UpdatedChanges...)
}

func (s *ReconcileService) GetHandoffSnapshot(ctx context.Context, changeID string) (HandoffSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT change_id, head_sha, present, valid, summary, content, updated_at
FROM handoff_snapshots
WHERE change_id = ?`, strings.TrimSpace(changeID))

	return scanHandoffSnapshot(row)
}

func (s *ReconcileService) issueExists(ctx context.Context, issueID string) bool {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM issues
WHERE id = ?`, issueID).Scan(&count); err != nil {
		return false
	}

	return count == 1
}

func (s *ReconcileService) ensureChangeProjection(ctx context.Context, issueID string, branch string, base string, headSHA string) (Change, bool, bool, error) {
	existing, err := s.changeForIssueBranch(ctx, issueID, branch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Change{}, false, false, err
	}

	nowText := formatTime(s.now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		id, err := randomPrefixedID("ch")
		if err != nil {
			return Change{}, false, false, err
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO changes (
	id,
	issue_id,
	branch,
	base,
	head_sha,
	created_at,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id,
			issueID,
			branch,
			base,
			headSHA,
			nowText,
			nowText,
		); err != nil {
			return Change{}, false, false, fmt.Errorf("insert reconciled change: %w", err)
		}
		change, err := s.getChange(ctx, id)
		return change, true, false, err
	}

	if existing.HeadSHA == headSHA {
		return existing, false, false, nil
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE changes
SET head_sha = ?,
	updated_at = ?
WHERE id = ?`,
		headSHA,
		nowText,
		existing.ID,
	); err != nil {
		return Change{}, false, false, fmt.Errorf("update reconciled change: %w", err)
	}
	change, err := s.getChange(ctx, existing.ID)
	return change, false, true, err
}

func (s *ReconcileService) changeForIssueBranch(ctx context.Context, issueID string, branch string) (Change, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE issue_id = ? AND branch = ?`, issueID, branch)

	return scanChange(row)
}

func (s *ReconcileService) getChange(ctx context.Context, changeID string) (Change, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, issue_id, branch, base, head_sha, created_at, updated_at, ready_at, merged_at
FROM changes
WHERE id = ?`, changeID)

	return scanChange(row)
}

// UpsertHandoffSnapshot records (or refreshes) the handoff snapshot for a
// change. A handoff is present when its content has real (non-whitespace)
// content; present handoffs have their valid/summary derived from the handoff
// package. Invalid handoffs are still recorded (valid=false) rather than
// rejected, so the coordinator always reflects the latest submitted handoff.
// The coordinator is the sole handoff store: this path is driven entirely by the
// PUT handler (flow ready / flow handoff write), not by git reconcile.
//
// Deriving present from content (rather than from a separate presence flag) keeps
// present consistent with handoff.Validate, which also treats a whitespace-only
// body as empty: such a body records present=false, valid=false, empty summary.
func (s *ReconcileService) UpsertHandoffSnapshot(ctx context.Context, changeID string, headSHA string, content string) error {
	present := strings.TrimSpace(content) != ""
	valid := false
	summary := ""
	if present {
		valid = handoff.Validate(content) == nil
		summary = handoff.Summary(content)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO handoff_snapshots (
	change_id,
	head_sha,
	present,
	valid,
	summary,
	content,
	updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(change_id) DO UPDATE SET
	head_sha = excluded.head_sha,
	present = excluded.present,
	valid = excluded.valid,
	summary = excluded.summary,
	content = excluded.content,
	updated_at = excluded.updated_at`,
		changeID,
		headSHA,
		boolInt(present),
		boolInt(valid),
		summary,
		content,
		formatTime(s.now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("upsert handoff snapshot: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO handoff_history (
	change_id,
	head_sha,
	present,
	valid,
	summary,
	content,
	recorded_at
) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		changeID,
		headSHA,
		boolInt(present),
		boolInt(valid),
		summary,
		content,
		formatTime(s.now().UTC()),
	); err != nil {
		return fmt.Errorf("insert handoff history: %w", err)
	}

	return nil
}

func scanHandoffSnapshot(scanner issueScanner) (HandoffSnapshot, error) {
	var snapshot HandoffSnapshot
	var present int
	var valid int
	var updatedAt string
	if err := scanner.Scan(
		&snapshot.ChangeID,
		&snapshot.HeadSHA,
		&present,
		&valid,
		&snapshot.Summary,
		&snapshot.Content,
		&updatedAt,
	); err != nil {
		return HandoffSnapshot{}, fmt.Errorf("scan handoff snapshot: %w", err)
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return HandoffSnapshot{}, err
	}
	snapshot.Present = present == 1
	snapshot.Valid = valid == 1
	snapshot.UpdatedAt = parsedUpdatedAt

	return snapshot, nil
}

func issueIDForBranch(branch string) string {
	branch = strings.TrimSpace(branch)
	if !strings.HasPrefix(branch, "issue/") {
		return ""
	}

	return strings.TrimPrefix(branch, "issue/")
}
