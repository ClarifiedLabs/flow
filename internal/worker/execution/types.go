package execution

import flowworker "github.com/ClarifiedLabs/flow/internal/worker"

type Job = flowworker.Job
type JobRole = flowworker.JobRole
type JobState = flowworker.JobState
type Lease = flowworker.Lease
type CapacityBucket = flowworker.CapacityBucket

const (
	RoleAuthor   = flowworker.RoleAuthor
	RoleReviewer = flowworker.RoleReviewer
	RoleVerifier = flowworker.RoleVerifier
	RoleCI       = flowworker.RoleCI
	RoleConsole  = flowworker.RoleConsole

	JobQueued   = flowworker.JobQueued
	JobClaimed  = flowworker.JobClaimed
	JobRunning  = flowworker.JobRunning
	JobFinished = flowworker.JobFinished
	JobFailed   = flowworker.JobFailed
	JobCrashed  = flowworker.JobCrashed
	JobCanceled = flowworker.JobCanceled

	BucketPersistentAgent = flowworker.BucketPersistentAgent
	BucketEphemeral       = flowworker.BucketEphemeral
)

func IsTerminalJobState(state JobState) bool {
	return flowworker.IsTerminalJobState(state)
}
