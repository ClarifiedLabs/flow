package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

// newHoldFlow builds gate -> done, a graph small enough that every hold test
// can reach an interesting state in two moves.
func newHoldFlow(t *testing.T, flows *FlowService, tasks *TaskService, runs *WorkflowRunService) (Task, WorkflowRun, WorkflowNodeRun) {
	t.Helper()
	ctx := context.Background()
	flow, err := flows.Create(ctx, FlowInput{
		Name:             "hold fixture",
		StartNode:        "gate",
		TransitionBudget: 5,
		Nodes: []FlowNodeInput{
			{Key: "gate", Name: "Wait for approval", Kind: NodeHumanGate, Config: FlowNodeConfig{HumanGate: &HumanGateNodeConfig{Outcomes: []string{"approved", "rejected"}}}},
			{Key: "done", Name: "Merge the change", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCompleted}}},
			{Key: "dropped", Name: "Dropped", Kind: NodeTerminal, Config: FlowNodeConfig{Terminal: &TerminalNodeConfig{Resolution: ResolutionCancelled}}},
		},
		Edges: []FlowEdgeInput{
			{From: "gate", Outcome: "approved", To: "done"},
			{From: "gate", Outcome: "rejected", To: "dropped"},
		},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Held task", FlowID: flow.ID})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	run, err := runs.Schedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("schedule run: %v", err)
	}
	nodeRun, _, err := runs.EnsureCurrentNode(ctx, run.ID)
	if err != nil {
		t.Fatalf("enter first node: %v", err)
	}
	return task, run, nodeRun
}

func convergenceEvidenceFixture(task Task, run WorkflowRun, nodeRun WorkflowNodeRun) ConvergenceEvidence {
	return ConvergenceEvidence{
		WorkflowRunID: run.ID, NodeRunID: nodeRun.ID, ChangeID: "ch-convergence-0001", TaskID: task.ID,
		SourceBranch: "task/" + task.ID, SourceHeadSHA: strings.Repeat("a", 40),
		TargetBaseBranch: "main", TargetBaseTipSHA: strings.Repeat("b", 40),
		MergeBaseSHA: strings.Repeat("c", 40), DiffDigest: "sha256:" + strings.Repeat("d", 64),
		Files: 8, Additions: 420, Deletions: 160, MaxFiles: 5, MaxLines: 500,
	}
}

func installConvergenceProjection(t *testing.T, runs *WorkflowRunService, evidence ConvergenceEvidence) {
	t.Helper()
	ctx := context.Background()
	artifactID := "wa-convergence-" + evidence.WorkflowRunID
	payload, err := json.Marshal(map[string]string{"change_id": evidence.ChangeID, "head_sha": evidence.SourceHeadSHA})
	if err != nil {
		t.Fatalf("marshal convergence artifact: %v", err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO changes (id, task_id, workflow_run_id, branch, base, head_sha, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	task_id = excluded.task_id,
	workflow_run_id = excluded.workflow_run_id,
	branch = excluded.branch,
	base = excluded.base,
	head_sha = excluded.head_sha,
	updated_at = excluded.updated_at`, evidence.ChangeID, evidence.TaskID, evidence.WorkflowRunID,
		evidence.SourceBranch, evidence.TargetBaseBranch, evidence.SourceHeadSHA, now, now); err != nil {
		t.Fatalf("insert convergence change projection: %v", err)
	}
	if _, err := runs.db.ExecContext(ctx, `
INSERT INTO workflow_artifacts (
	id, workflow_run_id, node_run_id, creator_key, kind, summary_markdown,
	payload_json, payload_sha256, client_key, created_at
) VALUES (?, ?, ?, 'test-convergence', 'change', 'Convergence fixture', ?, 'digest', 'convergence-fixture', ?)`,
		artifactID, evidence.WorkflowRunID, evidence.NodeRunID, string(payload), now); err != nil {
		t.Fatalf("insert convergence artifact projection: %v", err)
	}
	if _, err := runs.db.ExecContext(ctx, `
UPDATE workflow_runs SET current_artifact_id = ? WHERE id = ?;
UPDATE workflow_node_runs SET input_artifact_id = ? WHERE id = ?`,
		artifactID, evidence.WorkflowRunID, artifactID, evidence.NodeRunID); err != nil {
		t.Fatalf("project convergence artifact: %v", err)
	}
}

func TestHoldStopsTheExecutorAndComposesWithAnOpenWait(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, nodeRun := newHoldFlow(t, flows, tasks, runs)

	// The gate already opened a wait. A hold has to survive alongside it —
	// this is the case a workflow_waits row could not model, because
	// idx_workflow_waits_one_open allows only one open row per run.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait: %v", err)
	} else if !waiting {
		t.Fatal("fixture should be parked on a human gate")
	}

	held, err := runs.Hold(ctx, task.ID, ActorHuman)
	if err != nil {
		t.Fatalf("hold run: %v", err)
	}
	if !held.Held() {
		t.Fatal("run should report held")
	}
	if held.HeldBy != string(ActorHuman) {
		t.Fatalf("held_by = %q, want %q", held.HeldBy, ActorHuman)
	}
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait after hold: %v", err)
	} else if !waiting {
		t.Fatal("holding must not resolve the gate's wait")
	}

	// Holding twice is a no-op rather than a second transition, so a double
	// click cannot stack audit rows.
	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("re-hold run: %v", err)
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	holds := 0
	for _, transition := range transitions {
		if transition.EventKind == "workflow_hold_requested" {
			holds++
		}
	}
	if holds != 1 {
		t.Fatalf("hold transitions = %d, want 1", holds)
	}
	_ = nodeRun
}

func TestReleaseResumeClearsTheHoldWithoutMovingTheRun(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseResume, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release run: %v", err)
	}
	if result.Run.Held() {
		t.Fatal("released run should not report held")
	}
	if result.Run.CurrentNodeKey != "gate" {
		t.Fatalf("current node = %q, want gate — resume must not advance", result.Run.CurrentNodeKey)
	}
	if result.Done {
		t.Fatal("resume must not complete the run")
	}
}

func TestConvergenceHoldIsRequestedOnlyOncePerRun(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)
	evidence := convergenceEvidenceFixture(task, run, nodeRun)
	evidence.ChangedFiles = []ConvergenceFile{
		{Path: "internal/web/assets/storage.js", Additions: 280, Deletions: 80},
		{Path: "AGENTS.md", Additions: 0, Deletions: 40},
	}
	installConvergenceProjection(t, runs, evidence)

	held, created, err := runs.HoldForConvergence(ctx, evidence)
	if err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}
	if !created || !held.Held() || held.HeldBy != string(ActorSystem) {
		t.Fatalf("convergence hold = %+v created=%t, want a system hold", held, created)
	}
	if _, err := runs.Release(ctx, ReleaseWorkflowInput{
		TaskID: task.ID, Edge: ReleaseResume, Actor: ActorHuman,
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("generic convergence release error = %v, want workflow conflict", err)
	}
	if _, err := runs.Reset(ctx, task.ID, ActorAgent); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("convergence reset error = %v, want workflow conflict", err)
	}
	if _, err := runs.ForceDone(ctx, task.ID, ResolutionCancelled, "generic cancellation", ActorHuman); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("generic convergence force-done error = %v, want workflow conflict", err)
	}
	activeEvidence, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || activeEvidence == nil {
		t.Fatalf("load convergence evidence before acceptance: evidence=%+v err=%v", activeEvidence, err)
	}
	resolved, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceAcceptScope, Actor: ActorHuman, Note: "scope accepted",
		ExpectedEvidenceFingerprint: activeEvidence.Fingerprint,
	})
	if err != nil {
		t.Fatalf("accept convergence scope: %v", err)
	}
	if resolved.Run.Held() {
		t.Fatal("accepted convergence scope should release the run")
	}
	replayed, created, err := runs.HoldForConvergence(ctx, evidence)
	if err != nil {
		t.Fatalf("repeat convergence hold: %v", err)
	}
	if created || replayed.Held() {
		t.Fatalf("repeat convergence hold = %+v created=%t, want no second hold", replayed, created)
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list convergence transitions: %v", err)
	}
	requested := 0
	fingerprint := ""
	for _, transition := range transitions {
		if transition.EventKind != "workflow_convergence_review_requested" {
			continue
		}
		requested++
		var payload ConvergenceEvidence
		if err := json.Unmarshal(transition.Payload, &payload); err != nil {
			t.Fatalf("decode convergence payload: %v", err)
		}
		if len(payload.ChangedFiles) != 2 || payload.ChangedFiles[1].Path != "AGENTS.md" || payload.ChangedFilesOmitted != 6 {
			t.Fatalf("changed file evidence = %+v omitted=%d, want two files with six omitted", payload.ChangedFiles, payload.ChangedFilesOmitted)
		}
		if payload.SchemaVersion != ConvergenceEvidenceSchemaVersion || payload.WorkflowRunID != run.ID || payload.NodeRunID != nodeRun.ID || payload.Fingerprint == "" {
			t.Fatalf("typed convergence evidence = %+v, want versioned workflow identity and fingerprint", payload)
		}
		fingerprint = payload.Fingerprint
	}
	if requested != 1 {
		t.Fatalf("convergence requests = %d, want 1", requested)
	}
	resolvedCount := 0
	for _, transition := range transitions {
		if transition.EventKind != "workflow_convergence_review_resolved" {
			continue
		}
		resolvedCount++
		var payload convergenceResolutionPayload
		if err := json.Unmarshal(transition.Payload, &payload); err != nil {
			t.Fatalf("decode convergence resolution: %v", err)
		}
		if payload.Disposition != ConvergenceAcceptScope || payload.EvidenceFingerprint != fingerprint ||
			payload.Actor != string(ActorHuman) || payload.Note != "scope accepted" {
			t.Fatalf("convergence resolution payload = %+v", payload)
		}
	}
	if resolvedCount != 1 {
		t.Fatalf("convergence resolutions = %d, want 1", resolvedCount)
	}
	statuses, err := NewStatusService(runs.db).ListForTask(ctx, task.ID, 10)
	if err != nil {
		t.Fatalf("list convergence status: %v", err)
	}
	if len(statuses) == 0 || !strings.Contains(statuses[0].Message, "`AGENTS.md` — +0/-40") ||
		!strings.Contains(statuses[0].Message, "Repair the branch first") {
		t.Fatalf("convergence status = %+v, want changed paths and repair guidance", statuses)
	}
}

func TestManualConvergenceReviewConvertsAHoldAndCanBeRequestedAgain(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)
	evidence := convergenceEvidenceFixture(task, run, nodeRun)
	installConvergenceProjection(t, runs, evidence)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("create generic hold: %v", err)
	}
	held, created, err := runs.RequestConvergenceReview(ctx, evidence, ActorHuman)
	if err != nil {
		t.Fatalf("convert to manual convergence review: %v", err)
	}
	if !created || !held.Held() || held.HeldBy != string(ActorSystem) {
		t.Fatalf("manual convergence hold = %+v created=%t, want system-enforced typed hold", held, created)
	}
	if _, replayed, err := runs.RequestConvergenceReview(ctx, evidence, ActorHuman); err != nil || replayed {
		t.Fatalf("duplicate active manual review created=%t err=%v, want no-op", replayed, err)
	}

	active, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || active == nil {
		t.Fatalf("load manual convergence evidence: evidence=%+v err=%v", active, err)
	}
	if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceAcceptScope, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: active.Fingerprint,
	}); err != nil {
		t.Fatalf("accept manual convergence review: %v", err)
	}

	held, created, err = runs.RequestConvergenceReview(ctx, evidence, ActorHuman)
	if err != nil {
		t.Fatalf("request a later manual convergence review: %v", err)
	}
	if !created || !held.Held() {
		t.Fatalf("later manual convergence hold = %+v created=%t, want a new hold", held, created)
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list manual convergence transitions: %v", err)
	}
	requested := 0
	for _, transition := range transitions {
		if transition.EventKind == "workflow_convergence_review_requested" {
			requested++
			if transition.Actor != string(ActorHuman) {
				t.Fatalf("manual convergence transition actor = %q, want human", transition.Actor)
			}
		}
	}
	if requested != 2 {
		t.Fatalf("manual convergence requests = %d, want 2", requested)
	}
}

func TestConvergenceRepairKeepsEvidenceActiveAndCancelClosesTask(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)
	evidence := convergenceEvidenceFixture(task, run, nodeRun)
	installConvergenceProjection(t, runs, evidence)
	if _, _, err := runs.HoldForConvergence(ctx, evidence); err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}
	reviewedEvidence, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || reviewedEvidence == nil {
		t.Fatalf("load reviewed convergence evidence: evidence=%+v err=%v", reviewedEvidence, err)
	}

	if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceRepairBranch, Actor: ActorHuman,
		ExpectedEvidenceFingerprint: "stale-reviewed-fingerprint",
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("repair with stale reviewed evidence error = %v, want workflow conflict", err)
	}
	repaired, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceRepairBranch, Actor: ActorHuman, Note: "reduce the patch",
		ExpectedEvidenceFingerprint: reviewedEvidence.Fingerprint,
	})
	if err != nil {
		t.Fatalf("repair convergence branch: %v", err)
	}
	if !repaired.Run.Held() {
		t.Fatal("repair disposition must keep the source run held")
	}
	detail, err := runs.Detail(ctx, run.ID)
	if err != nil {
		t.Fatalf("load held workflow detail: %v", err)
	}
	if detail.ConvergenceEvidence == nil || detail.ConvergenceEvidence.Fingerprint == "" {
		t.Fatalf("detail convergence evidence = %+v, want active typed evidence", detail.ConvergenceEvidence)
	}
	for range 55 {
		if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
			TaskID: task.ID, Disposition: ConvergenceRepairBranch, Actor: ActorHuman,
			ExpectedEvidenceFingerprint: detail.ConvergenceEvidence.Fingerprint,
		}); err != nil {
			t.Fatalf("record repeated repair disposition: %v", err)
		}
	}
	activeEvidence, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || activeEvidence == nil || activeEvidence.Fingerprint != detail.ConvergenceEvidence.Fingerprint {
		t.Fatalf("active evidence after bounded activity window = %+v, err=%v", activeEvidence, err)
	}
	workers := flowworker.NewService(runs.db)
	consoleJob, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, Role: flowworker.RoleConsole, CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{"console_harness": "harness"},
	})
	if err != nil {
		t.Fatalf("enqueue repair console job: %v", err)
	}

	if _, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceCancel, Actor: ActorHuman, Note: "not worth decomposing",
		ExpectedEvidenceFingerprint: detail.ConvergenceEvidence.Fingerprint,
	}); !errors.Is(err, ErrWorkflowConflict) {
		t.Fatalf("cancel with live repair console error = %v, want workflow conflict", err)
	}
	if _, err := runs.db.ExecContext(ctx, `
UPDATE jobs SET state = 'finished' WHERE id = ?`, consoleJob.ID); err != nil {
		t.Fatalf("finish repair console job: %v", err)
	}
	cleanupJob, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, Role: flowworker.RoleCI, CapacityBucket: flowworker.BucketEphemeral,
		Payload: map[string]any{"blocking": true},
	})
	if err != nil {
		t.Fatalf("enqueue task-scoped cleanup job: %v", err)
	}

	cancelled, err := runs.ResolveConvergenceReview(ctx, ResolveConvergenceReviewInput{
		TaskID: task.ID, Disposition: ConvergenceCancel, Actor: ActorHuman, Note: "not worth decomposing",
		ExpectedEvidenceFingerprint: detail.ConvergenceEvidence.Fingerprint,
	})
	if err != nil {
		t.Fatalf("cancel convergence implementation after console exit: %v", err)
	}
	if cancelled.Task == nil || cancelled.Task.State == nil || *cancelled.Task.State != LifecycleDone ||
		cancelled.Task.DoneResolution == nil || *cancelled.Task.DoneResolution != ResolutionCancelled {
		t.Fatalf("cancelled task = %+v, want done/cancelled", cancelled.Task)
	}
	if cancelled.Run.State != WorkflowRunCompleted || cancelled.Run.Held() {
		t.Fatalf("cancel response run = %+v, want completed and unheld", cancelled.Run)
	}
	completedRun, err := runs.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("load cancelled run: %v", err)
	}
	if completedRun.State != WorkflowRunCompleted || completedRun.Held() {
		t.Fatalf("completed run = %+v, want completed and unheld", completedRun)
	}
	var consoleJobState string
	if err := runs.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, consoleJob.ID).Scan(&consoleJobState); err != nil {
		t.Fatalf("load cancelled repair console job: %v", err)
	}
	if consoleJobState != string(flowworker.JobFinished) {
		t.Fatalf("repair console job state = %q, want finished", consoleJobState)
	}
	var cleanupJobState string
	if err := runs.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, cleanupJob.ID).Scan(&cleanupJobState); err != nil {
		t.Fatalf("load cancelled task-scoped job: %v", err)
	}
	if cleanupJobState != string(flowworker.JobCanceled) {
		t.Fatalf("task-scoped job state = %q, want canceled", cleanupJobState)
	}
	if _, err := workers.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, Role: flowworker.RoleConsole, CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{"console_harness": "harness"},
	}); err == nil {
		t.Fatal("terminal convergence task accepted a new console job")
	}
}

func TestRefreshConvergenceEvidenceAllowsBaseOnlyMovementWithoutRepair(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)
	evidence := convergenceEvidenceFixture(task, run, nodeRun)
	installConvergenceProjection(t, runs, evidence)
	if _, _, err := runs.HoldForConvergence(ctx, evidence); err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}
	active, err := runs.ActiveConvergenceEvidenceForTask(ctx, task.ID)
	if err != nil || active == nil {
		t.Fatalf("load active evidence: evidence=%+v err=%v", active, err)
	}

	refreshedInput := *active
	refreshedInput.TargetBaseTipSHA = strings.Repeat("e", 40)
	refreshedInput.MergeBaseSHA = strings.Repeat("f", 40)
	refreshedInput.DiffDigest = "sha256:" + strings.Repeat("1", 64)
	refreshed, err := runs.RefreshConvergenceEvidence(ctx, refreshedInput)
	if err != nil {
		t.Fatalf("refresh base-only movement without repair: %v", err)
	}
	if refreshed.TargetBaseTipSHA != refreshedInput.TargetBaseTipSHA || refreshed.Fingerprint == active.Fingerprint {
		t.Fatalf("refreshed evidence = %+v, want changed base tip and fingerprint", refreshed)
	}
}

func TestConvergenceEvidenceFingerprintIgnoresCaptureTime(t *testing.T) {
	evidence := ConvergenceEvidence{SchemaVersion: ConvergenceEvidenceSchemaVersion, WorkflowRunID: "wr-1", DiffDigest: "sha256:content"}
	evidence.CapturedAt = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	first, err := convergenceEvidenceFingerprint(evidence)
	if err != nil {
		t.Fatalf("fingerprint first observation: %v", err)
	}
	evidence.CapturedAt = evidence.CapturedAt.Add(time.Hour)
	evidence.Fingerprint = "stale"
	second, err := convergenceEvidenceFingerprint(evidence)
	if err != nil {
		t.Fatalf("fingerprint replayed observation: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprints differ by capture time: %q != %q", first, second)
	}
}

func TestConvergenceFileEvidenceIsBoundedAndEscaped(t *testing.T) {
	files := []ConvergenceFile{{
		Path: "a`b\n\u202ec", Additions: 10_000,
	}}
	for range 150 {
		files = append(files, ConvergenceFile{Path: strings.Repeat("x", 2_048), Additions: 1})
	}
	files = sortConvergenceFiles(files)

	payloadFiles, omitted := convergencePayloadFiles(len(files), files)
	encoded, err := json.Marshal(payloadFiles)
	if err != nil {
		t.Fatalf("encode bounded file evidence: %v", err)
	}
	if len(payloadFiles) >= len(files) || omitted != len(files)-len(payloadFiles) {
		t.Fatalf("payload files = %d omitted=%d, want a bounded subset of %d", len(payloadFiles), omitted, len(files))
	}
	if len(encoded) > convergencePayloadByteLimit {
		t.Fatalf("payload file evidence = %d bytes, limit %d", len(encoded), convergencePayloadByteLimit)
	}

	message := convergenceHoldMessage(len(files), 10_150, 0, 10, 500, files, true)
	for _, want := range []string{`a\x60b\n\u202ec`, "(path truncated)", "131 more changed files omitted"} {
		if !strings.Contains(message, want) {
			t.Fatalf("convergence message missing %q:\n%s", want, message)
		}
	}
	if strings.ContainsRune(message, '\u202e') {
		t.Fatalf("convergence message contains an unescaped bidi control:\n%s", message)
	}
}

func TestConvergenceHoldCountsAsBlockedOnTheBoard(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, run, nodeRun := newHoldFlow(t, flows, tasks, runs)
	evidence := convergenceEvidenceFixture(task, run, nodeRun)
	installConvergenceProjection(t, runs, evidence)

	if _, _, err := runs.HoldForConvergence(ctx, evidence); err != nil {
		t.Fatalf("hold for convergence: %v", err)
	}

	result, err := tasks.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board result: %v", err)
	}
	// The convergence hold parks the task on a human scope decision, so it is
	// blocked: it shows up in the blocked overlay even though its lane state
	// stays held.
	if lane := result.LaneStates[task.ID]; lane != LaneStateHeld {
		t.Fatalf("lane state = %q, want held", lane)
	}
	assertBlockedIDs(t, result.BlockedIDs, []string{task.ID})
	if reason := result.WaitReasons[task.ID]; reason != WaitReasonBlocked {
		t.Fatalf("wait reason = %q, want %q", reason, WaitReasonBlocked)
	}
}

func TestManualHoldDoesNotCountAsBlockedOnTheBoard(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}

	result, err := tasks.BoardResult(ctx)
	if err != nil {
		t.Fatalf("board result: %v", err)
	}
	// A manual hold is owned by the operator, not blocked on a decision, so it
	// keeps the held lane state without joining the blocked overlay.
	if lane := result.LaneStates[task.ID]; lane != LaneStateHeld {
		t.Fatalf("lane state = %q, want held", lane)
	}
	assertBlockedIDs(t, result.BlockedIDs, nil)
	if reason := result.WaitReasons[task.ID]; reason != "" {
		t.Fatalf("wait reason = %q, want none for a manual hold", reason)
	}
}

func TestReleaseSatisfyTakesTheSuccessEdgeWithoutAnArtifact(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseSatisfy, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release satisfy: %v", err)
	}
	// "approved" is the gate's first configured outcome, so satisfy routes to
	// the done terminal rather than the dropped one.
	if !result.Done {
		t.Fatalf("run state = %q, want the run completed through the success edge", result.Run.State)
	}
	reloaded, err := tasks.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if reloaded.DoneResolution == nil || *reloaded.DoneResolution != ResolutionCompleted {
		t.Fatalf("resolution = %v, want completed", reloaded.DoneResolution)
	}
}

func TestReleaseMergeJumpsToTheTerminalNode(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	result, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseMerge, Actor: ActorHuman})
	if err != nil {
		t.Fatalf("release merge: %v", err)
	}
	if !result.Done {
		t.Fatalf("run state = %q, want completed", result.Run.State)
	}
	// The gate's wait must not outlive the jump, or the task stays blocked
	// forever on a node the run already left.
	if _, waiting, err := runs.OpenWait(ctx, task.ID); err != nil {
		t.Fatalf("read open wait: %v", err)
	} else if waiting {
		t.Fatal("jumping to the terminal should resolve the open wait")
	}
	transitions, err := runs.ListTransitionsForTask(ctx, task.ID, 50)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	var jumped bool
	for _, transition := range transitions {
		if transition.EventKind == "node_jumped" && transition.ToNodeKey == "done" {
			jumped = true
		}
	}
	if !jumped {
		t.Fatalf("transitions = %+v, want a node_jumped audit row", transitions)
	}
}

func TestReleaseWithoutAHoldIsRejected(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	_, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseResume, Actor: ActorHuman})
	if !errors.Is(err, ErrWorkflowNotHeld) {
		t.Fatalf("release err = %v, want ErrWorkflowNotHeld", err)
	}
}

func TestReleaseSubmitRequiresAnArtifact(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	if _, err := runs.Release(ctx, ReleaseWorkflowInput{TaskID: task.ID, Edge: ReleaseSubmit, Actor: ActorHuman}); err == nil {
		t.Fatal("submit without an artifact should fail")
	}
}

func TestCardStateReportsStepPositionDwellAndWait(t *testing.T) {
	ctx := context.Background()
	flows, tasks, runs := newWorkflowModelServices(t)
	task, _, _ := newHoldFlow(t, flows, tasks, runs)

	state, ok, err := runs.CardState(ctx, task.ID)
	if err != nil || !ok {
		t.Fatalf("card state: %v (ok=%t)", err, ok)
	}
	if state.StepCount != 3 {
		t.Fatalf("step count = %d, want 3", state.StepCount)
	}
	if state.StepIndex != 1 {
		t.Fatalf("step index = %d, want 1 (gate is the first node)", state.StepIndex)
	}
	if state.Wait == nil || state.Wait.Kind != WorkflowWaitHumanGate {
		t.Fatalf("wait = %+v, want the open human gate", state.Wait)
	}
	// An open wait is the most specific answer to "how long has it been
	// here", so dwell tracks the wait rather than the run.
	if !state.DwellSince.Equal(state.Wait.CreatedAt) {
		t.Fatalf("dwell = %v, want the wait's created_at %v", state.DwellSince, state.Wait.CreatedAt)
	}
	if state.Held {
		t.Fatal("card state should not report held before a hold")
	}

	if _, err := runs.Hold(ctx, task.ID, ActorHuman); err != nil {
		t.Fatalf("hold run: %v", err)
	}
	state, _, err = runs.CardState(ctx, task.ID)
	if err != nil {
		t.Fatalf("card state after hold: %v", err)
	}
	if !state.Held || state.HeldBy != string(ActorHuman) {
		t.Fatalf("card state = %+v, want held by human", state)
	}
}
