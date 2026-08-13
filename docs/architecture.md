# Architecture

This guide describes Flow's current implementation for contributors and operators
who want to understand or extend the system. For the longer historical design
narrative, see [flow-design.md](flow-design.md).

## Process model

Flow ships four Go commands from one module:

| Command | Role |
| --- | --- |
| `flow` | Human and in-session CLI. It registers projects, drives task/workflow commands, fetches prompts, submits typed artifacts, and attaches to terminals. |
| `flow-server` | Coordinator daemon. It serves the HTTP API, browser UI, Git HTTP exchange endpoints, web/terminal proxy routes, project registry, workflow executor, scheduler entrypoints, and exchange git hooks. |
| `flow-worker` | One-shot job executor. It registers a capacity slot with a direct credential, may wait unbound as idle capacity, exact-claims its eventual job, runs it, reports, and exits. |
| `flow-orchestrator` | Durable capacity reconciler. It recovers slots, verifies capabilities, binds jobs, and creates one Kubernetes Job/Secret or Darwin child per slot. |

A single `flow-server` can serve many projects. Every worker is one durable slot
that binds at most once and executes with `flow-worker run --one-shot --config
PATH`; invoking `flow-worker` without the `run` subcommand is an error.

## Data layout

The coordinator data directory defaults to
`${XDG_DATA_HOME:-$HOME/.local/share}/flow` unless `FLOW_DATA_DIR` or
`--data-dir` overrides it.

```text
<data-dir>/
  global.db                         # projects, workers, tokens, web sessions
  owner.token                       # fallback owner token, mode 0600
  hook.token                        # fallback hook token, mode 0600
  projects/
    <project-id>/
      flow.db                       # task/session/check/review/job/assignment state
      exchange.git/                 # private bare git exchange remote
        flow-spool/post-receive.jsonl
      transcripts/
      attachments/
```

Global state lives in `global.db`; project state is intentionally isolated in one
SQLite database per project. Jobs, leases, and durable provisioner assignments
live with their owning project. Because no database transaction spans projects,
the API registry serializes reservations and claims and checks a worker's live
leases across all project databases.

Each assignment runtime receives a private worker config and an isolated
`work_dir`, where it clones the assigned job's repository. Server data and worker
work directories are deliberately separate.

## Project registration

`flow init` performs a client/server onboarding flow:

1. The CLI resolves the target git worktree and current/base branch.
2. It calls the running coordinator to create or load a project row.
3. The coordinator creates `<data-dir>/projects/<project-id>/flow.db`, a bare
   `exchange.git`, and server-side exchange hooks.
4. The CLI adds the exchange remote to the user worktree, named `flow` by
   default.
5. The CLI seeds the configured base branch into the exchange remote and stores
   HTTP Git credentials through the user's configured Git credential helper when
   applicable.
6. The CLI records the project ID as the non-secret, repository-local
   `flow.project` Git setting used for command auto-detection.
7. The CLI refreshes the client config so later owner commands usually need no
   `--server` or `--token` flags.

The user's `origin` remote is not replaced. The Flow exchange remote is private
application state and is the rendezvous point between the coordinator, local CLI,
and workers.

`flow init --project ID_OR_NAME` attaches another worktree to an existing
project. This path verifies and configures the exchange and credential without
creating server state or pushing refs. `flow init --repair` resolves an existing
attachment and may replace its stale exchange URL, but never creates or seeds a
project. Repository-local project IDs let multiple independent clones target one project;
linked worktrees share one project association. Coordinator-side repository-path
lookup remains as a compatibility fallback for older initialized checkouts.

Exchange URLs are caller-derived, not project metadata. The CLI combines its
configured `server_url` with `/git/projects/<project-id>/exchange.git`; workers
do the same with `coordinator_url`. This lets host and container workers use
different network addresses for the same Flow server without URL rewriting or
a separately advertised exchange base URL.

## API and project routing

`internal/api.Server` handles all coordinator HTTP traffic:

- `/v2/health` for health checks.
- Git HTTP exchange routes before normal API auth.
- `/ui/*` and `/ui/api/*` for the embedded browser UI.
- Owner, worker, session, console, and hook-token authenticated API routes.
- Project-scoped routes under `/v2/projects/<project-id>/...`.

Per-project handlers are built around a `ProjectBundle` from
`internal/api/registry.go`. A bundle owns the services for exactly one project:
tasks, workflow runs and artifacts, sessions, checks, review threads, flows,
transcripts, attachments, merges, git events, worker queue, and workflow executor.

Task routes such as `/v2/tasks/t-flow-app-0001` resolve the owning project from
the task ID itself. Project-qualified routes remain the collection, creation,
and explicit authorization boundary for project-owned resources.

The API is HTTP/JSON and carries a `Flow-Protocol-Version` header. Mutating
endpoints use idempotency records where repeated client requests must be safe.

## Lifecycle and workflow executor

The task lifecycle stores only `scheduled`, `in_progress`, and `done`; null is
Unscheduled. Working and Blocked are derived In Progress substates. Done stores
one fixed resolution.

Scheduling freezes the selected flow graph and resolved agent definitions into
a `workflow_run`. The executor maintains exactly one active `workflow_node_run`,
dispatches its trusted handler, records typed artifacts and durable waits, and
appends every state or graph movement to `workflow_transitions`. Branches and
cycles are allowed, but graph nodes do not run in parallel. A multi-agent node
can internally fan out child jobs; those children remain owned by the same one
active node visit, and its successor cannot begin until the node's barrier
closes. A per-run transition budget opens an operator wait before a cycle can
run forever.

Only explicit typed results traverse graph edges. An agent artifact completion,
a launched CI command's exit status, a structured reviewer/verifier verdict,
a valid human response, or a typed merge result can select an outcome. A
handler, harness, worker, or action-application error instead records the node
error and opens a durable `execution_failed` operator wait without consuming a
transition. Human gates, execution failures, and budget exhaustion derive
Blocked. Reset cancels run-owned jobs, leases, sessions, and the active node and
returns the task to Unscheduled. Terminal nodes derive Done; a merged terminal
additionally proves the run owns a merged change.

## Worker scheduling, assignment, and execution

Orchestrator profiles select queued jobs by role, workload bucket, labels,
taints, harness models, and required selectors. Profile `accepts` controls which
workload buckets a provider may accept; it does not change the one-process-per-
slot lifecycle.

The global database holds capacity slots and runtime worker capabilities.
Project databases hold jobs, leases, and the authoritative assignment created
when a ready slot binds. Cross-database binding commits the project assignment
first; recovery repairs the global slot by stable worker ID after a crash. The
assignment-aware claim path permits that worker to claim only its bound job.

`flow-orchestrator` performs recovery, provider health checks, ready-slot
binding, demand calculation, and provisioning in that order. Desired instances
are `min(max_concurrency, active assignments + eligible queued jobs) +
idle_capacity`; pending slots count toward the target and surplus unbound slots
drain. A Kubernetes provider creates one Job and private Secret per slot; the
Darwin provider creates one child process and durable private state directory.
Both run
`flow-worker run --one-shot --config PATH` with the private config and direct
worker credential returned for that slot.

Harness availability is runtime-derived at registration and refreshed before
claim. Job requirements contain a Harness name and optional exact qualified
model ID; reasoning support is intentionally absent. Binding invalidates the
idle report, so a successful post-bind capability report is mandatory before
lease creation. Capability loss at this boundary requeues infrastructure work
without starting or consuming a task/workflow attempt.

Assignment closure fences the worker credential. Successful cleanup also removes
the global worker-directory row and records `cleaned_at`; provider deletion and
coordinator cleanup are retryable reconciliation steps. Once an assignment is
claimed, ordinary lease/job recovery is authoritative—provider failure alone
must not requeue it.

At coordinator startup, active lease deadlines are extended through the
configured worker reconnect grace before recovery starts. A still-running
worker keeps its local tmux job running and retries transient renewal failures
while the coordinator is unavailable. The original lease remains exclusive
during the grace window; if its worker does not reconnect, ordinary expiry and
crash recovery resume when the window closes.

For its one claimed job, the worker:

1. Clones or fetches the project's exchange remote into its work directory.
2. Checks out the job's task branch.
3. Builds the job environment, including Flow session/job variables.
4. Configures harness hooks and client-side git hooks where needed.
5. Starts a tmux session and registers it for coordinator terminal attach.
6. Runs the entrypoint.
7. Heartbeats the lease and reports session/check/job events.
8. Uploads the tail of the transcript when the job finishes.

`flow-worker run --one-shot --config PATH` long-polls until the exact claim,
runs one job, reports its outcome, and exits after job-scoped failure as well as
success. `SIGINT`/`SIGTERM` cancel registration retries, long polling,
maintenance, job supervision, and telemetry; an interrupted unreported job
remains subject to lease expiry and coordinator recovery.

## Git exchange and hooks

Each project has a private bare exchange remote. Flow-managed refs include:

- the protected base branch;
- `refs/heads/task/t-<project-key>-<number>/run-N` run-specific task branches;
- coordinator-owned tags and future internal `refs/flow/*` refs.

Server-side `pre-receive` hooks enforce guardrails:

- protected base updates require owner or coordinator principal;
- non-fast-forward task branch updates require coordinator principal;
- task branch pushes are allowed from owner, worker, session, coordinator, or
  local same-user principals when they are fast-forward;
- unknown Flow-managed namespaces are rejected;
- `.flow/session/**` is not allowed on the protected base branch.

`post-receive` writes JSONL events to `exchange.git/flow-spool/post-receive.jsonl`.
The coordinator drains those events into SQLite and uses them to update change
heads, stale checks, and review-thread trailer claims. `flow reconcile` remains a
manual recovery path when git and SQLite drift.

## Work items, epics, and features

Tasks, epics, and features share a project-local identity in `work_items` and a
single typed graph in `work_item_relations`. Subtype tables own their state and
metadata: tasks are executable, epics are non-executable aggregate containers,
and features are non-executable containers that own Git branches. A `parent_of`
edge gives an item at most one organizational parent; `blocks` and `related_to`
may connect any work-item kinds. Blockers attached to a container are effective
for all of its descendants.

An epic with the `all_children` policy completes once it has at least one direct
child, every direct child is terminal, and it has no unresolved effective
blocker. Reopening a child reopens automatically completed ancestors. Manual
epics require an explicit completion and are never silently reopened. Starting
an epic or feature schedules all of its unscheduled descendant tasks; blocked
tasks are scheduled and wait at the workflow dependency gate.

A **feature** has a long-lived exchange branch
`feature/f-<project-key>-<number>` (schema in
`internal/db/migrations/0001_init.sql`; code in
`internal/coordinator/features.go`). `tasks.feature_id` is a validated cache of
the nearest feature ancestor, used by existing branch-aware task and merge
queries. Parent mutations refresh that cache transactionally and are rejected
if they would change the base of a task with workflow or Git state.

Feature tasks treat the feature branch as their protected base: they branch off
its tip and (squash-)merge back into it via the same base-generic
`MergeService` used for the project base branch. A task merged into its feature
branch still resolves `merged`; landing is a separate event.

Nested features persist their integration target at creation. A root feature is
created from the project base branch; a nested feature is created from the
nearest parent feature tip, rebases onto that branch, and lands back into it.
The recorded target is immutable, including when a feature is organized below
an intervening epic. Parent feature operations require nested descendants to be
landed or archived first.

`POST /v2/projects/{id}/features/{feature_id}/rebase` rebases the feature
branch onto its recorded integration target. A clean rebase updates the shared ref
immediately; a conflicted rebase creates a system-owned **rebase task** assigned
to the feature that runs the built-in `feature-rebase` flow (agent → automated
checks → verifier → human gate → trusted `finalize_rebase` node). The agent
works on an ordinary task branch under its session `AllowedRef` confinement;
only the coordinator rewrites the shared feature ref, compare-and-swapped in
`finalize_rebase`. On creation the rebase task blocks the feature's non-done
tasks through ordinary `blocks` relations, so nothing starts from the stale
tip; unblocking follows the existing `has_active_blockers` semantics. Console
credentials bound to a single task may rebase only a feature whose open work
includes that task, and their conflicted rebases confine the rebase task's
blocker links to the bound task itself. The restriction is recorded on the
running `feature_rebases` row and applied both when the rebase starts and by
the schedule-time gate (`EnsureRebaseBlock`) for tasks created or reopened
while the rebase runs, so a sibling feature task is never linked whether it
existed at rebase start or appears mid-rebase. Owner and unbound project-console
credentials keep the full project-wide gate. The
bundled `flow-rebase-author` and `flow-rebase-verifier` skills carry the role
instructions; the verifier proves the delta between the old feature tip and the
rebased head is exactly the base branch's incoming changes.

`.../land` squash-merges the feature branch into its integration target and marks the
feature `landed`, healing on a no-op after a crash between the ref update and
the row update. `.../archive` marks the feature `archived`; the branch is
retained for audit.

Agent outputs are immutable typed workflow artifacts. `flow complete` submits a
Markdown summary plus a `handoff`, `change`, or `task_set` payload. Change
artifacts are pinned to the submitted HEAD; later prompts receive prior artifact
summaries from the frozen run.

## Flows, agent definitions, and checks

Flow configuration is stored in SQLite:

- **Agent definitions** combine the **model agent** selection (harness, optional
  model, and optional reasoning effort) with the **focus agent** identity and
  prompt instructions. The global database stores definitions inherited by all
  projects; each project database may shadow a global definition with a
  same-name local row. They are managed with
  `flow agent-defs list|create|edit|rm` and the `--global` flag.
- **Flows** define directed trusted-node graphs, strict per-kind configuration,
  outcome transitions, a start node, and a transition budget. They are managed
  with `flow flows list|create|edit|rm|set-default`.

`change_review` and `verify_change` are multi-agent trusted node kinds. Entering
one node fans out one persistent-agent child job per configured focus agent.
The workflow still has only that one active graph node: child jobs are internal
work, not graph-node concurrency. Its barrier awaits every blocking and
advisory child before evaluating the transition.

For `change_review`, the parallel children are non-mutating discovery reviewers:
they do not create or change threads, tasks, checks, or workflow state. Each child
writes a structured verdict, and its worker validates that verdict against the
lease-bound job and source check before persisting it as the source check result.
Discovery therefore persists reports even though it does not mutate review or
workflow state. After the barrier closes, one coordinator-owned aggregation job
receives those persisted source check results under **Candidate Reports**, uses
the node's dedicated frozen aggregator runtime and prompt, and deduplicates them.
The final aggregator is distinct from both discovery and a standalone reviewer:
it emits the one aggregate verdict, then the worker/coordinator alone creates
eligible threads, applies declared follow-up task actions, and chooses
`changes_requested`. Whether that final decision may block is still derived from
the discovery agents' blocking policy. A standalone reviewer likewise emits one
verdict for the worker to apply, without directly mutating project state.
`verify_change` continues to evaluate its children directly; verifiers declare
thread decisions in verdicts and the worker applies them.

Every reviewer comment also classifies its requirement source, finding basis,
remediation breadth, and scope rationale. Explicit requirements, demonstrated
regressions, and security defects remain ordinary blockers. A final aggregator
that finds a blocking inferred scope requirement with cross-cutting, legacy-
migration, or unknown remediation emits one structured decision request for one
coherent cluster. The worker opens a durable `review_scope_decision` wait before
filing comments, creating follow-up tasks, or reporting the aggregation check.
An owner choice becomes a durable ruling and reruns aggregation only; a changed
head invalidates the wait and reruns full discovery.

Owner rulings are versioned `workflow_owner_ruling_recorded` transitions.
Active rulings are projected from the complete run history, apply together, and
are removed only by an explicit replacement's `supersedes_id`. They are included
in prompt context for every task-bound author, reviewer, and verifier and are
best-effort broadcast to live same-run agent sessions after the transition
commits. Task-bound prompt enrichment is fail-closed so an agent never starts
without the current ruling projection.

Every reviewer and verifier mode compares the checked-out change to
`origin/${FLOW_BASE:-main}`. Flow guarantees that remote-tracking base ref is
present in the worker checkout; a local branch named by `FLOW_BASE` is not part
of the checkout contract. These generated guardrails are appended even when an
agent definition supplies custom role instructions.

Blocking is the default when an entry omits the flag. A candidate from an
advisory reviewer remains visible but cannot become a blocking aggregate
finding. New configuration uses `blocking: false` for advisory agents.
`required` is retained only as a deprecated compatibility alias (`true` is
blocking, `false` is advisory), and configurations should not specify both
fields.

Reviewer and verifier children must report `satisfied` or `blocked` through a
valid structured verdict file. Flow-owned agent checks run interactively and
must call `flow complete`; the command validates the role-specific schema and
atomically writes a job/check/mode/digest completion seal. The worker verifies
that seal against authoritative job context, captures the validated report,
and only then ends the terminal. A missing seal leaves the terminal live, so an
operator can attach to the check job and continue the conversation. Repository
check entrypoints retain process-exit semantics.

Missing or invalid verdicts, an explicit harness exit before completion,
harness failures, and failures while applying declared thread actions are
`errored`, never a domain outcome. CI maps an ordinary launched-command exit to
pass/fail, while worker/setup, command-resolution, and signal-termination
failures are `errored`. A required errored check pauses the node for owner
retry; advisory errors remain visible and non-blocking. Retrying a check node
preserves same-revision results and enqueues only its errored checks.

The coordinator seeds global task-planner, author, code-reviewer,
security-reviewer, review-aggregator, and verifier definitions. Fresh projects
inherit those rows and seed only two project-owned flows: the default coding
graph (implementation, checks, review, human gate, verification, and merge
loops) and the planning graph
(task-set authoring, human review, transactional materialization, and
completion). No default agent-definition rows are stored in project databases.
Flows store global definition IDs; snapshot resolution applies a same-name
project override before freezing the run.

Repo-versioned automated check configuration lives in `.flow/checks/*.yaml`.
Checks belong with the code; agent definitions and flows are coordinator-owned so
they can reflect live worker harness availability and can be edited through the
web UI or CLI.

## Authentication and credentials

Flow uses bearer tokens for API clients and short-lived cookies for the web UI:

- **Owner token**: human/admin CLI calls and web UI bootstrap.
- **Worker token**: short-lived, capacity-slot credential. Before binding it is
  limited to registration, heartbeat, claim-wait, and control. After binding it
  is limited to the exact project/assignment for claim/report and uploads;
  a worker that owns an unexpired, unreleased lease for a claimed/running job
  also has read-only task-facing context in that lease's project. Assignment
  abandonment and cleanup revoke both capabilities.
- **Orchestrator token**: capacity-slot provisioning, assignment binding,
  recovery, and cleanup calls for its bound provider IDs; it has no general
  owner authority.
  Retired provider IDs stay bound as explicit recovery tombstones until their
  durable assignments are cleaned, even after their final scheduling profile is
  removed.
- **Session/console tokens**: scoped tokens injected into agent sessions. For
  reviewer and verifier checks, the project is the read boundary: task records,
  lifecycle transition/status history, and review threads/comments are available
  as read-only context, including records outside the current task when useful.
  This does not grant direct mutation authority. Agents declare comments, follow-up task actions,
  thread decisions, and outcomes only in the verdict file; the lease-bound
  worker/coordinator validates and applies allowed writes. Task-bound author or
  console capabilities remain separately constrained by their role.
- **Hook token**: exchange hook/coordinator integration.
- **Web session cookie + CSRF cookie/header**: browser UI authentication after a
  one-time `flow ui` bootstrap URL.

Token files must be private (`0600`) when read from disk. HTTP Git remotes use
bearer credentials stored through Git's credential helper when available.

### Project task-facing read boundary

Task-facing project context includes the project task list; task body and status;
checks and review state; workflow state, history, and read-only artifacts;
lifecycle transitions and findings; relations; attachment list/download; prompt
context; change detail/diff; and review-thread/comment lists. An owner may read
this context in every project. A project-bound session or console may read it in
its own project, and a worker may read it in any project where it currently owns
an unexpired, unreleased lease for a claimed/running job. The lease need not be
for the requested task, and a direct worker can therefore have separate access
to multiple projects. Hook and provisioner credentials have no task-facing read
access. Each request checks the current lease; this association is not cached on
the credential.

Project-readable task-facing history is an authorization boundary, not per-task
privacy: task bodies, lifecycle transition/status text, and review discussion may
be disclosed to any principal meeting that project-read policy. Operators should
not put secrets in those fields and should separate projects when that visibility
is too broad. This does **not** include full-fidelity history captures, raw
transcripts, terminal access, session messages, upload grants, resume artifacts,
or other execution evidence. Those remain private, potentially secret-bearing
operational surfaces as documented in [`docs/history.md`](history.md), and worker
operations on them require the exact target lease for the job/session rather than
merely a task-facing project-read lease. That lease is normally live; the narrow
canceled-console exit-fence recovery path still requires the worker that owned
that exact lease.

## Web UI

`flow-server` serves the browser UI directly; there is no separate frontend
server. The implementation lives under `internal/web`:

- `internal/web/assets/app.js`: the entry module and composition root
  (`FlowApp` wiring, shell, routing, polling); collaborators in
  `internal/web/assets/app/` (`routes.js`, `settle.js`, `sidebar.js`,
  `caches.js`).
- `internal/web/assets/*-route.js`: thin routes that fetch and mount an element.
- `internal/web/assets/elements/*.js`: the custom elements the UI is built from,
  plus their view helpers. Each renders from its own `data` property and listens
  on itself once, so polling updates a view in place instead of rebuilding it.
- `internal/web/assets/models/`: pure projections (task run/review/relations,
  lifecycle options, now card, harness catalog/selection).
- `internal/web/assets/actions/`: the delegated action table (busy registry,
  dispatcher, per-domain handlers).
- `internal/web/assets/index.html`: embedded shell page.
- `internal/web/src/*.module.css`: one stylesheet per component, plus the
  flow-app chrome sheets split by domain.
- `internal/web/webassetbuild`: small Go step that scopes each stylesheet to its
  own element and concatenates them, used by embedded asset serving and tests.

The UI treats coordinator read models as the source of truth. It keeps a small
client-side cache for convenience, but task lanes, review state, checks,
terminal state, and flow state are authoritative on the server.

## Source map

| Area | Packages/files |
| --- | --- |
| Commands | `cmd/flow`, `cmd/flow-server`, `cmd/flow-worker`, `cmd/flow-orchestrator` |
| HTTP API and project registry | `internal/api` |
| API request/response contract aliases | `internal/api/contract` |
| CLI client plumbing | `internal/client` |
| Configuration loading and defaults | `internal/config` |
| Domain services | `internal/coordinator` |
| Project event log (durable stream + SSE) | `internal/coordinator/event_log.go`, `internal/api/event_handlers.go` |
| CLI machine output contract (`--json`/`--agent`) | `internal/cliout` |
| Post-commit hooks | `internal/hooks` |
| MCP read server | `internal/mcp` |
| Backup and restore | `internal/backup` |
| SQLite schema/migrations | `internal/db`, `internal/sqlitex` |
| Worker directory, queues, assignments, claims, leases | `internal/worker` |
| Assignment reconciliation and Kubernetes/Darwin providers | `internal/orchestrator` |
| Worker checkout/tmux/entrypoint execution | `internal/worker/execution` |
| Git exchange, hooks, refs, merge helpers | `internal/git` |
| Harness definitions, hooks, model serialization | `internal/harness` |
| Prompt and handoff rendering/validation | `internal/prompt`, `internal/handoff`, `skills/` |
| tmux/terminal helpers | `internal/terminal` |
| Embedded web UI | `internal/web` |

## Deliberately out of scope

Patterns evaluated and intentionally not adopted; recording them here so
they are not re-litigated:

- **Hub/spoke federation.** Flow's `flow-server` is already the shared
  daemon; there is no peer federation or cross-server sync.
- **ULID dual identity.** Flow's canonical keyed ids (`t-<key>-NNNN`) already
  carry project and sequence; a second id scheme adds nothing.
- **JSONL-cutover migrations.** Flow's in-place numbered SQL migrations are
  sufficient; the export is not the migration vehicle.
- **Semantic/embedding search.** `flow search` is lexical (FTS5 with a
  substring fallback); embeddings stay opt-out.
- **Terminal UI (TUI).** The CLI + embedded web UI cover the surfaces; no
  interactive TUI.
- **Task recurrences.** Not flow's model.

## Development and verification

Common verification targets are documented in [development.md](development.md):

```sh
make test
make js-test
make web-smoke
```

For changes that affect workflow transitions, worker execution, git exchange
hooks, or web routes, prefer a targeted package test first and then the broadest
relevant Make target.
