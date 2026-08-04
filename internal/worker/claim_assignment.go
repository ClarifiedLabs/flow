package worker

// ProjectClaim is a claimed job together with the project whose database holds
// the job and lease rows. Lease operations (renew, release, mark running) must
// be routed back to that project's queue.
type ProjectClaim struct {
	ProjectID string
	Job       Job
	Lease     Lease
}
