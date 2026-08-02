package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

func TestTaskFindingsRegistryEndpoint(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Findings endpoint target"}, http.StatusCreated, &created)
	taskID := created.Task.ID

	const (
		oldChange = "ch-findings-old"
		newChange = "ch-findings-new"
		oldHead   = "1111111111111111111111111111111111111111"
		newHead   = "2222222222222222222222222222222222222222"
		timestamp = "2026-01-01T00:00:00.000000000Z"
	)
	for _, change := range []struct {
		id     string
		branch string
		head   string
	}{
		{oldChange, "task/findings-old", oldHead},
		{newChange, "task/findings-new", newHead},
	} {
		if _, err := fixture.DB.Exec(`INSERT INTO changes (id, task_id, branch, base, head_sha, created_at, updated_at, ready_at) VALUES (?, ?, ?, 'main', ?, ?, ?, ?)`,
			change.id, taskID, change.branch, change.head, timestamp, timestamp, timestamp); err != nil {
			t.Fatalf("insert change %s: %v", change.id, err)
		}
	}

	createThread := func(changeID string, file string, line int, body string) string {
		t.Helper()
		var response threadResponse
		doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/changes/"+changeID+"/comments",
			createThreadRequest{AnchorCommitSHA: oldHead, FilePath: file, Line: line, Body: body},
			http.StatusCreated, &response)
		return response.Thread.ID
	}
	// One finding on the older change, resolved fixed and certified; one open
	// finding on the newer change; one not_warranted finding on the older
	// change — so the registry must span both review rounds.
	certifiedID := createThread(oldChange, "a.go", 1, "old round: buffer overflow")
	notWarrantedID := createThread(oldChange, "a.go", 2, "old round: naming nit")
	openID := createThread(newChange, "b.go", 1, "new round: missing nil check")

	var thread threadResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/threads/"+certifiedID+"/claims",
		threadClaimRequest{Kind: "fixed", ClaimCommitSHA: newHead}, http.StatusOK, &thread)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/threads/"+certifiedID+"/certify",
		threadCommentRequest{Body: "verified"}, http.StatusOK, &thread)
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/threads/"+notWarrantedID+"/claims",
		threadClaimRequest{Kind: "not_warranted", Body: "style only"}, http.StatusOK, &thread)

	// Deferred follow-ups through the real write path (the worker lease check
	// lives in the HTTP handler; the service call is the same transaction).
	existing, err := fixture.Bundle.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Existing follow-up target"})
	if err != nil {
		t.Fatalf("create existing follow-up task: %v", err)
	}
	deferred, err := fixture.Bundle.Tasks.ApplyReviewFollowUp(ctx, coordinator.ApplyReviewFollowUpInput{
		SourceTaskID:   taskID,
		SourceChangeID: oldChange,
		CheckName:      "review-aggregator",
		Finding: coordinator.ReviewFollowUpFinding{
			SHA: oldHead, File: "c.go", Line: 5, Body: "defer cleanup",
			Severity: "medium", Requirement: "req",
		},
		TaskAction: coordinator.ReviewFollowUpTaskAction{Action: coordinator.ReviewFollowUpCreateTask, Title: "Deferred cleanup task", Body: "Clean up."},
	})
	if err != nil {
		t.Fatalf("apply create_task follow-up: %v", err)
	}
	if _, err := fixture.Bundle.Tasks.ApplyReviewFollowUp(ctx, coordinator.ApplyReviewFollowUpInput{
		SourceTaskID:   taskID,
		SourceChangeID: newChange,
		CheckName:      "review-aggregator",
		Finding: coordinator.ReviewFollowUpFinding{
			SHA: newHead, File: "d.go", Line: 7, Body: "audit path",
			Severity: "low", Requirement: "req",
		},
		TaskAction: coordinator.ReviewFollowUpTaskAction{Action: coordinator.ReviewFollowUpUseExistingTask, TaskID: existing.ID},
	}); err != nil {
		t.Fatalf("apply use_existing_task follow-up: %v", err)
	}

	var registry contract.TaskFindingsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+taskID+"/findings",
		nil, http.StatusOK, &registry)

	if registry.TaskID != taskID {
		t.Fatalf("registry task id = %q, want %q", registry.TaskID, taskID)
	}
	if len(registry.Findings) != 3 {
		t.Fatalf("registry findings = %d, want 3: %+v", len(registry.Findings), registry.Findings)
	}
	changesSeen := map[string]bool{}
	for _, finding := range registry.Findings {
		changesSeen[finding.ChangeID] = true
		if finding.ID == certifiedID {
			if finding.State != coordinator.ThreadCertified || finding.ClaimKind == nil || *finding.ClaimKind != coordinator.ClaimFixed {
				t.Fatalf("certified finding = %+v, want certified fixed", finding)
			}
			if finding.Finding != "old round: buffer overflow" {
				t.Fatalf("certified finding body = %q, want the opening comment", finding.Finding)
			}
		}
		if finding.ID == notWarrantedID && (finding.State != coordinator.ThreadClaimed || finding.ClaimKind == nil || *finding.ClaimKind != coordinator.ClaimNotWarranted) {
			t.Fatalf("not_warranted finding = %+v, want claimed not_warranted", finding)
		}
		if finding.ID == openID && finding.State != coordinator.ThreadOpen {
			t.Fatalf("open finding = %+v, want open", finding)
		}
	}
	// Threads from the older, non-latest change must be included.
	if !changesSeen[oldChange] || !changesSeen[newChange] {
		t.Fatalf("registry changes = %v, want both %s and %s", changesSeen, oldChange, newChange)
	}

	if len(registry.FollowUps) != 2 {
		t.Fatalf("registry follow-ups = %d, want 2: %+v", len(registry.FollowUps), registry.FollowUps)
	}
	for _, followUp := range registry.FollowUps {
		switch followUp.Action {
		case coordinator.ReviewFollowUpCreateTask:
			if followUp.TargetTaskID != deferred.Task.ID || followUp.TargetTaskTitle != "Deferred cleanup task" {
				t.Fatalf("create_task follow-up = %+v, want target %s titled %q", followUp, deferred.Task.ID, "Deferred cleanup task")
			}
		case coordinator.ReviewFollowUpUseExistingTask:
			if followUp.TargetTaskID != existing.ID || followUp.TargetTaskTitle != "Existing follow-up target" {
				t.Fatalf("use_existing_task follow-up = %+v, want target %s titled %q", followUp, existing.ID, "Existing follow-up target")
			}
		default:
			t.Fatalf("unexpected follow-up action %q", followUp.Action)
		}
		if followUp.CheckName != "review-aggregator" || followUp.FindingHash == "" || followUp.CreatedAt.IsZero() {
			t.Fatalf("follow-up provenance = %+v, want check review-aggregator with hash and timestamp", followUp)
		}
	}

	wantSummary := coordinator.TaskFindingsSummary{
		ResolvedNotWarranted: 1,
		Certified:            1,
		Unresolved:           1,
		DeferredToTask:       2,
	}
	if registry.Summary != wantSummary {
		t.Fatalf("registry summary = %+v, want %+v", registry.Summary, wantSummary)
	}

	// Worker scope is not a findings reader.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/tasks/"+taskID+"/findings",
		nil, http.StatusForbidden, nil)

	// Unknown task id → 404 through task routing.
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/t-does-not-exist/findings",
		nil, http.StatusNotFound, nil)
}

func TestTaskFindingsRegistryEndpointEmptyTask(t *testing.T) {
	fixture := newTestFixture(t)

	var created taskResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/tasks",
		createTaskRequest{Title: "Findings-free target"}, http.StatusCreated, &created)

	var registry contract.TaskFindingsResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodGet, "/v2/tasks/"+created.Task.ID+"/findings",
		nil, http.StatusOK, &registry)
	if len(registry.Findings) != 0 || len(registry.FollowUps) != 0 {
		t.Fatalf("empty registry = %+v, want no findings and no follow-ups", registry)
	}
	if registry.Summary != (coordinator.TaskFindingsSummary{}) {
		t.Fatalf("empty registry summary = %+v, want all zeros", registry.Summary)
	}
}
