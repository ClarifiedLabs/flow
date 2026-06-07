package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	flowdb "github.com/ClarifiedLabs/flow/internal/db"
)

type claimFixture struct {
	directory *Directory
	queues    []ProjectQueue
}

func newClaimFixture(t *testing.T, projectIDs ...string) claimFixture {
	t.Helper()
	ctx := context.Background()

	global, err := flowdb.OpenGlobal(ctx, filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { _ = global.Close() })

	fixture := claimFixture{directory: NewDirectory(global.DB())}
	for _, projectID := range projectIDs {
		store, err := flowdb.Open(ctx, filepath.Join(t.TempDir(), projectID+".db"))
		if err != nil {
			t.Fatalf("open project db %s: %v", projectID, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		fixture.queues = append(fixture.queues, ProjectQueue{
			ProjectID: projectID,
			Queue:     NewService(store.DB()),
		})
	}

	return fixture
}

func (f claimFixture) registerWorker(t *testing.T, persistent int, ephemeral int) {
	t.Helper()

	if _, err := f.directory.RegisterWorker(context.Background(), RegisterWorkerInput{
		ID:                      "w-local",
		Labels:                  map[string]string{"gpu": "true"},
		CapacityPersistentAgent: persistent,
		CapacityEphemeral:       ephemeral,
		HeartbeatTTL:            time.Minute,
	}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
}

func (f claimFixture) enqueueCI(t *testing.T, projectIndex int) Job {
	t.Helper()

	job, err := f.queues[projectIndex].Queue.EnqueueJob(context.Background(), EnqueueJobInput{
		Role:           RoleCI,
		CapacityBucket: BucketEphemeral,
	})
	if err != nil {
		t.Fatalf("enqueue ci job in project %d: %v", projectIndex, err)
	}

	return job
}

func (f claimFixture) claim(t *testing.T) (ProjectClaim, bool) {
	t.Helper()

	claim, ok, err := ClaimAcrossProjects(context.Background(), f.directory, f.queues, ClaimInput{
		WorkerID:      "w-local",
		Buckets:       []CapacityBucket{BucketEphemeral},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim across projects: %v", err)
	}

	return claim, ok
}

func TestClaimAcrossProjectsPrefersOldestQueuedJob(t *testing.T) {
	fixture := newClaimFixture(t, "p-aaaa", "p-bbbb")
	fixture.registerWorker(t, 0, 2)

	older := fixture.enqueueCI(t, 0)
	time.Sleep(5 * time.Millisecond)
	newer := fixture.enqueueCI(t, 1)

	first, ok := fixture.claim(t)
	if !ok {
		t.Fatal("first claim should succeed")
	}
	if first.ProjectID != "p-aaaa" || first.Job.ID != older.ID {
		t.Fatalf("first claim = %s/%s, want p-aaaa/%s", first.ProjectID, first.Job.ID, older.ID)
	}

	second, ok := fixture.claim(t)
	if !ok {
		t.Fatal("second claim should succeed")
	}
	if second.ProjectID != "p-bbbb" || second.Job.ID != newer.ID {
		t.Fatalf("second claim = %s/%s, want p-bbbb/%s", second.ProjectID, second.Job.ID, newer.ID)
	}
}

func TestClaimAcrossProjectsAggregatesCapacityAcrossDatabases(t *testing.T) {
	fixture := newClaimFixture(t, "p-aaaa", "p-bbbb")
	fixture.registerWorker(t, 0, 1)

	fixture.enqueueCI(t, 0)
	fixture.enqueueCI(t, 1)

	if _, ok := fixture.claim(t); !ok {
		t.Fatal("first claim should succeed")
	}

	// The single ephemeral slot is occupied by a lease in project A's
	// database; project B's queued job must not be claimable even though
	// project B's database holds no leases.
	if claim, ok := fixture.claim(t); ok {
		t.Fatalf("second claim should be capacity-blocked, got %s/%s", claim.ProjectID, claim.Job.ID)
	}
}

func TestClaimAcrossProjectsRoutesLeaseToOwningProject(t *testing.T) {
	fixture := newClaimFixture(t, "p-aaaa", "p-bbbb")
	fixture.registerWorker(t, 0, 1)

	fixture.enqueueCI(t, 1)

	claim, ok := fixture.claim(t)
	if !ok {
		t.Fatal("claim should succeed")
	}
	if claim.ProjectID != "p-bbbb" {
		t.Fatalf("claim project = %s, want p-bbbb", claim.ProjectID)
	}

	ctx := context.Background()
	if _, err := fixture.queues[1].Queue.GetLease(ctx, claim.Lease.ID); err != nil {
		t.Fatalf("lease should live in project B's database: %v", err)
	}
	if _, err := fixture.queues[0].Queue.GetLease(ctx, claim.Lease.ID); err == nil {
		t.Fatal("lease should not exist in project A's database")
	}

	if _, err := fixture.queues[1].Queue.MarkJobRunning(ctx, claim.Lease.ID); err != nil {
		t.Fatalf("mark job running via owning project: %v", err)
	}
}

func TestClaimAcrossProjectsWithNoQueuedJobs(t *testing.T) {
	fixture := newClaimFixture(t, "p-aaaa")
	fixture.registerWorker(t, 1, 1)

	if claim, ok := fixture.claim(t); ok {
		t.Fatalf("claim should find nothing, got %s/%s", claim.ProjectID, claim.Job.ID)
	}
}
