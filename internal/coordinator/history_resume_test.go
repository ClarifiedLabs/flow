package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestCreateHistoryResumeDerivesImmutableLineageAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	env := newHistoryCaptureTestEnv(t)
	project := Project{ID: "p-history", Name: "History", BaseBranch: "main"}
	tasks := NewTaskService(env.store.DB(), project.ID)
	task, err := tasks.CreateTask(ctx, CreateTaskInput{Title: "Resume captured Harness work"})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("b", 40)
	insertChangeForTest(t, env.store.DB(), task.ID, "ch-history-resume", "task/history-resume", false)
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = 'ch-history-resume'`, head); err != nil {
		t.Fatal(err)
	}

	queue := flowworker.NewService(env.store.DB())
	sourceJob, err := queue.EnqueueJob(ctx, flowworker.EnqueueJobInput{
		TaskID: &task.ID, ChangeID: stringPointer("ch-history-resume"),
		Role: flowworker.RoleAuthor, CapacityBucket: flowworker.BucketPersistentAgent,
		Payload: map[string]any{
			"branch": "task/history-resume", "base": "main", "head_sha": head,
			"entrypoint": map[string]any{
				"argv":  []any{`prompt=$(cat); exec harness --session "$FLOW_HARNESS_SESSION" "$prompt"`},
				"shell": true, "harness": "harness",
			}, "agent_harness": "harness", "phase_index": 0, "final_phase": true},
	})
	if err != nil {
		t.Fatal(err)
	}

	reservation := baseHistoryReservation()
	reservation.ProjectID = project.ID
	reservation.JobID = sourceJob.ID
	reservation.LeaseID = "lease-history-resume-source"
	reservation.TaskID = task.ID
	reservation.ChangeID = "ch-history-resume"
	reserved := reserveHistoryCapture(t, env.service, reservation)
	capture := transitionHistory(t, env.service, reserved.Capture, HistoryCaptureRunning)

	harnessArtifact := publishHistoryBytes(t, env.service, capture.ID, reserved.UploadGrant, PublishHistoryArtifactInput{
		LogicalKey: "harness/final/root", Kind: HistoryArtifactHarnessRoot, Phase: HistoryArtifactFinal,
		ArchiveID: "native-root", MediaType: "application/x-tar", LogicalSize: 7, EntryCount: 2,
	}, []byte("harness"))
	if err := env.service.RegisterHarnessArchiveMembers(ctx, capture.ID, reserved.UploadGrant, "harness/final/root", []HarnessArchiveMemberInput{
		{NativeSessionID: "native-root", RelativeMemberPath: "state.json", MemberKind: "root", HarnessBuild: "0.4.5", ParseStatus: "parsed"},
		{NativeSessionID: "native-child", NativeParentSessionID: "native-root", RelativeMemberPath: "children/native-child/state.json", MemberKind: "delegated_child", HarnessBuild: "0.4.5", ParseStatus: "parsed"},
	}); err != nil {
		t.Fatal(err)
	}
	observed, err := env.service.Get(ctx, capture.ID)
	if err != nil || observed.HarnessVersion != "0.4.5" {
		t.Fatalf("observed source Harness build = %q, err=%v", observed.HarnessVersion, err)
	}
	workspaceArtifact := publishWorkspaceSummary(t, env.service, capture.ID, reserved.UploadGrant)
	publishCanonicalManifestBytes(t, env.service, capture.ID, PublishHistoryArtifactInput{
		LogicalKey: "manifest/final", Kind: HistoryArtifactManifest, Phase: HistoryArtifactFinal,
		MediaType: "application/json", LogicalSize: 2,
	}, []byte("{}"))

	exitCode := 0
	capture, err = env.service.RecordExecutionVerdict(ctx, capture.ID, RecordHistoryExecutionVerdictInput{
		Verdict: HistoryExecutionSucceeded, ExitCode: &exitCode, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	capture = transitionHistory(t, env.service, capture, HistoryCaptureQuiescing)
	emptySeal := TranscriptSeal{FinalEpoch: -1, SHA256: historyDigest(nil)}
	if err := env.service.SealTranscript(ctx, capture.ID, reserved.UploadGrant, emptySeal); err != nil {
		t.Fatal(err)
	}
	capture, err = env.service.DeclareExpectedSet(ctx, capture.ID, reserved.UploadGrant, DeclareHistoryExpectedSetInput{
		Artifacts: []FinalArtifactExpectation{
			{LogicalKey: "harness/final/root", Kind: HistoryArtifactHarnessRoot},
			{LogicalKey: "workspace/final", Kind: HistoryArtifactWorkspaceSnapshot},
			{LogicalKey: "manifest/final", Kind: HistoryArtifactManifest},
		},
		TranscriptSeal: &emptySeal, ExpectedVersion: capture.Version, Actor: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	capture = transitionHistory(t, env.service, capture, HistoryCaptureSealed)
	capture = transitionHistory(t, env.service, capture, HistoryCaptureUploading)
	capture, err = env.service.Complete(ctx, capture.ID, reserved.UploadGrant, capture.Version, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.CancelLiveJobsForTask(ctx, task.ID, flowworker.RoleAuthor); err != nil {
		t.Fatal(err)
	}
	listedCapture, err := env.service.Get(ctx, capture.ID)
	if err != nil || !listedCapture.Resumable {
		t.Fatalf("completed capture resumable=%t err=%v, want true", listedCapture.Resumable, err)
	}
	resumable := true
	listed, err := env.service.List(ctx, HistoryCaptureListOptions{Resumable: &resumable, Limit: 10})
	if err != nil || len(listed) != 1 || listed[0].ID != capture.ID {
		t.Fatalf("resumable discovery = %+v, err=%v", listed, err)
	}
	availability, err := env.service.CountAvailability(ctx, HistoryCaptureListOptions{})
	if err != nil || availability.Resumable != 1 {
		t.Fatalf("resumable availability = %+v, err=%v", availability, err)
	}

	input := CreateHistoryResumeInput{
		SourceCaptureID: capture.ID, NativeSessionID: "native-child",
		IdempotencyKey: "resume-once", RequestedBy: "owner:test",
	}
	if _, err := env.store.DB().ExecContext(ctx, `
CREATE TRIGGER abort_history_resume_insert
BEFORE INSERT ON history_resumes
BEGIN
    SELECT RAISE(ABORT, 'injected resume insert failure');
END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.service.CreateResume(ctx, queue, project, input); err == nil || !strings.Contains(err.Error(), "injected resume insert failure") {
		t.Fatalf("injected resume failure = %v", err)
	}
	var orphanJobs int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE dispatch_key LIKE 'history-resume:%'`).Scan(&orphanJobs); err != nil {
		t.Fatal(err)
	}
	if orphanJobs != 0 {
		t.Fatalf("orphaned resume jobs after lineage failure = %d, want 0", orphanJobs)
	}
	if _, err := env.store.DB().ExecContext(ctx, `DROP TRIGGER abort_history_resume_insert`); err != nil {
		t.Fatal(err)
	}

	resume, created, err := env.service.CreateResume(ctx, queue, project, input)
	if err != nil {
		t.Fatalf("create resume: %v", err)
	}
	if !created || resume.SourceNativeSessionID != "native-child" || resume.HarnessArtifactID != harnessArtifact.ID ||
		resume.WorkspaceArtifactID != workspaceArtifact.ID || resume.RequiredHeadCommit != head || resume.SourceHarnessBuild != "0.4.5" ||
		resume.RequiredHarnessSchema != 5 || resume.State != "queued" {
		t.Fatalf("resume = %+v, created=%t", resume, created)
	}
	job, err := queue.GetJob(ctx, resume.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Selector) != 0 {
		t.Fatalf("history resume selector = %#v, want no scheduling requirements", job.Selector)
	}
	raw, ok := job.Payload["history_resume"].(map[string]any)
	if !ok {
		t.Fatalf("history resume payload = %#v", job.Payload["history_resume"])
	}
	for key, want := range map[string]string{
		"id": resume.ID, "source_capture_id": capture.ID, "native_session_id": "native-child",
		"session_relative_dir": "children/native-child", "harness_artifact_id": harnessArtifact.ID,
		"workspace_artifact_id": workspaceArtifact.ID, "required_head_commit": head,
		"source_harness_build": "0.4.5",
	} {
		if got := raw[key]; got != want {
			t.Fatalf("history_resume[%q] = %#v, want %q", key, got, want)
		}
	}
	if inject, ok := job.Payload["inject_initial_prompt"].(bool); !ok || inject {
		t.Fatalf("inject_initial_prompt = %#v, want false", job.Payload["inject_initial_prompt"])
	}

	retried, retryCreated, err := env.service.CreateResume(ctx, queue, project, input)
	if err != nil || retryCreated || retried.ID != resume.ID || retried.JobID != resume.JobID {
		t.Fatalf("idempotent retry = %+v created=%t err=%v", retried, retryCreated, err)
	}
	omittedRetry := input
	omittedRetry.NativeSessionID = ""
	if _, _, err := env.service.CreateResume(ctx, queue, project, omittedRetry); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("retry omitting explicit native session error = %v, want conflict", err)
	}
	var resumeRows int
	if err := env.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM history_resumes WHERE source_capture_id = ?`, capture.ID).Scan(&resumeRows); err != nil || resumeRows != 1 {
		t.Fatalf("history resume row count = %d, err=%v", resumeRows, err)
	}
	for _, state := range []flowworker.JobState{flowworker.JobClaimed, flowworker.JobRunning, flowworker.JobFinished} {
		if _, err := env.store.DB().ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = ? WHERE id = ?`, state, time.Now().UTC().Format(time.RFC3339Nano), resume.JobID); err != nil {
			t.Fatalf("transition resume job to %s: %v", state, err)
		}
	}
	var state string
	var claimedAt, runningAt, completedAt any
	if err := env.store.DB().QueryRowContext(ctx, `
SELECT state, claimed_at, running_at, completed_at FROM history_resumes WHERE id = ?`, resume.ID).
		Scan(&state, &claimedAt, &runningAt, &completedAt); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || claimedAt == nil || runningAt == nil || completedAt == nil {
		t.Fatalf("tracked resume state = %q timestamps=%v/%v/%v", state, claimedAt, runningAt, completedAt)
	}

	// Discovery must stop advertising the capture when a coordinator-derived
	// restore precondition drifts, matching a new CreateResume request.
	if _, err := env.store.DB().ExecContext(ctx, `UPDATE changes SET head_sha = ? WHERE id = ?`, strings.Repeat("c", 40), "ch-history-resume"); err != nil {
		t.Fatal(err)
	}
	drifted, err := env.service.Get(ctx, capture.ID)
	if err != nil || drifted.Resumable {
		t.Fatalf("capture after head drift resumable=%t err=%v, want false", drifted.Resumable, err)
	}
	listed, err = env.service.List(ctx, HistoryCaptureListOptions{Resumable: &resumable, Limit: 10})
	if err != nil || len(listed) != 0 {
		t.Fatalf("resumable discovery after head drift = %+v, err=%v", listed, err)
	}
	availability, err = env.service.CountAvailability(ctx, HistoryCaptureListOptions{})
	if err != nil || availability.Resumable != 0 {
		t.Fatalf("availability after head drift = %+v, err=%v", availability, err)
	}
}

func stringPointer(value string) *string { return &value }
