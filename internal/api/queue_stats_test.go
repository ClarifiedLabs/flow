package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowworker "github.com/ClarifiedLabs/flow/internal/worker"
)

func TestQueueStatsScopeAndCounts(t *testing.T) {
	fixture := newTestFixture(t)
	ctx := context.Background()

	if err := fixture.Credentials.EnsureToken(ctx, coordinator.CredentialInput{
		Token: "orchestrator-token",
		Scope: coordinator.TokenScopeOrchestrator,
	}); err != nil {
		t.Fatalf("store orchestrator token: %v", err)
	}

	task, err := fixture.Tasks.CreateTask(ctx, coordinator.CreateTaskInput{Title: "Queue stats task"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	var enqueue jobResponse
	doJSONRequestAs(t, fixture.Server, "owner-token", http.MethodPost, "/v2/jobs", enqueueJobRequest{
		TaskID:         &task.ID,
		Role:           string(flowworker.RoleCI),
		CapacityBucket: string(flowworker.BucketEphemeral),
		Payload:        map[string]any{"entrypoint": "make test"},
	}, http.StatusCreated, &enqueue)

	// Worker and hook tokens are not authorized for queue stats.
	doJSONRequestAs(t, fixture.Server, "worker-token", http.MethodGet, "/v2/queue/stats", nil, http.StatusForbidden, nil)
	doJSONRequestAs(t, fixture.Server, "hook-token", http.MethodGet, "/v2/queue/stats", nil, http.StatusForbidden, nil)

	for _, token := range []string{"orchestrator-token", "owner-token"} {
		var stats queueStatsResponse
		doJSONRequestAs(t, fixture.Server, token, http.MethodGet, "/v2/queue/stats", nil, http.StatusOK, &stats)
		if stats.Queued != 1 {
			t.Fatalf("%s queued = %d, want 1", token, stats.Queued)
		}
		if stats.ClaimedOrRunning != 0 {
			t.Fatalf("%s claimed_or_running = %d, want 0", token, stats.ClaimedOrRunning)
		}
		if stats.ByBucket[string(flowworker.BucketEphemeral)] != 1 {
			t.Fatalf("%s by_bucket = %+v", token, stats.ByBucket)
		}
	}
}
