# Change Summary

## Current Goal

Serialize review submission against a concurrent change-head advance (t-flow-0085):
make the head read and the thread/check writes of `handleSubmitReview` atomic so a
concurrent head advance cannot anchor threads or a verdict to a head that is stale
at write time, while preserving the 409 `head_moved` fail-closed behavior and
matching-head behavior.

## Completed Work

The requested change is **already implemented in the base** (`main` @ `1cc5710`):
the t-flow-0064 change's final form included the coordinator-side atomic fix
(commit `417d424` "fix(api): atomically bind review submissions to the displayed
change head"), and that merge landed before this task's branch was cut. No further
code change was needed; this node verified the behavior end to end.

Mechanism already in place:

- `internal/api/change_review_handlers.go` no longer does the `GetChange`-then-write
  shape described in the task. It delegates the whole submission to
  `ThreadService.SubmitReview` and maps `coordinator.ErrReviewHeadMoved` to
  409 `head_moved` (nothing written on mismatch).
- `internal/coordinator/reviews.go` `SubmitReview` re-reads `changes.head_sha`
  **inside** the same `BEGIN IMMEDIATE` transaction (`sqlitex.BeginImmediate`) that
  creates the inline threads (`createThreadTx`), reports the `human-review` check
  (`reportCheckTx`), and completes the verdict's workflow gate
  (`respondToReviewGateTx`), then commits. A concurrent head advance can therefore
  never interleave between the comparison and the writes: either the submission
  commits for the inspected head first, or the advance lands first and the whole
  submission is refused with `ErrReviewHeadMoved` (409, no partial state).
- Production DB busy timeout is 5000 ms (`internal/db/db.go`), so a concurrent
  advance waits for the review transaction rather than failing mid-flight.

## Remaining Work

None. All four acceptance criteria are satisfied by the existing implementation and
covered by existing focused API tests.

## Tests Run and Results

- `go test ./internal/api -run 'TestSubmitReview' -count=1 -v` — PASS (6/6)
  - `TestSubmitReviewHeadUpdateCannotInterleave`: the focused concurrent-advance
    test. `AfterHeadCheck` fires inside the review transaction right after the head
    comparison; a racing `UPDATE changes SET head_sha` over a second connection with
    `busy_timeout 0` must fail immediately (SQLITE_BUSY). Asserts no threads/checks
    are created, the head is unchanged, and the run stays waiting.
  - `TestSubmitReviewStaleHeadConflicts`: genuine inspect-then-submit mismatch
    returns 409 and creates nothing.
  - `TestSubmitReviewApprovalAdvancesHumanGate`,
    `TestSubmitReviewCommentLeavesHumanGateWaiting`,
    `TestSubmitReviewCommentDoesNotStartScheduledRun`: matching-head comment and
    verdict behavior preserved.
  - `TestSubmitReviewRejectsWrongCredentialsAndState`.
- `go test ./internal/api -count=1` — PASS
- `go test ./internal/coordinator -count=1` — PASS

## Failed Approaches

None.

## Important Files and Commands

- `internal/coordinator/reviews.go` — `SubmitReview` (atomic transaction, head
  re-check at lines 321-333, `AfterHeadCheck` test seam at 334-338).
- `internal/api/change_review_handlers.go` — `handleSubmitReview` (409 `head_moved`
  mapping at lines 105-107).
- `internal/api/change_review_handlers_test.go` — race/stale/matching-head tests.
- Commands: `go test ./internal/api -run TestSubmitReview -count=1 -v`;
  `go test ./internal/api -count=1`; `go test ./internal/coordinator -count=1`.

## Next Recommended Action

Close the node: the change requested by t-flow-0085 landed with ch-flow-0064
(commit `417d424`) and is verified by the passing focused API tests.
