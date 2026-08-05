# Full-fidelity execution history

Flow captures durable evidence for every worker execution attempt. History is an
evidence and recovery facility, not a general backup of the worker host. This
document describes the implemented capture boundary, owner operations, recovery
rules, and storage requirements.

For deployment details, see [Kubernetes deployment](kubernetes.md). For the
worker and coordinator configuration context, see [Setup](setup.md).

## Capture invariant and identity

Final history capture is mandatory. `flow-worker` rejects
`history.mandatory_final_capture: false`; there is no supported legacy or
best-effort execution path. Before user code starts, the worker reserves a
capture and durably records the reservation in its outbox. It pauses new claims
until startup/replay of that outbox succeeds. At the end of execution it records
the execution verdict, stages the final bytes, and uses the idempotent publication
protocol to complete the capture. A publication failure therefore remains
pending for replay instead of silently turning into an uncaptured execution.

A **capture is one lease attempt of one job**, not one task:

- A task can create several workflow jobs (author, reviewer, verifier, CI, or
  console), and each job has its own captures.
- If a job is leased again, the new lease is a new capture with a new
  `lease_attempt`. Earlier attempts remain immutable.
- Task, change, session, workflow-run, node-run/visit, stage, role, job, lease,
  and worker attribution are copied from coordinator-owned job and lease data.
  Producers cannot use history to retarget an attempt.
- The execution verdict (`succeeded`, `failed`, `cancelled`, or `crashed`) is
  independent of capture state. For example, a failed execution can have a
  `complete` capture: the failure evidence was captured successfully.

The normal lifecycle is `reserved` → `running` → `quiescing` → `sealed` →
`uploading` → `complete`. `blocked` and `lost` describe interrupted publication,
while `waived` is an explicit terminal owner decision. State changes, upload
grant rotation/revocation, waivers, and resume requests are audit events and use
optimistic capture versions.

## Artifact boundary

A complete capture is an immutable set closed by a declared expected set, a
sealed transcript, and a coordinator-generated canonical manifest. Blob content
is content-addressed and publication is a pending/committed operation; producer
bytes never supply the canonical final manifest.

The final set can contain:

- **Transcript segments.** The worker segments the attempt transcript and records
  ordering, offsets, logical length, and a whole-stream SHA-256 seal. The
  transcript can contain prompts, tool output, subprocess output, errors, and
  other text visible during execution.
- **Harness archive.** For native Harness jobs, Flow archives exactly the
  Flow-managed output session root, including indexed root and delegated-child
  `state.json` members. It does not inspect sibling session directories or a
  user-wide Harness store, and it does not follow filesystem links. Everything
  under that managed root is in scope; there is no generic cache exclusion
  inside the root.
- **Workspace snapshot.** Flow stores the HEAD/base identity, index inventory,
  staged and unstaged binary patches, and eligible untracked files. It does not
  embed the repository's `.git` directory or a second copy of committed Git
  objects. Untracked discovery uses `git ls-files --others --exclude-standard`,
  so ignored files and ordinary ignored build/dependency caches are omitted.
  `.flow/session`, `.flow/attachments`, and `.flow/history` are also excluded.
  Restoration starts from the coordinator-selected repository/head and applies
  this reconstructive state.
- **Manifest.** The coordinator constructs the final manifest from committed
  artifact metadata, transcript seal, Harness member index, workspace summary,
  attribution, and execution verdict.

Optional live checkpoint artifacts use separate generations and do not replace
the required final set. Limits on stored bytes, logical bytes, individual files,
entry count, and path length apply during capture, inspection, and restore.
Unsupported sparse checkouts, unsafe paths/links, special files, malformed
archives, or digest mismatches fail capture/restore rather than weakening the
boundary.

## Sensitivity and authorization

Treat every capture and export as **private, potentially secret-bearing data**.
There is currently no public artifact class and no per-artifact sensitivity label.
Owner reads—including list/detail, events, manifests, and artifact content—require
an owner-scoped token. Owner projections omit blob keys, temporary upload IDs,
internal filesystem paths, and upload grants. Artifact responses are marked
`Cache-Control: private` but that is not an access-control boundary.

Worker publication has two checks. Reservation requires the authenticated worker
to hold the active matching job lease. Subsequent publication requires that same
worker identity plus the random, revocable, capture-specific upload grant. The
grant is returned only on reservation (or an exactly attributed retry that
rotates it), is stored in the private worker outbox, and is never exposed by owner
reads. This permits final publication after lease expiry without granting access
to other captures.

Resume artifact reads are narrower still: the authenticated worker must present
an unexpired, unreleased lease for the exact coordinator-created resume job, and
the requested artifact must be one of the Harness/workspace artifacts selected
for that resume. A worker token or an unrelated active lease is insufficient.

The archive writers reject bytes matching the credentials Flow knows at capture
time (worker, session, and model-proxy credentials, including during restart
recovery). This is a guardrail, **not general secret discovery or redaction**.
A user secret pasted into a prompt, emitted by a tool/model, written under the
managed Harness root, or placed in an eligible Git change/untracked file can be
stored in history. Keep secrets out of prompts and output where possible, apply
owner-token and blob-store controls accordingly, and include history in retention,
incident-response, and data-classification policy.

## Listing and time windows

The owner list operation filters on repeated `task_id`, `job_id`, `session_id`,
`capture_id`, and `state` values, plus `resumable`, `since`, and `until`. Repeated
values of one field are alternatives; different fields narrow the result. Results
are newest first and use a snapshot-stable keyset cursor. The first page chooses
`now` as `snapshot_until` unless `until` is supplied; later pages must reuse the
returned cursor and unchanged filters. Limits are 1–200 (default 50).

Time filters apply to `reserved_at` as a half-open interval:

```text
since <= reserved_at < until
```

For example:

```text
since=2026-08-03T10:00:00Z&until=2026-08-03T11:00:00Z
since=2026-08-03T11:00:00Z&until=2026-08-03T12:00:00Z
```

A capture reserved exactly at 11:00 appears only in the second window. Adjacent
windows therefore neither duplicate nor omit a boundary capture. `since` alone
is capped by the server-selected snapshot time; `until` must be strictly after
`since`. Availability totals (`complete`, `resumable`, `blocked`, `lost`, and
`waived`) are computed over the same snapshot and filters, before page limiting.
A task filter returns every matching job and lease attempt; use `job_id` to narrow
to one job and the returned capture/lease attempt fields to distinguish retries.

## Owner operations and CLI

The owner API, Go client, and `flow history` commands can list captures, export
complete evidence, and request native resume jobs:

```text
flow history list --since 2026-08-03T00:00:00Z --state complete
flow history list --task-id t-123 --format json
flow history export --capture-id hc-... --output ./private-history
flow history export --all --output ./private-history-all --allow-incomplete
flow history resume hc-... [--native-session SESSION_ID]
```

`list` supports the filters described above and emits a newest-first table by
default. `--format json` preserves the complete API response, continuation cursor,
and availability totals.

`export` requires at least one selector unless `--all` is explicit. It walks every
cursor page, downloads each complete capture's detail, events, canonical manifest,
and committed artifacts, and writes a deterministic uncompressed tar bundle. It
verifies every artifact's advertised byte length and SHA-256 before installing the
bundle. Incomplete captures remain in `index.json` as unavailable; without
`--allow-incomplete` they make the command exit nonzero. Hard download or
validation failures always exit nonzero.

The output directory and files must be owner-only. Flow creates and marks a new
private directory, rejects unmarked, public, symlinked, or unrelated existing
content, and records `export-descriptor.json` with the server, project, normalized
selection, and frozen `snapshot_until`. A rerun must match that descriptor; matching
bundles are verified and reused, while differing bundle files are never overwritten.
`SHA256SUMS` covers the descriptor, index, and available bundles.

`resume` accepts only a capture ID and optional already-indexed native session. It
never accepts artifact paths, blob keys, targets, HEADs, or compatibility
overrides. If `--idempotency-key` is omitted, the CLI generates a fresh random key;
operators retrying an uncertain request should explicitly reuse the same key.
Changing the selected native session on such a retry is a conflict.

## Validated resume policy

Resume is deliberately exact rather than a best-effort import. The owner selects
a completed, resumable native Harness capture and optionally one already indexed
native session. Omitting the session selects the unique root member. The
coordinator, not the caller, derives and durably records:

- the source capture, selected parsed native member, and lineage;
- exactly one committed final Harness archive and one committed final workspace
  archive, with their artifact IDs and SHA-256 values;
- the source task/change target, source managed entrypoint, branch/base, current
  change head, and required captured workspace HEAD;
- the source Harness build string as provenance and the required Harness native
  schema version; and
- the queued author job and its selector.

Creation fails if the source is not complete native Harness history, the member
is absent/ambiguous/unparsed, archive metadata is invalid, the task is done, or
the change head no longer equals the captured workspace HEAD. The resume job does
not accept caller-supplied target or lineage coordinates and does not inject a new
initial prompt.

Before mutating its checkout, the selected worker downloads both archives through
the active-lease authorization described above, enforces byte ceilings, verifies
the coordinator-pinned SHA-256 values, and fully inspects both archives. The
required native schema must equal the worker's supported schema (currently
schema 5), but the installed Harness build may differ from the source build.
This deliberately permits a newer Harness to recover state produced by an older
buggy build without treating executable identity as serialization compatibility.
Flow performs no schema migration. It restores the selected session and workspace
state, then the new capture records `resumed_from_capture_id` and
`resumed_from_harness_session_id`; its member index records the build that
continued the session, preserving both sides of the upgrade as provenance.

## Blocked, lost, and waived captures

A worker restart replays its durable outbox before taking new work. A reservation
left by a crash is assigned a `crashed` verdict (`worker_restarted`), the worker
quiesces remaining job sources, reconstructs only Flow-owned source paths, stages
immutable bytes, and retries the idempotent protocol. Pending payloads are never
pruned by completed-tombstone retention.

`blocked` and `lost` are recoverable evidence states, not permission to delete
bytes. A producer with an intact grant/outbox can return to the normal publication
states; a capture whose expected set is complete can be completed from either
state. The coordinator can reconcile already-persisted pending artifact rows
without a grant by checking immutable object metadata, but it cannot recreate
worker-local bytes that were lost with the outbox/job storage. Production code
does not currently include a watchdog that automatically classifies stale
captures as `blocked` or `lost`; those states are available through the capture
transition protocol and audit log.

If recovery is impossible, an owner may explicitly waive any non-complete capture
using its current expected version and a non-empty reason. Waiver:

- permanently makes the capture `waived` and non-resumable;
- revokes its publication grant;
- abandons and attempts to abort active temporary uploads; and
- preserves capture metadata, events, and already committed evidence for audit.

Waiver does **not** manufacture a complete manifest or prove that execution was
captured. It is a recorded acceptance of missing evidence and should be rare,
reviewed, and incorporated into compliance reporting. Revoking only the upload
grant has similarly permanent publication consequences but leaves lifecycle state
unchanged; if no recovery path remains, the owner must separately waive the
capture. A complete capture cannot be waived or revoked.

## Storage, outbox, and retention sizing

Committed coordinator history has no normal TTL. With the default local backend,
blobs live under `data_dir/history/blobs`; the coordinator database and blob tree
must be backed up and restored as one logical history store. S3 can be used for
blob durability; see [S3 history-storage permissions](kubernetes.md#s3-history-storage-permissions).
Each project fails closed at the configured `coordinator.history.retention`
ceilings (100,000 captures and 1 TiB of retained artifact bytes by default).
The byte total includes active completed uploads plus pending and committed
artifact rows, without double-counting an upload when it becomes an artifact.
In-progress request bodies can temporarily consume up to their separately bounded
single-upload limits before completion either reserves capacity or aborts them.
Existing reservation and publication retries remain idempotent at a ceiling, but
new captures or artifact bytes are rejected rather than deleting immutable audit
evidence. Operators must monitor capacity and raise the ceilings or provision a
new retention domain before they are reached; capture remains mandatory.
Reconciliation defaults to a 15-minute interval, a 24-hour temporary-upload grace,
a 7-day orphan grace, and batches of 1,000. These clean incomplete storage
mechanics, not committed captures.

Relevant default ceilings are:

| Resource | Default |
| --- | ---: |
| Committed captures per project | 100,000 |
| Retained active/pending/committed artifact bytes per project | 1 TiB |
| Transcript segment | 4 MiB |
| One archive, stored | 512 MiB |
| One archive, logical | 2 GiB |
| One archived file | 1 GiB |
| Worker pending staged bytes | 2 GiB |
| Worker pending captures | 32 |
| Coordinator outstanding uploads per capture | 32 |
| Coordinator outstanding upload bytes per capture | 2 GiB |

The worker outbox defaults to `<work_dir>/history-outbox` and is required to be
outside `<work_dir>/jobs`. Size its durable filesystem for the configured pending
byte ceiling **plus** active job sources, archive construction overhead, and the
longest expected coordinator/network outage. The pending-byte ceiling counts
staged payloads, not the source worktree/Harness directory from which they are
built. Hitting count, byte, archive, or disk-pressure limits pauses or fails new
capture work rather than dropping history.

After coordinator completion, the worker scrubs payloads, source paths, temporary
upload IDs, and the upload grant, retaining only a completion tombstone. Replay
prunes tombstones older than 30 days and then keeps at most 1,024; pending entries
and payloads are never selected. These worker tombstone limits do not delete
coordinator history.

## Kubernetes durability and network boundary

The reference `flow-server` Deployment mounts a persistent volume at
`/var/lib/flow`, so its SQLite metadata and default local history blob directory
survive Pod replacement. If S3 is configured, protect the database PVC and S3
prefix consistently.

Assignment-created `flow-worker --one-shot` Jobs need durable storage for both
`work_dir/jobs` and the outbox until final publication is acknowledged. Losing
either can prevent replay or reconstruction. The current reference Kubernetes
profile sets `work_dir: /var/lib/flow-worker` but does **not** attach a PVC or
other durable volume to generated worker Jobs, and the provider currently exposes
no profile volume-mount setting. Its writable Pod filesystem is therefore not a
durable implementation of the restart/replay guarantee. S3 on `flow-server` does
not fix bytes that never left a deleted worker Pod. See
[History durability for assignment workers](kubernetes.md#history-durability-for-assignment-workers)
for the deployment consequence.

Keep coordinator API traffic (`flow-server:8421`) cluster-internal unless an
explicit authenticated owner ingress is required. History worker endpoints are
not a substitute for network isolation. Every binary's telemetry listener
(default `127.0.0.1:8422`, configured as `:8422` in the reference Pods) exposes
unauthenticated `/readyz`, `/livez`, and `/metrics`; Services, probes, and
Prometheus may reach it inside the cluster, but it must not be exposed through an
Ingress or public LoadBalancer.
