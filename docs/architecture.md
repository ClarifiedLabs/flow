# Architecture

This guide describes Flow's current implementation for contributors and operators
who want to understand or extend the system. For the longer historical design
narrative, see [flow-design.md](flow-design.md).

## Process model

Flow ships three Go commands from one module:

| Command | Role |
| --- | --- |
| `flow` | Human and in-session CLI. It registers projects, drives task/workflow commands, fetches prompts, submits typed artifacts, and attaches to terminals. |
| `flow-server` | Coordinator daemon. It serves the HTTP API, browser UI, Git HTTP exchange endpoints, web/terminal proxy routes, project registry, workflow executor, scheduler entrypoints, and exchange git hooks. |
| `flow-worker` | Job executor. It registers, claims its eligible or assignment-bound job, clones exchange branches, runs jobs in tmux, heartbeats its lease, uploads transcripts, and reports results. |
| `flow-orchestrator` | Durable assignment reconciler. It reserves exact queued jobs and creates one Kubernetes Job/Secret or Darwin child process per assignment. |

A single `flow-server` can serve many projects. Static service-mode workers can
still claim eligible jobs across projects. Provisioned workers instead have a
stable assignment identity and execute one assignment with `--one-shot`.

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

Workers keep their own `work_dir` from `flow-worker.yaml` and clone per-job
repositories there. Server data and worker work directories are deliberately
separate, especially in Docker deployments.

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
6. The CLI refreshes the client config so later owner commands usually need no
   `--server` or `--token` flags.

The user's `origin` remote is not replaced. The Flow exchange remote is private
application state and is the rendezvous point between the coordinator, local CLI,
and workers.

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
- `/v2/workers/join` for join-token based worker registration.
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

Workers register globally in `global.db` with labels (including discovered
harness capabilities), taints, accepted workload buckets, heartbeat expiry, and
optional harness model catalogs. The canonical worker config field is `accepts`:

- `persistent_agent`: author, reviewer, verifier, and console agent sessions;
- `ephemeral`: CI/check commands.

The word *ephemeral* remains a workload-bucket name; it does not select a worker
process lifecycle. For protocol compatibility, positive legacy capacity values
are normalized to acceptance value 1, and magnitudes greater than one are
ignored. One worker identity may hold only one live lease total, even when it
accepts both buckets.

Project databases hold jobs, leases, and provisioner assignments. A reservation
selects one exact eligible queued job across projects and durably records its
job/worker/provider/profile identity, role and bucket, scheduling snapshot,
startup deadline, retry state, and cleanup state in that project's database.
The assignment-aware claim path permits that worker to claim only its bound job.

`flow-orchestrator` performs recovery before new reservation on every cycle. It
inspects each open assignment's provider resource, relaunches a missing pending
resource only when its durable descriptor still matches a locally approved
profile, abandons unapproved, permanently failed, or startup-expired pending
assignments, and deletes resources for closed assignments. Only then does it
reserve new work up to each profile's `max_concurrency`. A Kubernetes provider
creates one Job and one private worker-config Secret per assignment; the Darwin
provider creates one child process and durable private state directory. Both run
`flow-worker --one-shot` with a direct worker credential returned by the
reservation API.

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

`--one-shot` long-polls until that claim, runs exactly one job, and exits after a
reported job-scoped failure as well as success. `SIGINT`/`SIGTERM` cancel worker
registration retries, long polling, maintenance, job supervision, and telemetry;
an interrupted unreported job remains subject to lease expiry and recovery.
Static workers that join with `FLOW_WORKER_JOIN_TOKEN` clear it after receiving a
scoped token. Orchestrated assignment workers instead receive a direct token and
never need the reusable join secret.

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
`feature/f-<project-key>-<number>` (schema in `0005_features.sql`; code in
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
existed at rebase start or appears mid-rebase. Rows that predate migration
0010 carry no initiator provenance, so the upgrade stamps them with a legacy
sentinel and the schedule-time gate links nothing new for them; a legacy owner
rebase simply stops acquiring new blockers after the upgrade. Owner and
unbound project-console
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

For `change_review`, the parallel children are side-effect-free discovery
reviewers. After their barrier closes, one coordinator-owned aggregation job
uses the node's dedicated frozen aggregator runtime and prompt, deduplicates the
candidate findings, and becomes the only reviewer allowed to create threads or
choose `changes_requested`. Whether that final decision may block is still
derived from the discovery agents' blocking policy. `verify_change` continues
to evaluate its children directly.

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
- **Worker join token**: reusable secret a static worker presents once to mint
  its scoped worker token.
- **Worker token**: scoped token for worker heartbeat, job claim/report, and
  transcript upload. Assignment workers receive it directly from reservation;
  abandonment and cleanup revoke credentials for that assignment worker ID.
- **Orchestrator token**: provisioner-assignment reservation, recovery, and
  cleanup calls for its bound provider IDs; it has no general owner authority.
  Retired provider IDs stay bound as explicit recovery tombstones until their
  durable assignments are cleaned, even after their final scheduling profile is
  removed.
- **Session/console tokens**: scoped tokens injected into agent sessions.
- **Hook token**: exchange hook/coordinator integration.
- **Web session cookie + CSRF cookie/header**: browser UI authentication after a
  one-time `flow ui` bootstrap URL.

Token files must be private (`0600`) when read from disk. HTTP Git remotes use
bearer credentials stored through Git's credential helper when available.

## Web UI

`flow-server` serves the browser UI directly; there is no separate frontend
server. The implementation lives under `internal/web`:

- `internal/web/assets/*.js`: browser-native ES modules; routes, models and
  shared helpers.
- `internal/web/assets/elements/*.js`: the custom elements the UI is built from.
  Each renders from its own `data` property and listens on itself once, so
  polling updates a view in place instead of rebuilding it.
- `internal/web/assets/index.html`: embedded shell page.
- `internal/web/src/*.module.css`: one stylesheet per component, plus the shared
  token and base sheets.
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
| SQLite schema/migrations | `internal/db`, `internal/sqlitex` |
| Worker directory, queues, assignments, claims, leases | `internal/worker` |
| Assignment reconciliation and Kubernetes/Darwin providers | `internal/orchestrator` |
| Worker checkout/tmux/entrypoint execution | `internal/worker/execution` |
| Git exchange, hooks, refs, merge helpers | `internal/git` |
| Harness definitions, hooks, model serialization | `internal/harness` |
| Prompt and handoff rendering/validation | `internal/prompt`, `internal/handoff`, `skills/` |
| tmux/terminal helpers | `internal/terminal` |
| Embedded web UI | `internal/web` |

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
