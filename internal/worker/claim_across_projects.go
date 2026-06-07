package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ClarifiedLabs/flow/internal/scheduler"
)

// ProjectQueue pairs a project with its job/lease queue service.
type ProjectQueue struct {
	ProjectID string
	Queue     *Service
}

// ProjectClaim is a claimed job together with the project whose database
// holds the job and lease rows. Lease operations (renew, release, mark
// running) must be routed back to that project's queue.
type ProjectClaim struct {
	ProjectID string
	Job       Job
	Lease     Lease
}

// ClaimAcrossProjects claims the next eligible job for a worker across every
// project queue. Worker capacity is enforced against the sum of live leases
// in all project databases; no transaction spans databases, so callers must
// serialize claims for correctness (the coordinator holds an in-process
// claim mutex).
//
// Queues are tried in order of their oldest queued job so that no project is
// starved while another has a deep backlog.
func ClaimAcrossProjects(ctx context.Context, directory *Directory, queues []ProjectQueue, input ClaimInput) (ProjectClaim, bool, error) {
	input.WorkerID = strings.TrimSpace(input.WorkerID)
	if input.WorkerID == "" {
		return ProjectClaim{}, false, errors.New("worker id is required")
	}
	if input.LeaseDuration <= 0 {
		return ProjectClaim{}, false, errors.New("lease duration must be positive")
	}
	if len(input.Buckets) == 0 {
		input.Buckets = []CapacityBucket{BucketPersistentAgent, BucketEphemeral}
	}
	for _, bucket := range input.Buckets {
		if err := validateCapacityBucket(bucket); err != nil {
			return ProjectClaim{}, false, err
		}
	}

	worker, err := directory.GetWorker(ctx, input.WorkerID)
	if err != nil {
		return ProjectClaim{}, false, err
	}

	var used scheduler.Capacity
	for _, queue := range queues {
		queueUsed, err := queue.Queue.UsedCapacity(ctx, worker.ID)
		if err != nil {
			return ProjectClaim{}, false, fmt.Errorf("aggregate used capacity for project %s: %w", queue.ProjectID, err)
		}
		used.PersistentAgent += queueUsed.PersistentAgent
		used.Ephemeral += queueUsed.Ephemeral
	}

	var buckets []CapacityBucket
	for _, bucket := range input.Buckets {
		if worker.capacityFor(bucket) > usedFor(used, bucket) {
			buckets = append(buckets, bucket)
		}
	}
	if len(buckets) == 0 {
		return ProjectClaim{}, false, nil
	}

	ordered, err := orderQueuesByOldestQueued(ctx, queues, buckets)
	if err != nil {
		return ProjectClaim{}, false, err
	}

	for _, queue := range ordered {
		claimed, ok, err := queue.Queue.claimQueuedJob(ctx, worker, buckets, used, input.LeaseDuration)
		if err != nil {
			return ProjectClaim{}, false, fmt.Errorf("claim in project %s: %w", queue.ProjectID, err)
		}
		if ok {
			return ProjectClaim{
				ProjectID: queue.ProjectID,
				Job:       claimed.Job,
				Lease:     claimed.Lease,
			}, true, nil
		}
	}

	return ProjectClaim{}, false, nil
}

func usedFor(used scheduler.Capacity, bucket CapacityBucket) int {
	switch bucket {
	case BucketPersistentAgent:
		return used.PersistentAgent
	case BucketEphemeral:
		return used.Ephemeral
	default:
		return 0
	}
}

type queueOrder struct {
	queue        ProjectQueue
	oldestQueued *time.Time
}

func orderQueuesByOldestQueued(ctx context.Context, queues []ProjectQueue, buckets []CapacityBucket) ([]ProjectQueue, error) {
	orders := make([]queueOrder, 0, len(queues))
	for _, queue := range queues {
		oldest, err := queue.Queue.OldestQueuedAt(ctx, buckets)
		if err != nil {
			return nil, fmt.Errorf("read oldest queued job for project %s: %w", queue.ProjectID, err)
		}
		if oldest == nil {
			continue
		}
		orders = append(orders, queueOrder{queue: queue, oldestQueued: oldest})
	}

	sort.SliceStable(orders, func(i, j int) bool {
		return orders[i].oldestQueued.Before(*orders[j].oldestQueued)
	})

	ordered := make([]ProjectQueue, 0, len(orders))
	for _, order := range orders {
		ordered = append(ordered, order.queue)
	}

	return ordered, nil
}
