package coordinator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	flowgit "github.com/ClarifiedLabs/flow/internal/git"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
	"gopkg.in/yaml.v3"
)

const checkConfigPrefix = ".flow/checks"

const (
	defaultReviewerCheckName = "reviewer"
	defaultVerifierCheckName = "verifier"
)

// CompletionAssessmentCheckMarker is stamped into the Details of a reviewer
// check enqueued to recover a crashed author session that never ran flow ready
// (Mode-B recovery). The recovering reviewer fetches its own check while
// building its prompt and renders completion-assessment guidance when it sees
// this marker, so the carrier never needs a new env var or schema column. The
// reviewer overwrites Details with its real verdict details once it reports.
const CompletionAssessmentCheckMarker = "completion-assessment: prior author session exited without finalizing"

type CheckPhase string

const (
	CheckPhaseCritique   CheckPhase = "critique"
	CheckPhaseAcceptance CheckPhase = "acceptance"
)

type CheckEntrypoint struct {
	Argv  []string          `json:"argv" yaml:"argv"`
	CWD   string            `json:"cwd" yaml:"cwd"`
	Env   map[string]string `json:"env" yaml:"env"`
	Shell bool              `json:"shell" yaml:"shell"`
}

type CheckDefinition struct {
	Name        string                 `json:"name" yaml:"name"`
	Kind        CheckKind              `json:"kind" yaml:"kind"`
	Phase       CheckPhase             `json:"phase" yaml:"phase"`
	Required    *bool                  `json:"required" yaml:"required"`
	Entrypoint  *CheckEntrypoint       `json:"entrypoint" yaml:"entrypoint"`
	RunsOn      map[string]string      `json:"runs_on" yaml:"runs_on"`
	Requires    []string               `json:"requires" yaml:"requires"`
	Size        string                 `json:"size" yaml:"size"`
	Tolerations []scheduler.Toleration `json:"tolerations" yaml:"tolerations"`
	sourcePath  string
}

type CheckSuite struct {
	Definitions []CheckDefinition `json:"definitions"`
	Configured  bool              `json:"configured"`
}

type ScheduleReviewRoundInput struct {
	Issue           Issue
	Change          Change
	PreviousHeadSHA string
	// CompletionAssessment requests a Mode-B recovery review: the reviewer
	// critique check is stamped with CompletionAssessmentCheckMarker so the
	// recovering reviewer assesses whether the crashed author's work is complete
	// instead of running an ordinary critique. Routing of the verdict is
	// unchanged (satisfied → verification, blocked → author fix round).
	CompletionAssessment bool
}

type ScheduleReviewRoundResult struct {
	ChecksCreated int `json:"checks_created"`
	JobsEnqueued  int `json:"jobs_enqueued"`
	// EnqueuedCheckNames lists the checks for which a job was actually
	// enqueued this round. The lifecycle engine arms a check-timeout timer per
	// name so a job that never reports cannot park the issue indefinitely.
	EnqueuedCheckNames []string `json:"enqueued_check_names,omitempty"`
}

type checkRecoveryCandidate struct {
	change Change
}

// PendingCheckTimeout identifies the pending automated checks on a ready change
// for which the recovery scan expects a worker job to report. The lifecycle
// engine arms a check timeout per name (keyed by head) so a review round that
// was scheduled OUTSIDE the engine — e.g. a Mode-B completion-assessment review
// dispatched directly by the coordinator's crash reconcile, which cannot reach
// the engine's timeout arming — still times out like a normal review round
// instead of parking the change indefinitely when its reviewer never reports.
type PendingCheckTimeout struct {
	IssueID    string
	HeadSHA    string
	CheckNames []string
}

type CheckConfigService struct {
	db          *sql.DB
	issues      *IssueService
	checks      *CheckService
	workers     *flowworker.Service
	threads     *ThreadService
	project     Project
	harnessArgs flowharness.Args
}

type CheckConfigServiceOptions struct {
	HarnessArgs flowharness.Args
}

func NewCheckConfigServiceWithOptions(database *sql.DB, checks *CheckService, workers *flowworker.Service, threads *ThreadService, project Project, opts CheckConfigServiceOptions) *CheckConfigService {
	if checks == nil {
		checks = NewCheckService(database)
	}
	if workers == nil {
		workers = flowworker.NewService(database)
	}
	harnessArgs, err := flowharness.NormalizeArgs(opts.HarnessArgs)
	if err != nil {
		panic(fmt.Sprintf("normalize harness args: %v", err))
	}
	return &CheckConfigService{
		db:          database,
		issues:      NewIssueService(database),
		checks:      checks,
		workers:     workers,
		threads:     threads,
		project:     project,
		harnessArgs: harnessArgs,
	}
}

func (s *CheckConfigService) LoadSuiteForChange(ctx context.Context, change Change) (CheckSuite, error) {
	head := strings.TrimSpace(change.HeadSHA)
	if head == "" {
		return CheckSuite{}, nil
	}
	exchangePath, ok, err := s.exchangePathForChange(ctx, change)
	if err != nil {
		return CheckSuite{}, err
	}
	if !ok {
		return CheckSuite{}, nil
	}

	files, err := flowgit.ListTextFilesAtRef(ctx, exchangePath, head, checkConfigPrefix)
	if err != nil {
		return CheckSuite{}, err
	}
	var suite CheckSuite
	seen := map[string]string{}
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file.Path))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		suite.Configured = true
		definition, err := parseCheckDefinition(file.Path, file.Content)
		if err != nil {
			return CheckSuite{}, err
		}
		if previous := seen[definition.Name]; previous != "" {
			return CheckSuite{}, fmt.Errorf("duplicate check name %q in %s and %s", definition.Name, previous, file.Path)
		}
		seen[definition.Name] = file.Path
		suite.Definitions = append(suite.Definitions, definition)
	}

	return suite, nil
}

func (s *CheckConfigService) ScheduleReviewRound(ctx context.Context, input ScheduleReviewRoundInput) (ScheduleReviewRoundResult, error) {
	suite, err := s.LoadSuiteForChange(ctx, input.Change)
	if err != nil {
		return ScheduleReviewRoundResult{}, err
	}
	if strings.TrimSpace(input.Change.HeadSHA) != "" {
		suite, err = withDefaultAgentChecks(suite, input.Issue.AgentHarness, s.harnessArgs.Add(input.Issue.HarnessArgs))
		if err != nil {
			return ScheduleReviewRoundResult{}, err
		}
	}
	var result ScheduleReviewRoundResult
	if err := s.retireRemovedConfiguredChecks(ctx, input, suite); err != nil {
		return ScheduleReviewRoundResult{}, err
	}
	for _, definition := range suite.Definitions {
		details := ""
		if input.CompletionAssessment && definition.Kind == CheckKindReviewer {
			details = CompletionAssessmentCheckMarker
		}
		if err := s.ensurePendingCheckWithDetails(ctx, input.Issue.ID, definition, details); err != nil {
			return ScheduleReviewRoundResult{}, err
		}
		result.ChecksCreated++
		if definition.Phase == CheckPhaseCritique && definition.Kind != CheckKindHuman {
			enqueued, err := s.enqueueCheckJob(ctx, input.Issue.ID, input.Change, definition)
			if err != nil {
				return ScheduleReviewRoundResult{}, err
			}
			if enqueued {
				result.JobsEnqueued++
				result.EnqueuedCheckNames = append(result.EnqueuedCheckNames, definition.Name)
			}
		}
	}
	if input.Issue.RequiresHumanReview {
		created, err := s.ensureHumanReviewCheck(ctx, input.Issue.ID, input.Change, input.PreviousHeadSHA)
		if err != nil {
			return ScheduleReviewRoundResult{}, err
		}
		if created {
			result.ChecksCreated++
		}
	}
	acceptanceNames, err := s.EnqueueAcceptanceIfReady(ctx, input.Issue.ID, input.Change)
	if err != nil {
		return ScheduleReviewRoundResult{}, err
	}
	result.JobsEnqueued += len(acceptanceNames)
	result.EnqueuedCheckNames = append(result.EnqueuedCheckNames, acceptanceNames...)

	return result, nil
}

// AcceptancePending reports whether the issue sits in the acceptance gate: every
// required critique-kind check is satisfied AND at least one verifier-kind check
// has not yet been satisfied. It delegates to the package-level predicate that
// the coordinator's DerivePhase also uses, so the acceptance-job enqueue
// (EnqueueAcceptanceIfReady), the lifecycle engine's phase derivation, and the
// coordinator phase mirror never disagree about what "acceptance" means.
func (s *CheckConfigService) AcceptancePending(ctx context.Context, issueID string) (bool, error) {
	return acceptancePendingForIssue(ctx, s.db, issueID)
}

// EnqueueAcceptanceIfReady enqueues acceptance-phase check jobs once the
// critique gate is satisfied. It returns the names of the checks for which a
// job was actually enqueued (empty when the gate is not yet met or every
// acceptance check already has a live job) so the lifecycle engine can arm a
// timeout per scheduled check.
func (s *CheckConfigService) EnqueueAcceptanceIfReady(ctx context.Context, issueID string, change Change) ([]string, error) {
	ready, err := s.checks.CritiqueSatisfied(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, nil
	}
	suite, err := s.LoadSuiteForChange(ctx, change)
	if err != nil {
		return nil, err
	}
	issue, err := s.issues.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(change.HeadSHA) != "" {
		suite, err = withDefaultAgentChecks(suite, issue.AgentHarness, s.harnessArgs.Add(issue.HarnessArgs))
		if err != nil {
			return nil, err
		}
	}
	var enqueued []string
	for _, definition := range suite.Definitions {
		if definition.Phase != CheckPhaseAcceptance || definition.Kind == CheckKindHuman {
			continue
		}
		check, err := s.ensureCheckExists(ctx, issueID, definition)
		if err != nil {
			return nil, err
		}
		if check.Verdict != CheckPending {
			continue
		}
		ok, err := s.enqueueCheckJob(ctx, issueID, change, definition)
		if err != nil {
			return nil, err
		}
		if ok {
			enqueued = append(enqueued, definition.Name)
		}
	}

	return enqueued, nil
}

// RecoverPendingCheckJobs re-enqueues missing automated check jobs for pending
// checks on ready-unmerged changes and returns, alongside the enqueued count,
// the pending checks for which the recovery expects a job to report so the
// lifecycle engine can arm a check timeout per name. The pending list covers a
// check whether or not a job was enqueued this tick (a live job that never
// reports needs the timeout just as much as a freshly enqueued one).
func (s *CheckConfigService) RecoverPendingCheckJobs(ctx context.Context) (int, []PendingCheckTimeout, error) {
	candidates, err := s.checkRecoveryCandidates(ctx)
	if err != nil {
		return 0, nil, err
	}

	var total int
	var pending []PendingCheckTimeout
	var joinedErr error
	for _, candidate := range candidates {
		enqueued, names, err := s.recoverPendingCheckJobsForChange(ctx, candidate.change)
		// Surface whatever pending checks were collected even when the change's
		// recovery errored partway: arming a timeout for them is best-effort and
		// must not be lost to an unrelated per-change fault.
		if len(names) > 0 {
			pending = append(pending, PendingCheckTimeout{
				IssueID:    candidate.change.IssueID,
				HeadSHA:    candidate.change.HeadSHA,
				CheckNames: names,
			})
		}
		if err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("recover pending check jobs for change %s: %w", candidate.change.ID, err))
			continue
		}
		total += enqueued
	}

	return total, pending, joinedErr
}

func (s *CheckConfigService) checkRecoveryCandidates(ctx context.Context) ([]checkRecoveryCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ch.id, ch.issue_id, ch.branch, ch.base, ch.head_sha, ch.created_at, ch.updated_at, ch.ready_at, ch.merged_at
FROM changes ch
JOIN issues i ON i.id = ch.issue_id
WHERE i.triage_state = ?
	AND i.schedule_state != ?
	AND ch.ready_at IS NOT NULL
	AND ch.merged_at IS NULL
	AND ch.id = (
		SELECT current.id
		FROM changes current
		WHERE current.issue_id = i.id
			AND current.ready_at IS NOT NULL
			AND current.merged_at IS NULL
		ORDER BY current.updated_at DESC, current.created_at DESC, current.id DESC
		LIMIT 1
	)
ORDER BY ch.updated_at, ch.id`,
		string(TriageAccepted),
		string(ScheduleClosed),
	)
	if err != nil {
		return nil, fmt.Errorf("select check recovery candidates: %w", err)
	}
	defer rows.Close()

	var candidates []checkRecoveryCandidate
	for rows.Next() {
		change, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, checkRecoveryCandidate{change: change})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate check recovery candidates: %w", err)
	}

	return candidates, nil
}

func (s *CheckConfigService) recoverPendingCheckJobsForChange(ctx context.Context, change Change) (int, []string, error) {
	suite, err := s.LoadSuiteForChange(ctx, change)
	if err != nil {
		return 0, nil, err
	}
	issue, err := s.issues.GetIssue(ctx, change.IssueID)
	if err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(change.HeadSHA) != "" {
		suite, err = withDefaultAgentChecks(suite, issue.AgentHarness, s.harnessArgs.Add(issue.HarnessArgs))
		if err != nil {
			return 0, nil, err
		}
	}
	checks, err := s.ensureCurrentAutomatedChecks(ctx, change.IssueID, suite)
	if err != nil {
		return 0, nil, err
	}

	var enqueued int
	var pendingNames []string
	critiqueSatisfied := false
	checkedCritique := false
	for _, definition := range suite.Definitions {
		if definition.Kind == CheckKindHuman {
			continue
		}
		check, ok := checks[definition.Name]
		if !ok || check.Verdict != CheckPending || definition.Kind != check.Kind {
			continue
		}
		if definition.Phase == CheckPhaseAcceptance {
			if !checkedCritique {
				critiqueSatisfied, err = s.checks.CritiqueSatisfied(ctx, change.IssueID)
				if err != nil {
					return enqueued, pendingNames, err
				}
				checkedCritique = true
			}
			if !critiqueSatisfied {
				continue
			}
		}
		// This pending automated check is in a phase whose job the recovery
		// expects to report (critique always; acceptance once critique is
		// satisfied), so it needs a check timeout — record it whether or not a
		// job is enqueued below.
		pendingNames = append(pendingNames, definition.Name)
		ok, err := s.enqueueCheckJob(ctx, change.IssueID, change, definition)
		if err != nil {
			return enqueued, pendingNames, err
		}
		if ok {
			enqueued++
		}
	}

	return enqueued, pendingNames, nil
}

func (s *CheckConfigService) ensureCurrentAutomatedChecks(ctx context.Context, issueID string, suite CheckSuite) (map[string]Check, error) {
	checks, err := s.checks.ListChecks(ctx, issueID)
	if err != nil {
		return nil, err
	}
	current := map[string]Check{}
	for _, check := range checks {
		current[check.Name] = check
	}

	for _, definition := range suite.Definitions {
		if definition.Kind == CheckKindHuman {
			continue
		}
		check, ok := current[definition.Name]
		if ok && check.Verdict != CheckPending {
			continue
		}
		if ok && check.Kind == definition.Kind && check.Required == requiredForCheckDefinition(definition) {
			continue
		}
		if ok {
			if err := s.ensurePendingCheck(ctx, issueID, definition); err != nil {
				return nil, err
			}
			check, err = s.checks.GetCheck(ctx, issueID, definition.Name)
			if err != nil {
				return nil, err
			}
		} else {
			check, err = s.ensureCheckExists(ctx, issueID, definition)
			if err != nil {
				return nil, err
			}
		}
		current[check.Name] = check
	}

	return current, nil
}

func withDefaultAgentChecks(suite CheckSuite, harness string, args flowharness.Args) (CheckSuite, error) {
	harness = flowharness.NormalizeName(harness)
	if harness == "" {
		harness = flowharness.DefaultAgentName()
	}
	if err := flowharness.ValidateAgentName(harness); err != nil {
		return CheckSuite{}, err
	}
	var hasReviewer, hasVerifier bool
	usedNames := map[string]bool{}
	for _, definition := range suite.Definitions {
		usedNames[definition.Name] = true
		switch definition.Kind {
		case CheckKindReviewer:
			hasReviewer = true
		case CheckKindVerifier:
			hasVerifier = true
		}
	}
	if !hasReviewer {
		name := unusedDefaultCheckName(defaultReviewerCheckName, usedNames)
		definition, err := defaultAgentCheckDefinition(name, CheckKindReviewer, harness, args)
		if err != nil {
			return CheckSuite{}, err
		}
		suite.Definitions = append(suite.Definitions, definition)
		usedNames[name] = true
	}
	if !hasVerifier {
		name := unusedDefaultCheckName(defaultVerifierCheckName, usedNames)
		definition, err := defaultAgentCheckDefinition(name, CheckKindVerifier, harness, args)
		if err != nil {
			return CheckSuite{}, err
		}
		suite.Definitions = append(suite.Definitions, definition)
		usedNames[name] = true
	}

	return suite, nil
}

func defaultAgentCheckDefinition(name string, kind CheckKind, harness string, args flowharness.Args) (CheckDefinition, error) {
	phase := CheckPhaseCritique
	if kind == CheckKindVerifier {
		phase = CheckPhaseAcceptance
	}
	command, err := flowharness.DefaultAgentCheckCommandWithArgs(harness, args.For(harness))
	if err != nil {
		return CheckDefinition{}, err
	}
	return CheckDefinition{
		Name:  name,
		Kind:  kind,
		Phase: phase,
		Entrypoint: &CheckEntrypoint{
			Argv:  []string{command},
			Shell: true,
		},
		Requires: []string{flowharness.AgentHarnessLabel(harness)},
	}, nil
}

func unusedDefaultCheckName(preferred string, used map[string]bool) string {
	if !used[preferred] {
		return preferred
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", preferred, suffix)
		if !used[candidate] {
			return candidate
		}
	}
}

func (s *CheckConfigService) ensurePendingCheck(ctx context.Context, issueID string, definition CheckDefinition) error {
	return s.ensurePendingCheckWithDetails(ctx, issueID, definition, "")
}

// ensurePendingCheckWithDetails resets a check to pending, optionally seeding its
// Details. Details is empty for an ordinary review round; the completion-
// assessment round seeds CompletionAssessmentCheckMarker on the reviewer check so
// the recovering reviewer can detect the recovery mode from its own check.
func (s *CheckConfigService) ensurePendingCheckWithDetails(ctx context.Context, issueID string, definition CheckDefinition, details string) error {
	required := requiredForCheckDefinition(definition)
	_, err := s.checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issueID,
		Name:     definition.Name,
		Kind:     definition.Kind,
		Required: &required,
		Verdict:  CheckPending,
		Details:  details,
		Reporter: "coordinator",
	})
	return err
}

func requiredForCheckDefinition(definition CheckDefinition) bool {
	if definition.Required != nil {
		return *definition.Required
	}
	return true
}

func (s *CheckConfigService) retireRemovedConfiguredChecks(ctx context.Context, input ScheduleReviewRoundInput, currentSuite CheckSuite) error {
	if currentSuite.Configured {
		return s.retireAbsentAutomatedChecks(ctx, input.Issue.ID, currentSuite)
	}
	previousHeadSHA := strings.TrimSpace(input.PreviousHeadSHA)
	if previousHeadSHA == "" {
		return nil
	}
	previousChange := input.Change
	previousChange.HeadSHA = previousHeadSHA
	previousSuite, err := s.LoadSuiteForChange(ctx, previousChange)
	if err != nil {
		return err
	}
	if !previousSuite.Configured {
		return nil
	}

	return s.retireAbsentAutomatedChecks(ctx, input.Issue.ID, currentSuite)
}

func (s *CheckConfigService) retireAbsentAutomatedChecks(ctx context.Context, issueID string, suite CheckSuite) error {
	current := map[string]bool{}
	for _, definition := range suite.Definitions {
		if definition.Kind != CheckKindHuman {
			current[definition.Name] = true
		}
	}
	checks, err := s.checks.ListChecks(ctx, issueID)
	if err != nil {
		return err
	}
	required := false
	for _, check := range checks {
		if check.Kind == CheckKindHuman || current[check.Name] {
			continue
		}
		if _, err := s.checks.ReportCheck(ctx, ReportCheckInput{
			IssueID:  issueID,
			Name:     check.Name,
			Kind:     check.Kind,
			Required: &required,
			Verdict:  CheckSkipped,
			Details:  "check no longer exists in repo configuration",
			Reporter: "coordinator",
		}); err != nil {
			return err
		}
	}

	return nil
}

func (s *CheckConfigService) ensureHumanReviewCheck(ctx context.Context, issueID string, change Change, previousHeadSHA string) (bool, error) {
	required := true
	existing, err := s.checks.GetCheck(ctx, issueID, "human-review")
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.checks.ReportCheck(ctx, ReportCheckInput{
			IssueID:  issueID,
			Name:     "human-review",
			Kind:     CheckKindHuman,
			Required: &required,
			Verdict:  CheckPending,
			Reporter: "coordinator",
		}); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if existing.Verdict != CheckSatisfied {
		return false, nil
	}
	stale, err := s.humanReviewStale(ctx, issueID, change, previousHeadSHA)
	if err != nil {
		return false, err
	}
	if !stale {
		return false, nil
	}
	if _, err := s.checks.ReportCheck(ctx, ReportCheckInput{
		IssueID:  issueID,
		Name:     "human-review",
		Kind:     CheckKindHuman,
		Required: &required,
		Verdict:  CheckPending,
		Details:  "reset after changes touched human-commented files",
		Reporter: "coordinator",
	}); err != nil {
		return false, err
	}

	return false, nil
}

func (s *CheckConfigService) humanReviewStale(ctx context.Context, issueID string, change Change, previousHeadSHA string) (bool, error) {
	previousHeadSHA = strings.TrimSpace(previousHeadSHA)
	if previousHeadSHA == "" || strings.TrimSpace(change.HeadSHA) == "" || previousHeadSHA == strings.TrimSpace(change.HeadSHA) {
		return false, nil
	}
	exchangePath, ok, err := s.exchangePathForChange(ctx, change)
	if err != nil || !ok {
		return false, err
	}
	touched, err := flowgit.ChangedPaths(ctx, exchangePath, previousHeadSHA, change.HeadSHA)
	if err != nil {
		return false, err
	}
	if len(touched) == 0 {
		return false, nil
	}
	touchedSet := map[string]bool{}
	for _, path := range touched {
		touchedSet[path] = true
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT rt.file_path
FROM review_threads rt
LEFT JOIN review_comments rc ON rc.thread_id = rt.id
WHERE rt.issue_id = ?
	AND (
		rt.created_by LIKE 'owner%'
		OR rt.created_by LIKE 'human%'
		OR rc.actor LIKE 'owner%'
		OR rc.actor LIKE 'human%'
	)`, issueID)
	if err != nil {
		return false, fmt.Errorf("load human review thread paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return false, err
		}
		if touchedSet[strings.TrimSpace(path)] {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate human review thread paths: %w", err)
	}

	return false, nil
}

func (s *CheckConfigService) ensureCheckExists(ctx context.Context, issueID string, definition CheckDefinition) (Check, error) {
	if check, err := s.checks.GetCheck(ctx, issueID, definition.Name); err == nil {
		return check, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Check{}, err
	}

	if err := s.ensurePendingCheck(ctx, issueID, definition); err != nil {
		return Check{}, err
	}
	return s.checks.GetCheck(ctx, issueID, definition.Name)
}

func (s *CheckConfigService) enqueueCheckJob(ctx context.Context, issueID string, change Change, definition CheckDefinition) (bool, error) {
	role, bucket, err := jobRoleForCheck(definition.Kind)
	if err != nil {
		return false, err
	}
	headSHA := strings.TrimSpace(change.HeadSHA)
	exists, err := s.liveCheckJobExists(ctx, issueID, change.ID, role, definition.Name, headSHA)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	payload := map[string]any{
		"entrypoint": definition.Entrypoint.payloadMap(),
		"check_name": definition.Name,
		"change_id":  change.ID,
		"head_sha":   headSHA,
		"branch":     change.Branch,
		"base":       change.Base,
	}
	stampProjectPayload(payload, s.project)
	if (role == flowworker.RoleReviewer || role == flowworker.RoleVerifier) && s.threads != nil {
		reviewContext, err := s.threads.ReviewContextForIssue(ctx, issueID)
		if err != nil {
			return false, err
		}
		payload["review_context"] = reviewContext
	}
	job, err := s.workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		IssueID:        &issueID,
		ChangeID:       &change.ID,
		Role:           role,
		CapacityBucket: bucket,
		RunsOn:         definition.RunsOn,
		Requires:       definition.Requires,
		Size:           definition.Size,
		Tolerations:    definition.Tolerations,
		Payload:        payload,
	})
	if err != nil {
		exists, lookupErr := s.liveCheckJobExists(ctx, issueID, change.ID, role, definition.Name, headSHA)
		if lookupErr == nil && exists {
			return false, nil
		}
		return false, err
	}

	return job.ID != "", nil
}

func (s *CheckConfigService) exchangePathForChange(_ context.Context, _ Change) (string, bool, error) {
	exchangePath := strings.TrimSpace(s.project.ExchangePath)
	if exchangePath == "" {
		return "", false, nil
	}

	return exchangePath, true, nil
}

func parseCheckDefinition(path string, content string) (CheckDefinition, error) {
	var definition CheckDefinition
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&definition); err != nil {
		return CheckDefinition{}, fmt.Errorf("%s: parse check config: %w", path, err)
	}
	definition.sourcePath = path
	definition, err := normalizeCheckDefinition(definition)
	if err != nil {
		return CheckDefinition{}, fmt.Errorf("%s: %w", path, err)
	}

	return definition, nil
}

func normalizeCheckDefinition(definition CheckDefinition) (CheckDefinition, error) {
	definition.Name = strings.TrimSpace(definition.Name)
	if err := validateCheckName(definition.Name); err != nil {
		return CheckDefinition{}, err
	}
	if definition.Kind == "" {
		return CheckDefinition{}, errors.New("kind is required")
	}
	if err := validateCheckKind(definition.Kind); err != nil {
		return CheckDefinition{}, err
	}
	if definition.Phase == "" {
		if definition.Kind == CheckKindVerifier {
			definition.Phase = CheckPhaseAcceptance
		} else {
			definition.Phase = CheckPhaseCritique
		}
	}
	switch definition.Phase {
	case CheckPhaseCritique, CheckPhaseAcceptance:
	default:
		return CheckDefinition{}, fmt.Errorf("invalid phase: %s", definition.Phase)
	}
	if definition.Kind == CheckKindVerifier && definition.Phase != CheckPhaseAcceptance {
		return CheckDefinition{}, errors.New("verifier checks must use acceptance phase")
	}
	if definition.Kind != CheckKindVerifier && definition.Phase == CheckPhaseAcceptance {
		return CheckDefinition{}, errors.New("only verifier checks may use acceptance phase")
	}
	if definition.Kind == CheckKindHuman {
		if definition.Entrypoint != nil {
			return CheckDefinition{}, errors.New("human checks must not define entrypoint")
		}
		return definition, nil
	}
	if definition.Entrypoint == nil {
		return CheckDefinition{}, errors.New("entrypoint is required")
	}
	if err := validateCheckEntrypoint(*definition.Entrypoint); err != nil {
		return CheckDefinition{}, err
	}
	if _, err := scheduler.CompileSelector(scheduler.SelectorInput{
		RunsOn:   definition.RunsOn,
		Requires: definition.Requires,
		Size:     definition.Size,
	}); err != nil {
		return CheckDefinition{}, err
	}

	return definition, nil
}

func validateCheckEntrypoint(entrypoint CheckEntrypoint) error {
	if len(entrypoint.Argv) == 0 {
		return errors.New("entrypoint argv is required")
	}
	for _, arg := range entrypoint.Argv {
		if strings.TrimSpace(arg) == "" {
			return errors.New("entrypoint argv entries must not be empty")
		}
	}
	if entrypoint.Shell && len(entrypoint.Argv) != 1 {
		return errors.New("shell entrypoints require exactly one argv command string")
	}
	if filepath.IsAbs(entrypoint.CWD) {
		return errors.New("entrypoint cwd must be relative")
	}
	if strings.Contains(entrypoint.CWD, "..") {
		cleaned := filepath.Clean(entrypoint.CWD)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return errors.New("entrypoint cwd must stay inside the job worktree")
		}
	}
	for key := range entrypoint.Env {
		if !validCheckEnvKey(key) {
			return fmt.Errorf("entrypoint env key %q is invalid", key)
		}
		if strings.HasPrefix(strings.ToUpper(key), "FLOW_") {
			return fmt.Errorf("entrypoint env cannot override reserved FLOW_* variable %q", key)
		}
	}

	return nil
}

func (entrypoint CheckEntrypoint) payloadMap() map[string]any {
	payload := map[string]any{
		"argv":  entrypoint.Argv,
		"shell": entrypoint.Shell,
	}
	if strings.TrimSpace(entrypoint.CWD) != "" {
		payload["cwd"] = strings.TrimSpace(entrypoint.CWD)
	}
	if len(entrypoint.Env) > 0 {
		payload["env"] = entrypoint.Env
	}

	return payload
}

func jobRoleForCheck(kind CheckKind) (flowworker.JobRole, flowworker.CapacityBucket, error) {
	switch kind {
	case CheckKindCI:
		return flowworker.RoleCI, flowworker.BucketEphemeral, nil
	case CheckKindReviewer:
		return flowworker.RoleReviewer, flowworker.BucketPersistentAgent, nil
	case CheckKindVerifier:
		return flowworker.RoleVerifier, flowworker.BucketPersistentAgent, nil
	default:
		return "", "", fmt.Errorf("check kind %s does not enqueue worker jobs", kind)
	}
}

func (s *CheckConfigService) liveCheckJobExists(ctx context.Context, issueID string, changeID string, role flowworker.JobRole, checkName string, headSHA string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT payload_json
FROM jobs
WHERE issue_id = ?
	AND change_id = ?
	AND role = ?
	AND state IN (?, ?, ?)`,
		strings.TrimSpace(issueID),
		strings.TrimSpace(changeID),
		string(role),
		string(flowworker.JobQueued),
		string(flowworker.JobClaimed),
		string(flowworker.JobRunning),
	)
	if err != nil {
		return false, fmt.Errorf("query live check jobs: %w", err)
	}
	defer rows.Close()

	checkName = strings.TrimSpace(checkName)
	headSHA = strings.TrimSpace(headSHA)
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return false, err
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return false, fmt.Errorf("decode check job payload: %w", err)
		}
		if payloadString(payload, "check_name") == checkName && payloadString(payload, "head_sha") == headSHA {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate live check jobs: %w", err)
	}

	return false, nil
}

var checkEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validCheckEnvKey(key string) bool {
	return checkEnvKeyPattern.MatchString(key)
}
