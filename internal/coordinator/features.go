package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	"github.com/ClarifiedLabs/flow/internal/sqlitex"
)

var (
	ErrFeatureNotFound      = errors.New("feature not found")
	ErrFeatureTitleTaken    = errors.New("a feature with this title already exists")
	ErrFeatureClosed        = errors.New("feature is landed or archived")
	ErrFeatureRebaseRunning = errors.New("feature rebase already running")
	ErrNoOpenRebase         = errors.New("no running rebase for feature")
)

type FeatureStatus string

const (
	FeatureOpen     FeatureStatus = "open"
	FeatureLanded   FeatureStatus = "landed"
	FeatureArchived FeatureStatus = "archived"
)

// RebaseState is the lifecycle of one feature_rebases row. Exactly one row per
// feature may be running at a time (enforced by a partial unique index).
type RebaseState string

const (
	RebaseRunning   RebaseState = "running"
	RebaseFinalized RebaseState = "finalized"
	RebaseStale     RebaseState = "stale"
	RebaseFailed    RebaseState = "failed"
	RebaseCancelled RebaseState = "cancelled"
)

// RebaseStartKind names the outcome of a RebaseOnMain request.
type RebaseStartKind string

const (
	RebaseAlreadyUpToDate RebaseStartKind = "already_up_to_date"
	RebaseRebased         RebaseStartKind = "rebased"
	RebaseTaskCreated     RebaseStartKind = "rebase_task_created"
)

// Feature groups a set of tasks behind one long-lived feature branch. Tasks
// assigned to a feature branch off and merge back into Branch instead of the
// project base branch.
type Feature struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	Branch    string        `json:"branch"`
	Status    FeatureStatus `json:"status"`
	CreatedBy Actor         `json:"created_by"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	LandedAt  *time.Time    `json:"landed_at,omitempty"`
	LandSHA   string        `json:"land_sha,omitempty"`
}

// FeatureRebase is the durable record of one rebase of a feature branch onto
// the project base: the crash/recovery record and the finalize node's
// compare-and-swap expectation. TaskID is empty for clean instant rebases,
// which never create a system rebase task.
type FeatureRebase struct {
	ID            string      `json:"id"`
	FeatureID     string      `json:"feature_id"`
	TaskID        string      `json:"task_id,omitempty"`
	OldTipSHA     string      `json:"old_tip_sha"`
	TargetBase    string      `json:"target_base"`
	TargetBaseSHA string      `json:"target_base_sha"`
	NewTipSHA     string      `json:"new_tip_sha,omitempty"`
	State         RebaseState `json:"state"`
	CreatedAt     time.Time   `json:"created_at"`
	CompletedAt   *time.Time  `json:"completed_at,omitempty"`
}

// FeatureBranchState is the live divergence of a feature branch against the
// project base, computed on read from the local exchange.
type FeatureBranchState struct {
	BaseTipSHA    string `json:"base_tip_sha"`
	FeatureTipSHA string `json:"feature_tip_sha"`
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
}

// RebaseStartResult reports what a RebaseOnMain request did.
type RebaseStartResult struct {
	Kind         RebaseStartKind `json:"kind"`
	Feature      Feature         `json:"feature"`
	NewTipSHA    string          `json:"new_tip_sha,omitempty"`
	RebaseTaskID string          `json:"rebase_task_id,omitempty"`
}

type CreateFeatureInput struct {
	Title     string
	Body      string
	CreatedBy Actor
}

type EditFeatureInput struct {
	Title *string
	Body  *string
}

type FeatureService struct {
	db      *sql.DB
	tasks   *TaskService
	project Project
	now     func() time.Time

	// Runs schedules the system rebase task a conflicted rebase creates. It is
	// wired through the project bundle; a nil Runs leaves the task unscheduled
	// (tests schedule it explicitly).
	Runs *WorkflowRunService
}

func NewFeatureService(database *sql.DB, tasks *TaskService, project Project) *FeatureService {
	if tasks == nil {
		tasks = NewTaskService(database, project.ID)
	}
	return &FeatureService{db: database, tasks: tasks, project: project, now: sqlitex.UTCNow}
}

// Create allocates the feature id, seeds its branch from the base-branch tip,
// and stores the row. The branch is seeded with a coordinator-local update-ref
// before any other writer can know the ref name.
func (s *FeatureService) Create(ctx context.Context, input CreateFeatureInput) (Feature, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return Feature{}, errors.New("feature title is required")
	}
	actor := defaultActor(input.CreatedBy, ActorHuman)
	if err := validateActor(actor); err != nil {
		return Feature{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return Feature{}, errors.New("project exchange remote is required for feature branches")
	}
	base := s.baseBranch()
	baseTip, ok, err := flowgit.BranchTip(ctx, exchangePath, base)
	if err != nil {
		return Feature{}, fmt.Errorf("resolve base branch tip: %w", err)
	}
	if !ok {
		return Feature{}, fmt.Errorf("base branch %q not found in exchange remote", base)
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return Feature{}, err
	}
	defer tx.Rollback()

	id, err := s.allocateFeatureID(ctx, tx)
	if err != nil {
		return Feature{}, err
	}
	branch := "feature/" + id
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO features (
	id, title, body, branch, status, created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, title, strings.TrimSpace(input.Body), branch,
		string(FeatureOpen), string(actor), formatTime(now), formatTime(now)); err != nil {
		if strings.Contains(err.Error(), "features.title_norm") {
			return Feature{}, ErrFeatureTitleTaken
		}
		return Feature{}, fmt.Errorf("insert feature: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Feature{}, fmt.Errorf("commit feature: %w", err)
	}

	if err := flowgit.UpdateRef(ctx, exchangePath, "refs/heads/"+branch, baseTip); err != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM features WHERE id = ?`, id)
		return Feature{}, fmt.Errorf("seed feature branch: %w", err)
	}

	return s.Get(ctx, id)
}

func (s *FeatureService) Get(ctx context.Context, id string) (Feature, error) {
	return s.getOne(ctx, featureSelect+`
FROM features
WHERE id = ?`, strings.TrimSpace(id))
}

// Resolve loads a feature by id, falling back to an exact title match so CLI
// and web callers can use either.
func (s *FeatureService) Resolve(ctx context.Context, ref string) (Feature, error) {
	feature, err := s.Get(ctx, ref)
	if err == nil {
		return feature, nil
	}
	if !errors.Is(err, ErrFeatureNotFound) {
		return Feature{}, err
	}
	return s.getOne(ctx, featureSelect+`
FROM features
WHERE title = ?`, strings.TrimSpace(ref))
}

func (s *FeatureService) getOne(ctx context.Context, query string, arg any) (Feature, error) {
	feature, err := scanFeature(s.db.QueryRowContext(ctx, query, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Feature{}, ErrFeatureNotFound
	}
	if err != nil {
		return Feature{}, err
	}
	return feature, nil
}

const featureSelect = `
SELECT
	id,
	title,
	body,
	branch,
	status,
	created_by,
	created_at,
	updated_at,
	landed_at,
	land_sha`

// List returns features ordered by title. A non-empty status filters in SQL.
func (s *FeatureService) List(ctx context.Context, status FeatureStatus) ([]Feature, error) {
	query := featureSelect + `
FROM features`
	var args []any
	switch status {
	case "":
		// No filter.
	case FeatureOpen, FeatureLanded, FeatureArchived:
		query += `
WHERE status = ?`
		args = append(args, string(status))
	default:
		return nil, fmt.Errorf("invalid feature status: %s", status)
	}
	query += `
ORDER BY title`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	defer rows.Close()

	var features []Feature
	for rows.Next() {
		feature, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		features = append(features, feature)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate features: %w", err)
	}

	return features, nil
}

// Edit updates mutable feature metadata. Status transitions go through the
// dedicated Land/Archive methods; the branch name is immutable because task
// branches and changes reference it.
func (s *FeatureService) Edit(ctx context.Context, id string, input EditFeatureInput) (Feature, error) {
	feature, err := s.Get(ctx, id)
	if err != nil {
		return Feature{}, err
	}
	if input.Title == nil && input.Body == nil {
		return feature, nil
	}
	title := feature.Title
	if input.Title != nil {
		title = strings.TrimSpace(*input.Title)
		if title == "" {
			return Feature{}, errors.New("feature title is required")
		}
	}
	body := feature.Body
	if input.Body != nil {
		body = *input.Body
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE features
SET title = ?, body = ?, updated_at = ?
WHERE id = ?`, title, body, formatTime(s.now().UTC()), feature.ID); err != nil {
		if strings.Contains(err.Error(), "features.title_norm") {
			return Feature{}, ErrFeatureTitleTaken
		}
		return Feature{}, fmt.Errorf("update feature: %w", err)
	}
	return s.Get(ctx, feature.ID)
}

// ErrFeatureActive reports a land/archive request against a feature that
// still has active work: non-done tasks or a running rebase. Offenders lists
// the blocking task ids so the caller can name them.
type ErrFeatureActive struct {
	Offenders []string
}

func (e *ErrFeatureActive) Error() string {
	return fmt.Sprintf("feature has active work: %s", strings.Join(e.Offenders, ", "))
}

// Land squash-merges the feature branch into the project base and stamps the
// feature landed. Landing requires an idle feature: every assigned task done
// and no rebase running. A squash that stages nothing is the heal path: the
// feature's content is already in the base (an empty feature, or a crashed
// earlier land whose push landed), so the feature is stamped landed at the
// current base tip. Replays return the landed feature unchanged.
func (s *FeatureService) Land(ctx context.Context, featureRef string, actor Actor) (Feature, error) {
	actor = defaultActor(actor, ActorHuman)
	if err := validateActor(actor); err != nil {
		return Feature{}, err
	}
	feature, err := s.Resolve(ctx, featureRef)
	if err != nil {
		return Feature{}, err
	}
	if feature.Status == FeatureLanded {
		return feature, nil
	}
	if feature.Status != FeatureOpen {
		return Feature{}, ErrFeatureClosed
	}
	if err := s.requireIdle(ctx, feature.ID); err != nil {
		return Feature{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return Feature{}, errors.New("project exchange remote is required for feature branches")
	}
	base := s.baseBranch()
	tip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return Feature{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return Feature{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}

	result, err := flowgit.SquashMergeToBase(ctx, flowgit.SquashMergeInput{
		ExchangeRepoPath: exchangePath,
		BaseBranch:       base,
		Branch:           feature.Branch,
		ExpectedHeadSHA:  tip,
		Message:          fmt.Sprintf("%s: %s", feature.ID, feature.Title),
	})
	landSHA := ""
	switch {
	case err == nil:
		landSHA = result.MergeSHA
	case errors.Is(err, flowgit.ErrNoMergeChanges):
		baseTip, found, tipErr := flowgit.BranchTip(ctx, exchangePath, base)
		if tipErr != nil {
			return Feature{}, fmt.Errorf("resolve base branch tip: %w", tipErr)
		}
		if !found {
			return Feature{}, fmt.Errorf("base branch %q not found in exchange remote", base)
		}
		landSHA = baseTip
	default:
		var conflict *flowgit.MergeConflictError
		if errors.As(err, &conflict) {
			return Feature{}, fmt.Errorf("feature branch conflicts with %s; rebase it first: %w", base, err)
		}
		return Feature{}, fmt.Errorf("land feature branch: %w", err)
	}

	now := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE features
SET status = ?, landed_at = ?, land_sha = ?, updated_at = ?
WHERE id = ? AND status = 'open'`,
		string(FeatureLanded), now, landSHA, now, feature.ID); err != nil {
		return Feature{}, fmt.Errorf("stamp feature landed: %w", err)
	}
	return s.Get(ctx, feature.ID)
}

// Archive closes a feature without landing it. Archive is the only delete:
// the branch is retained in the exchange for audit. It requires the same idle
// precondition as Land and replays as a no-op.
func (s *FeatureService) Archive(ctx context.Context, featureRef string) (Feature, error) {
	feature, err := s.Resolve(ctx, featureRef)
	if err != nil {
		return Feature{}, err
	}
	if feature.Status == FeatureArchived {
		return feature, nil
	}
	if err := s.requireIdle(ctx, feature.ID); err != nil {
		return Feature{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE features SET status = ?, updated_at = ? WHERE id = ? AND status != ?`,
		string(FeatureArchived), formatTime(s.now().UTC()), feature.ID, string(FeatureArchived)); err != nil {
		return Feature{}, fmt.Errorf("archive feature: %w", err)
	}
	return s.Get(ctx, feature.ID)
}

// Tasks returns the feature's assigned tasks in creation order.
func (s *FeatureService) Tasks(ctx context.Context, featureID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT"+taskSelectColumns+`
FROM tasks i
WHERE i.feature_id = ?
ORDER BY i.created_at, i.id`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, fmt.Errorf("list feature tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature tasks: %w", err)
	}

	return tasks, nil
}

// requireIdle enforces the land/archive precondition: no running rebase and
// no non-done tasks. A rebase task is itself a non-done feature task, so it
// appears among the offenders naturally; a taskless running row (the clean
// path's crash window) is named symbolically.
func (s *FeatureService) requireIdle(ctx context.Context, featureID string) error {
	offenders, err := s.nonDoneFeatureTaskIDs(ctx, featureID)
	if err != nil {
		return err
	}
	if running, found, err := s.RunningRebase(ctx, featureID); err != nil {
		return err
	} else if found && running.TaskID == "" {
		offenders = append([]string{"rebase in progress"}, offenders...)
	}
	if len(offenders) > 0 {
		return &ErrFeatureActive{Offenders: offenders}
	}
	return nil
}

// BranchState computes the feature branch's live divergence from the project
// base: ahead/behind commit counts plus both tips, read from the local
// exchange so the feature page can show "N ahead / M behind" on every load.
func (s *FeatureService) BranchState(ctx context.Context, featureID string) (FeatureBranchState, error) {
	feature, err := s.Get(ctx, featureID)
	if err != nil {
		return FeatureBranchState{}, err
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return FeatureBranchState{}, errors.New("project exchange remote is required for feature branches")
	}
	base := s.baseBranch()
	baseTip, ok, err := flowgit.BranchTip(ctx, exchangePath, base)
	if err != nil {
		return FeatureBranchState{}, fmt.Errorf("resolve base branch tip: %w", err)
	}
	if !ok {
		return FeatureBranchState{}, fmt.Errorf("base branch %q not found in exchange remote", base)
	}
	featureTip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return FeatureBranchState{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return FeatureBranchState{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	ahead, behind, err := flowgit.RefDivergence(ctx, exchangePath, base, feature.Branch)
	if err != nil {
		return FeatureBranchState{}, err
	}

	return FeatureBranchState{
		BaseTipSHA:    baseTip,
		FeatureTipSHA: featureTip,
		Ahead:         ahead,
		Behind:        behind,
	}, nil
}

// RunningRebase returns the feature's open feature_rebases row, if any. Both
// the schedule-time block linking and the finalize node resolve through here.
func (s *FeatureService) RunningRebase(ctx context.Context, featureID string) (FeatureRebase, bool, error) {
	rebase, err := scanFeatureRebase(s.db.QueryRowContext(ctx, `
SELECT
	id,
	feature_id,
	COALESCE(task_id, ''),
	old_tip_sha,
	target_base,
	target_base_sha,
	new_tip_sha,
	state,
	created_at,
	completed_at
FROM feature_rebases
WHERE feature_id = ? AND state = 'running'`, strings.TrimSpace(featureID)))
	if errors.Is(err, sql.ErrNoRows) {
		return FeatureRebase{}, false, nil
	}
	if err != nil {
		return FeatureRebase{}, false, fmt.Errorf("load running rebase: %w", err)
	}
	return rebase, true, nil
}

// ListRebases returns the feature's rebase history, newest first.
func (s *FeatureService) ListRebases(ctx context.Context, featureID string) ([]FeatureRebase, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT
	id,
	feature_id,
	COALESCE(task_id, ''),
	old_tip_sha,
	target_base,
	target_base_sha,
	new_tip_sha,
	state,
	created_at,
	completed_at
FROM feature_rebases
WHERE feature_id = ?
ORDER BY created_at DESC, id DESC`, strings.TrimSpace(featureID))
	if err != nil {
		return nil, fmt.Errorf("list feature rebases: %w", err)
	}
	defer rows.Close()

	var rebases []FeatureRebase
	for rows.Next() {
		rebase, err := scanFeatureRebase(rows)
		if err != nil {
			return nil, err
		}
		rebases = append(rebases, rebase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature rebases: %w", err)
	}

	return rebases, nil
}

// RebaseOnMain rebases the feature branch onto the project base tip. A branch
// that is not behind the base is already up to date. A clean rebase is
// published immediately and the row stamped finalized. A conflicted rebase
// keeps the running row, creates a system rebase task on the feature-rebase
// flow, links rebase_task blocks every other non-done feature task, and
// schedules it — the trusted finalize node publishes the result.
//
// restrictBlockedTo confines the conflicted path's blocker links to the named
// tasks: when non-empty, only those tasks (if still non-done) receive a
// rebase_task blocks relation. Task-bound console credentials pass exactly
// their bound task, so the blocker set is confined at relation-creation time
// rather than by a racy pre-read: a feature task created concurrently after
// any API-side scope check can never be linked.
func (s *FeatureService) RebaseOnMain(ctx context.Context, featureRef string, restrictBlockedTo ...string) (RebaseStartResult, error) {
	feature, err := s.Resolve(ctx, featureRef)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if feature.Status != FeatureOpen {
		return RebaseStartResult{}, ErrFeatureClosed
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return RebaseStartResult{}, errors.New("project exchange remote is required for feature branches")
	}

	if running, found, err := s.RunningRebase(ctx, feature.ID); err != nil {
		return RebaseStartResult{}, err
	} else if found {
		// Crash between the clean path's push and its stamp: the ref already
		// moved, so heal the row to finalized instead of refusing forever.
		tip, ok, tipErr := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
		if tipErr == nil && ok && tip != running.OldTipSHA {
			if err := s.stampRebaseDone(ctx, running.ID, RebaseFinalized, tip); err != nil {
				return RebaseStartResult{}, err
			}
			return RebaseStartResult{Kind: RebaseRebased, Feature: feature, NewTipSHA: tip}, nil
		}
		return RebaseStartResult{}, ErrFeatureRebaseRunning
	}

	base := s.baseBranch()
	baseTip, ok, err := flowgit.BranchTip(ctx, exchangePath, base)
	if err != nil {
		return RebaseStartResult{}, fmt.Errorf("resolve base branch tip: %w", err)
	}
	if !ok {
		return RebaseStartResult{}, fmt.Errorf("base branch %q not found in exchange remote", base)
	}
	featureTip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return RebaseStartResult{}, fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return RebaseStartResult{}, fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	_, behind, err := flowgit.RefDivergence(ctx, exchangePath, base, feature.Branch)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if behind == 0 {
		return RebaseStartResult{Kind: RebaseAlreadyUpToDate, Feature: feature, NewTipSHA: featureTip}, nil
	}

	// The durable record comes first: if the process crashes mid-rebase the
	// next request reconciles the row against the exchange ref.
	rebaseID, err := s.insertRebaseRow(ctx, feature.ID, "", featureTip, base, baseTip)
	if err != nil {
		return RebaseStartResult{}, err
	}

	result, err := flowgit.RebaseOnto(ctx, flowgit.RebaseOntoInput{
		ExchangePath:   exchangePath,
		Branch:         feature.Branch,
		Onto:           base,
		ExpectedOldSHA: featureTip,
	})
	if err == nil {
		if err := s.stampRebaseDone(ctx, rebaseID, RebaseFinalized, result.HeadSHA); err != nil {
			return RebaseStartResult{}, err
		}
		return RebaseStartResult{Kind: RebaseRebased, Feature: feature, NewTipSHA: result.HeadSHA}, nil
	}
	var conflict *flowgit.RebaseConflictError
	if !errors.As(err, &conflict) {
		// Operational failure: nothing was pushed, so the running row would
		// block future rebases forever. Fail it and surface the error.
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, fmt.Errorf("rebase feature branch: %w", err)
	}

	flowID, err := s.flowIDByName(ctx)
	if err != nil {
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, err
	}
	body := fmt.Sprintf(
		"Rebase the feature branch `%s` onto `%s` (base tip `%s`).\n\n"+
			"The automatic rebase stopped on conflicts. Resolve them preserving the feature's content exactly, "+
			"then complete this task's change; the rebase is published to the feature branch after checks, verification, and human approval.\n\n"+
			"git reported:\n```\n%s\n```",
		feature.Branch, base, baseTip, strings.TrimSpace(conflict.Output))
	task, err := s.tasks.CreateTask(ctx, CreateTaskInput{
		Title:     fmt.Sprintf("Rebase %s onto %s", feature.Title, base),
		Body:      body,
		FeatureID: &feature.ID,
		FlowID:    flowID,
		CreatedBy: ActorSystem,
	})
	if err != nil {
		_ = s.stampRebaseDone(ctx, rebaseID, RebaseFailed, "")
		return RebaseStartResult{}, fmt.Errorf("create rebase task: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE feature_rebases SET task_id = ? WHERE id = ?`, task.ID, rebaseID); err != nil {
		return RebaseStartResult{}, fmt.Errorf("attach rebase task: %w", err)
	}

	blocked, err := s.nonDoneFeatureTaskIDs(ctx, feature.ID)
	if err != nil {
		return RebaseStartResult{}, err
	}
	if err := s.tasks.BlockOnRebase(ctx, task.ID, restrictBlockedTasks(blocked, restrictBlockedTo)); err != nil {
		return RebaseStartResult{}, fmt.Errorf("block feature tasks on rebase: %w", err)
	}

	if s.Runs != nil {
		if _, err := s.Runs.ScheduleAs(ctx, task.ID, ActorSystem); err != nil {
			return RebaseStartResult{}, fmt.Errorf("schedule rebase task: %w", err)
		}
	}

	return RebaseStartResult{Kind: RebaseTaskCreated, Feature: feature, RebaseTaskID: task.ID}, nil
}

// flowIDByName resolves the seeded feature-rebase flow.
func (s *FeatureService) flowIDByName(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM flows WHERE name = ?`, FeatureRebaseFlowName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s flow is not seeded for this project", FeatureRebaseFlowName)
	}
	if err != nil {
		return "", fmt.Errorf("look up %s flow: %w", FeatureRebaseFlowName, err)
	}
	return id, nil
}

// FinalizeRebase publishes the rebased head recorded on the rebase task's
// change to the feature ref, guarded by a compare-and-swap on the tip the
// running feature_rebases row recorded. It returns the finalize node's
// outcome: "finalized" when the ref now points at the change head, "stale"
// when the feature branch moved mid-rebase and the rebase must redo itself
// (the flow loops back to the rebase agent with a fresh running row).
func (s *FeatureService) FinalizeRebase(ctx context.Context, featureID, taskID string, change Change) (string, error) {
	feature, err := s.Get(ctx, featureID)
	if err != nil {
		return "", err
	}
	rebase, found, err := s.RunningRebase(ctx, feature.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNoOpenRebase
	}
	if rebase.TaskID != strings.TrimSpace(taskID) {
		return "", fmt.Errorf("running rebase belongs to task %s, not %s", rebase.TaskID, taskID)
	}
	head := strings.TrimSpace(change.HeadSHA)
	if head == "" {
		return "", errors.New("rebase task change has no head sha")
	}
	if head == rebase.OldTipSHA {
		return "", errors.New("rebase task change head matches the pre-rebase tip")
	}
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return "", errors.New("project exchange remote is required for feature branches")
	}

	tip, ok, err := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve feature branch tip: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("feature branch %q not found in exchange remote", feature.Branch)
	}
	if tip == head {
		// Heal: the push landed but the stamp crashed (merge-intent-style
		// replay). The ref already points at the new head, so finalizing is a
		// no-op beyond the row.
		if err := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); err != nil {
			return "", err
		}
		return "finalized", nil
	}
	if tip != rebase.OldTipSHA {
		if err := s.markRebaseStale(ctx, rebase, tip); err != nil {
			return "", err
		}
		return "stale", nil
	}

	ref := "refs/heads/" + feature.Branch
	if err := flowgit.PushRefCompareAndSwap(ctx, exchangePath, ref, head, rebase.OldTipSHA); err != nil {
		// A lost lease means a concurrent update (a task merged mid-rebase).
		// Re-probe and take the stale path when the tip really moved.
		current, ok, probeErr := flowgit.BranchTip(ctx, exchangePath, feature.Branch)
		if probeErr == nil && ok && current != rebase.OldTipSHA && current != head {
			if staleErr := s.markRebaseStale(ctx, rebase, current); staleErr != nil {
				return "", staleErr
			}
			return "stale", nil
		}
		if probeErr == nil && ok && current == head {
			if stampErr := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); stampErr != nil {
				return "", stampErr
			}
			return "finalized", nil
		}
		return "", fmt.Errorf("push rebased head to feature branch: %w", err)
	}

	if err := s.stampRebaseDone(ctx, rebase.ID, RebaseFinalized, head); err != nil {
		return "", err
	}
	return "finalized", nil
}

// markRebaseStale records that the feature branch moved out from under a
// running rebase and opens the redo row against the current tip, keeping the
// finalize node's compare-and-swap expectation truthful for the next visit.
func (s *FeatureService) markRebaseStale(ctx context.Context, rebase FeatureRebase, currentTip string) error {
	baseTip, ok, err := flowgit.BranchTip(ctx, strings.TrimSpace(s.project.ExchangePath), rebase.TargetBase)
	if err != nil {
		return fmt.Errorf("resolve base branch tip: %w", err)
	}
	if !ok {
		return fmt.Errorf("base branch %q not found in exchange remote", rebase.TargetBase)
	}

	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(s.now().UTC())
	if _, err := tx.ExecContext(ctx, `
UPDATE feature_rebases SET state = ?, completed_at = ? WHERE id = ? AND state = 'running'`,
		string(RebaseStale), now, rebase.ID); err != nil {
		return fmt.Errorf("mark rebase stale: %w", err)
	}
	id, err := s.allocateRebaseID(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feature_rebases (
	id, feature_id, task_id, old_tip_sha, target_base, target_base_sha,
	new_tip_sha, state, created_at
) VALUES (?, ?, ?, ?, ?, ?, '', 'running', ?)`,
		id, rebase.FeatureID, rebase.TaskID, currentTip, rebase.TargetBase, baseTip, now); err != nil {
		return fmt.Errorf("open redo rebase row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit stale rebase: %w", err)
	}

	return nil
}

// EnsureRebaseBlock links a running rebase's task as a blocker of taskID when
// taskID belongs to the rebased feature. WorkflowRunService calls it before
// scheduling a task so tasks created or reopened mid-rebase are held at the
// dependency gate like the rest of the feature.
func (s *FeatureService) EnsureRebaseBlock(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	var rebaseTaskID string
	err := s.db.QueryRowContext(ctx, `
SELECT fr.task_id
FROM feature_rebases fr
JOIN tasks t ON t.feature_id = fr.feature_id
WHERE t.id = ? AND fr.state = 'running' AND fr.task_id IS NOT NULL AND fr.task_id != ?`,
		taskID, taskID).Scan(&rebaseTaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load running rebase for task: %w", err)
	}

	return s.tasks.BlockOnRebase(ctx, rebaseTaskID, []string{taskID})
}

// restrictBlockedTasks filters ids down to the allowed membership set,
// preserving order. An empty allowed set leaves ids untouched, keeping the
// default rebase behavior of holding every non-done feature task.
func restrictBlockedTasks(ids, allowed []string) []string {
	if len(allowed) == 0 {
		return ids
	}
	set := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		set[strings.TrimSpace(id)] = struct{}{}
	}
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := set[strings.TrimSpace(id)]; ok {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

// nonDoneFeatureTaskIDs lists the feature's tasks that a running rebase must
// hold: everything not done. Done blockers are resolved by definition, so
// they neither need nor get a link.
func (s *FeatureService) nonDoneFeatureTaskIDs(ctx context.Context, featureID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM tasks WHERE feature_id = ? AND (lifecycle_state IS NULL OR lifecycle_state != 'done')`, featureID)
	if err != nil {
		return nil, fmt.Errorf("list feature tasks: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate feature tasks: %w", err)
	}

	return ids, nil
}

func (s *FeatureService) insertRebaseRow(ctx context.Context, featureID, taskID, oldTip, targetBase, targetBaseSHA string) (string, error) {
	tx, err := sqlitex.BeginImmediate(ctx, s.db)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := s.allocateRebaseID(ctx, tx)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO feature_rebases (
	id, feature_id, task_id, old_tip_sha, target_base, target_base_sha,
	new_tip_sha, state, created_at
) VALUES (?, ?, ?, ?, ?, ?, '', 'running', ?)`,
		id, featureID, sqlitex.NullableNonEmptyString(taskID), oldTip, targetBase, targetBaseSHA,
		formatTime(s.now().UTC())); err != nil {
		return "", fmt.Errorf("insert rebase row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit rebase row: %w", err)
	}

	return id, nil
}

// stampRebaseDone closes a rebase row. newTipSHA is recorded for finalized
// rows and left empty otherwise.
func (s *FeatureService) stampRebaseDone(ctx context.Context, rebaseID string, state RebaseState, newTipSHA string) error {
	now := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx, `
UPDATE feature_rebases
SET state = ?, new_tip_sha = ?, completed_at = ?
WHERE id = ? AND state = 'running'`,
		string(state), newTipSHA, now, rebaseID); err != nil {
		return fmt.Errorf("stamp rebase %s: %w", state, err)
	}

	return nil
}

func (s *FeatureService) baseBranch() string {
	base := strings.TrimSpace(s.project.BaseBranch)
	if base == "" {
		base = "main"
	}
	return base
}

// allocateFeatureID mirrors allocateTaskID: per-project, gapless under the
// write transaction, formatted f-<project-key>-NNNN.
func (s *FeatureService) allocateFeatureID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	return s.allocateID(ctx, tx, "feature", "f")
}

// allocateRebaseID formats rb-<project-key>-NNNN from its own allocator row.
func (s *FeatureService) allocateRebaseID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	return s.allocateID(ctx, tx, "feature_rebase", "rb")
}

func (s *FeatureService) allocateID(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, allocator, prefix string) (string, error) {
	var nextNumber int64
	if err := tx.QueryRowContext(ctx, `
UPDATE id_allocators
SET next_number = next_number + 1
WHERE name = ?
RETURNING next_number - 1`, allocator).Scan(&nextNumber); err != nil {
		return "", fmt.Errorf("allocate %s id: %w", allocator, err)
	}

	key, err := projectKeyFromID(s.project.ID)
	if err != nil {
		return "", fmt.Errorf("format %s id: %w", allocator, err)
	}
	return fmt.Sprintf("%s-%s-%04d", prefix, key, nextNumber), nil
}

type featureScanner interface {
	Scan(dest ...any) error
}

func scanFeature(row featureScanner) (Feature, error) {
	var feature Feature
	var status, createdBy string
	var createdAt, updatedAt string
	var landedAt sql.NullString
	err := row.Scan(
		&feature.ID,
		&feature.Title,
		&feature.Body,
		&feature.Branch,
		&status,
		&createdBy,
		&createdAt,
		&updatedAt,
		&landedAt,
		&feature.LandSHA,
	)
	if err != nil {
		return Feature{}, err
	}
	feature.Status = FeatureStatus(status)
	feature.CreatedBy = Actor(createdBy)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return Feature{}, fmt.Errorf("parse feature created_at: %w", err)
	}
	parsedUpdatedAt, err := parseTime(updatedAt)
	if err != nil {
		return Feature{}, fmt.Errorf("parse feature updated_at: %w", err)
	}
	feature.CreatedAt = parsedCreatedAt
	feature.UpdatedAt = parsedUpdatedAt
	if landedAt.Valid {
		parsed, err := parseTime(landedAt.String)
		if err != nil {
			return Feature{}, fmt.Errorf("parse feature landed_at: %w", err)
		}
		feature.LandedAt = &parsed
	}
	return feature, nil
}

func scanFeatureRebase(row featureScanner) (FeatureRebase, error) {
	var rebase FeatureRebase
	var state string
	var createdAt string
	var completedAt sql.NullString
	err := row.Scan(
		&rebase.ID,
		&rebase.FeatureID,
		&rebase.TaskID,
		&rebase.OldTipSHA,
		&rebase.TargetBase,
		&rebase.TargetBaseSHA,
		&rebase.NewTipSHA,
		&state,
		&createdAt,
		&completedAt,
	)
	if err != nil {
		return FeatureRebase{}, err
	}
	rebase.State = RebaseState(state)
	parsedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return FeatureRebase{}, fmt.Errorf("parse rebase created_at: %w", err)
	}
	rebase.CreatedAt = parsedCreatedAt
	if completedAt.Valid {
		parsed, err := parseTime(completedAt.String)
		if err != nil {
			return FeatureRebase{}, fmt.Errorf("parse rebase completed_at: %w", err)
		}
		rebase.CompletedAt = &parsed
	}
	return rebase, nil
}
