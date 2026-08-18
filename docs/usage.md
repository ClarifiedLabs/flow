# Usage

This guide covers the web UI, task lifecycle, common CLI commands, check
configuration, routes, transcripts, and operational notes.

## Web UI

There is no separate web server to start. `flow-server serve` serves the web app
under `/ui/*` on the same coordinator address.

The web UI setup is:

1. Start `flow-server serve` with owner and orchestrator tokens.
2. Start `flow-orchestrator` with a Kubernetes or Darwin capacity profile. It
   creates one-shot worker runtimes for active demand plus configured idle
   capacity.
3. Onboard at least one repository with `flow init`.
4. Run `flow ui` from a registered repository, or run
   `flow ui --server URL --token TOKEN`.
5. Open the printed login URL.

`flow ui` creates a short-lived, single-use browser login URL. The browser
exchanges that bootstrap token for an HttpOnly session cookie, so the long-lived
owner token is not placed in JavaScript.

The board shows every project's live tasks as cards in three lanes:
**Scheduled** (queued for its first worker job), **Working** (actively
executing — including tasks whose worker job is queued or claimed but not yet
running, labeled **awaiting worker** so a stalled dispatch is visible at a
glance), and **Waiting** (idle: parked at a human gate or in review, held, or
blocked). A throughput strip above the lanes tallies successful completions in
cumulative windows from 15m up to 24h; closed work lives in the Tasks view's
Done filter (`/ui/tasks?state=done`) and on the Done page (`/ui/done`), which
lists every closed task with its outcome and merged change. A sticky top bar
frames every
page: primary navigation lives in the dropdown on the left, whose trigger
shows the current page label alongside compact board lane chips (scheduled,
in progress, blocked). The panel lists every destination — Board, Tasks,
Console, Done, Flows, Workers, Jobs — with live per-destination badges such as
the unscheduled-task count, closed-task count, assignment-worker status, and active and queued jobs, plus the
theme switcher at the bottom. A project picker appears at the right edge of
the bar when more than one project is registered and filters the board by
project. Task IDs embed their normalized project key, so links such as
`/ui/tasks/t-flow-app-0001` remain unambiguous across every registered project.

The **Tasks** page is a flat, filterable list of every task across the visible
projects — including unscheduled work, which no longer has a board lane.
Lifecycle-state chips (All, Unscheduled, Scheduled, In Progress, Done) narrow
the list; the four state chips combine, so several states can be shown at
once, and All selects or clears all four in one click. An in-view project
dropdown (composing with the topbar project picker) and a title/body text
search narrow the list further. Row checkboxes (plus select-all)
enable bulk edits that fan out over the selected tasks: set priority, set
flow, schedule, reset to unscheduled, and retry a failed workflow.

Use **New Task** to create unscheduled work from the browser. The form can
select a project, workflow, priority, attachments, and whether to schedule the
task after creation. Use the **Flows** page to configure trusted workflow
nodes, transitions, agents, start nodes, and transition budgets.

For temporary live verification, avoid rewriting your normal CLI discovery
config by either running the server with `--no-write-client-config`, or by
pointing `--client-config` at a temporary path. The `make web-smoke` target uses
an isolated `XDG_CONFIG_HOME` and `FLOW_DATA_DIR` before running the Chromium
web UI smoke test.

## Task Lifecycle

Tasks have four user-facing lifecycle states:

```mermaid
flowchart LR
    unscheduled([Unscheduled / null]) -->|schedule| scheduled([Scheduled])
    scheduled -->|worker starts| progress([In Progress])
    progress --> done([Done])
    progress --- awaiting([Awaiting Worker])
    progress --- working([Working])
    progress --- blocked([Blocked])
    scheduled -->|reset| unscheduled
    progress -->|reset| unscheduled
    done -->|reopen| unscheduled
```

New tasks have no persisted lifecycle state and appear as **Unscheduled**.
**Scheduled** means the workflow run has not started yet: the task is queued
for its first worker job, and unresolved blocker tasks hold execution at the
dependency gate. **In Progress / Awaiting worker** (the derived `awaiting_worker`
lane state) means the active workflow node has a live job that is queued or
claimed but not yet running. **In Progress / Working** means a job is running
or the node is executing synchronously. **In Progress / Blocked** means a human
gate or safety budget requires owner action. **Done** carries a fixed
resolution: `completed`, `merged`, `rejected`, `abandoned`, `cancelled`, or
`failed`.

The work inside In Progress is a frozen directed graph. It may branch and loop,
but has one active node at a time. Human gates expose their configured outcomes
on task detail. Reset cancels the active run and preserves its history; reopen
starts a new Unscheduled run, including after a merge.

Inspect the active run and its node history with `flow task workflow TASK_ID`.

## Machine-readable output (--json / --agent)

Every API-backed command accepts `--json` and `--agent`, either globally
(`flow --agent task list`) or on the command (`flow task list --agent`).
Both modes write machine output to **stdout**; human diagnostics stay on
stderr. The schema is contract **v1** (`internal/cliout`); breaking changes
bump the version.

- `--json` prints the command's result as one bare JSON object.
- `--agent` wraps it in a versioned envelope and structures errors:

```json
{
  "contract_version": 1,
  "command": "task create",
  "ok": true,
  "data": { "task": { "id": "t-demo-0001" }, "reused": false, "attachments": [] }
}
```

On failure the envelope carries the server's error code and a message:

```json
{
  "contract_version": 1,
  "command": "task show",
  "ok": false,
  "error": { "code": "task_not_found", "message": "task t-demo-9999 not found" }
}
```

Exit codes are stable: `0` success, `1` command error, `2` usage error, plus
domain codes such as `flow wait`'s `3` (timeout, reported as
`error.code = "wait_timeout"`). Usage errors report `error.code =
"usage_error"`. `--json` failures print a bare `{"error": {...}}` object.

See `docs/reference/cli-output.md` for the full stability contract, the
`agent_format`/`protocol` fields on `flow version`, and the idempotency
`reused` semantics.

Every command's `data` is a JSON object so fields can be added without
breaking the contract. Currently covered: `task create|list|show|edit|done|
reopen|relations`, `feature create|list|show`, `epic create|list|show|edit|
start|complete|reopen|archive`, `board`, `ready`, `next`, `wait`, and
`status`. Show commands return the API v2 response object as `data`; other
commands key their payload (`{"task": ...}`, `{"tasks": [...]}`,
`{"board": ...}`, ...). Agents should treat unknown keys as forward
compatible.

```sh
flow --agent task create --title "Wire retries"   # {"task": ..., "reused": false}
flow --agent next                                 # {"task": {...}} or {"task": null}
flow --agent wait t-demo-0001 --until done        # exit 0/3, envelope either way
```

## Common CLI Commands

Once `flow-server serve` has written `$XDG_CONFIG_HOME/flow/config.yaml`, owner
commands need no `--server` or `--token` flags. Pass `--server` or `--token`
only to override the discovered config.

CLI commands auto-detect their project from the current repo's worktree. Run
them from inside a registered repository or target project-owned collection
operations explicitly with `--project NAME|ID`. A full task ID is globally
resolvable, so `flow task show t-flow-app-0001` works from any directory.

Initialization records the project ID in the repository-local `flow.project` Git
setting. Use `flow init --repair` to restore a missing or stale Flow remote and
its path-scoped Git credential without creating a project. Use
`flow init --project NAME|ID` in a fresh clone to attach it to an existing
project without seeding or pushing refs.

Project IDs are derived from the project display name. Flow lowercases ASCII
letters, replaces punctuation and whitespace runs with hyphens, and keeps the
display name unchanged. For example, `flow init --name "Flow App"` creates
project `p-flow-app`, whose first task is `t-flow-app-0001`. Two names that
normalize to the same key are rejected so identifiers remain predictable.

Tasks:

```sh
flow task create --title TITLE [--flow FLOW] [--feature FEATURE] [--idempotency-key KEY]
flow task list
flow task show TASK_ID
flow task edit --title TITLE [--feature FEATURE] TASK_ID
flow task schedule TASK_ID
flow task workflow TASK_ID
flow task guide TASK_ID "OWNER RULING" [--supersedes RULING_ID]
flow task decide-review TASK_ID WAIT_ID fix_in_task|out_of_scope|defer_follow_up [--guidance TEXT]
flow task respond TASK_ID --node-run NODE_RUN_ID --outcome OUTCOME --feedback "..."
flow task retry [--refresh-agent-runtime] TASK_ID
flow task budget TASK_ID --additional N
flow task reset TASK_ID
flow task done TASK_ID --resolution completed
flow task reopen TASK_ID
```

`task create` is idempotent: the CLI sends an `Idempotency-Key` header on
every create, generating a fresh key per invocation unless `--idempotency-key`
is supplied. A retried request with the same key and the same body replays the
stored response (marked with an `X-Flow-Idempotent-Replay: 1` header) instead
of creating a second task. The server's fingerprint is a hash of the whole
request — method, path, and exact body — so the same key with any difference
(even cosmetic whitespace in the body) is rejected with
`idempotency_key_conflict` rather than silently reusing the task. Scripted
callers that may retry the same command should pass an explicit
`--idempotency-key` (for example, one derived from their own operation ID) so
the retry replays rather than duplicates.

Internal task creators use durable operation identities. Final review aggregation
submits its exact sealed report as one batch keyed by the authenticated source job;
an exact retry returns the same batch receipt and a different report digest for
that job is rejected. Its `task_action` entries remain proposals until a separate,
human-approved organizer task-set is materialized. Task-set materialization
replays per artifact, so neither worker retries nor organizer retries duplicate
work. The legacy singular follow-up endpoint remains readable for rolling
compatibility but current workers do not call it.

`task guide` records durable guidance on the active workflow run and delivers
it to every live author, reviewer, and verifier session in that run. Rulings
remain active together until a later ruling explicitly names one with
`--supersedes`; they do not use newest-wins semantics. The CLI generates a
fresh idempotency key unless `--idempotency-key` is supplied.

Agent-oriented task discovery and synchronization:

```sh
flow ready [--tag TAG]
flow next [--tag TAG]
flow wait TASK_ID... [--until done|blocked|scheduled|in_progress] [--any|--all] [--timeout 30s] [--poll-interval 2s]
flow events [--since N] [--limit N] [--follow]
```

`ready` lists tasks an agent could start right now: unscheduled with no
unresolved blockers — the same rule the schedule-time dependency gate applies
before starting workflow nodes (blockers are any work items linked by `blocks`
to the task or an ancestor; a blocker resolves when it reaches its terminal
state). Output orders by priority, then creation time, so `next` prints the
single best task to pick up. `wait` polls task reads until the `--until`
condition holds (default `done` on all tasks; `--any` flips to
at-least-one). A task's `blocked` state is the board's needs-attention
derivation: unresolved blockers when unscheduled, open human gates or a system
convergence hold when in progress. Durations need Go units (`30s`, `5m`);
`--until` matches current states, so a task that reset to unscheduled never
satisfies `done`. Exit codes: 0 condition met, 1 error, 2 usage, 3 timeout.

`events` reads the project event log: one durable, totally ordered stream of
agent-relevant state changes (task lifecycle and edits, epic/feature
transitions — including reconciler-driven automatic epic completion/reopen,
marked `"automatic": true` with actor `system` — session status writes,
deduplicated git pushes). Every event
carries a monotonic `seq`; pass `--since` with the last seq you saw (or the
`next_since` cursor from the previous machine-mode page) to resume without
gaps or duplicates. `--follow` streams new events as server-sent events,
printing one event JSON per line in machine modes. Use it instead of polling
`board` when an agent needs to react to changes.

`search QUERY` finds tasks by title/body: ranked full-text matches when the
server binary is built with FTS5, substring matches otherwise.

Completion evidence and audit:

```sh
flow task done TASK_ID [--resolution R] [--message TEXT] [--evidence type:value]
flow audit completions [--resolution R] [--limit N]
```

`task done` accepts an optional substantive `--message` and repeatable typed
`--evidence` entries (`commit:SHA`, `test:cmd`, `pr:URL`, `review:ref`,
`note:text`). `flow complete --evidence ...` attaches the same evidence to the
handoff artifact and auto-records the pinned HEAD as commit evidence. Evidence
is stored with the task's done stamp and shown by `flow task show`. `flow audit
completions` lists done tasks with their resolution, actor, message, and
evidence — the tool for finding lazy closes.

Final review aggregation may pause on a `review_scope_decision` when a valid
blocking finding depends on an inferred, broad scope requirement. Use
`task decide-review` to require the fix in this task, exclude it without
follow-up work, or retain it only as non-blocking follow-up. If the change head
moves while the decision is open, Flow invalidates the decision and restarts
the complete discovery round against the new head.

Accepted non-blocking proposals never delay that review or merge. After approval,
Flow asynchronously creates a system-owned organizer task. Its dedicated planning
agent consolidates same-root findings, reuses only high-confidence open tasks,
and proposes only real prerequisite `blocks` edges. A human approves the plan
before materialization. Organizer rejection or failure leaves the source delivered
and marks the follow-up set `attention` for retry. The source task's **Findings**
tab shows aggregation check/job/head provenance, every batch and proposal, the
active revision and plan artifact, each final disposition/target/grouping/blocker,
and errors. Pre-batch actions appear separately as **Legacy follow-ups** because
they do not contain the newer report and job provenance. Verifier jobs can certify
or reopen review threads but cannot create follow-up proposals.

Features (project-child task groups with their own exchange branch):

```sh
flow feature create --title TITLE [--body BODY]
flow feature list [--status open|landed|archived|all]
flow feature show FEATURE_ID
flow feature edit --title TITLE FEATURE_ID
flow feature rebase FEATURE_ID
flow feature land FEATURE_ID
flow feature archive FEATURE_ID
```

A feature groups a set of tasks behind one long-lived `feature/f-...` branch.
Tasks assigned to a feature (`flow task create --feature`, the web task form's
feature picker) branch off the feature branch and merge back into it instead of
the base branch. `flow feature rebase` rebases the feature branch onto the base
branch — instantly when the merge is clean, otherwise by creating a rebase task
whose agent resolves the conflicts and whose verification gates a
compare-and-swap update of the shared branch. A running rebase blocks the
feature's other tasks via ordinary `blocks` relations. `flow feature land`
squash-merges the feature branch into the base branch; archive is the only
delete and retains the branch for audit.

Board and diagnostics:

```sh
flow board
flow checks TASK_ID
flow transitions TASK_ID
flow workers
flow jobs
flow reconcile
```

Flow configuration:

```sh
flow agent-defs list
flow agent-defs create -f agent.yaml
flow agent-defs edit -f agent.yaml AGENT_DEF_ID
flow agent-defs rm AGENT_DEF_ID
flow agent-defs list --global
flow agent-defs create --global -f agent.yaml
flow agent-defs edit --global -f agent.yaml AGENT_DEF_ID
flow agent-defs rm --global AGENT_DEF_ID
flow flows list
flow flows create -f flow.yaml
flow flows edit FLOW_ID -f flow.yaml
flow flows rm FLOW_ID
flow flows set-default FLOW_ID
```

`flow board` aggregates the lanes of every registered project. Use `--project`
to scope a command to one project when you are not inside its worktree.
In-progress tasks whose active workflow node has a queued or claimed worker job
are annotated `[awaiting worker]`, distinguishing them from tasks that are
actively working (which carry no annotation).

`flow agent-defs` and `flow flows` manage the same configuration shown in the
web UI's **Flows** page. Agent definitions are project-scoped by default; pass
`--global` to manage the coordinator-wide catalog inherited by every project.
The `-f` files can be YAML or JSON; use `-f -` to read from stdin. `flow flows
list` prints each graph's start key and node count, followed by its
blocking/advisory reviewer and verifier counts.

An agent definition combines two concerns: the **model agent** selection
(`harness`, optional `model`, and optional `reasoning_effort`) and the **focus
agent** instructions (`name` and `prompt`) applied to that model. The
`flow agent-defs list|create|edit|rm` commands manage these reusable
model/focus definitions; graph nodes reference their IDs. A project's effective
catalog merges its local rows with the global catalog by name. A local row wins
when names match. Editing an inherited row through the project command creates
that same-name local override; editing with `--global` changes the definition
for every project that has not overridden it. Global definitions referenced by
any project flow cannot be deleted.

For example, create two definitions with distinct models and review focuses:

```yaml
# security-review.yaml
name: security-review
harness: harness
model: openai:gpt-5.2
reasoning_effort: high
prompt: Review authorization boundaries, input trust, and secret handling. Report concrete exploitable paths.
```

```yaml
# performance-review.yaml
name: performance-review
harness: harness
model: anthropic:claude-opus-4-8
reasoning_effort: high
prompt: Review algorithmic cost, database access patterns, allocation hot spots, and realistic scaling risks.
```

```sh
flow agent-defs create -f security-review.yaml
flow agent-defs create -f performance-review.yaml
flow agent-defs list
```

Use the IDs returned by those commands in a review node (shown here as the
relevant node from a flow file):

```yaml
key: review
name: Security and performance review
kind: change_review
config:
  change_review:
    agents:
      - agent_def_id: ad-security-review
        blocking: true
      - agent_def_id: ad-performance-review
        blocking: false
    aggregator_agent_def_id: ad-review-aggregator
```

A flow may reference an inherited global ID. At scheduling time Flow resolves a
same-name project override, if present, then freezes the selected model and
focus prompt. Editing a live global or project definition therefore affects
only later workflow runs.

Agent/session workflow, usually run inside a Flow-managed tmux session:

```sh
flow fetch-prompt
flow status "Working on implementation"
flow status --kind blocker "Stuck: API contract is ambiguous"
git commit -m "feat: implement the change"
mkdir -p .flow/session
flow complete --summary-file .flow/session/SUMMARY.md
flow session event working|waiting
```

`flow status` records a typed progress event on the session's task. The
`--kind` flag classifies the entry so the coordinator and web UI can tell
routine notes apart from things that need a human. Valid kinds are `note`,
`progress`, `plan`, `blocker`, and `question`.

`flow complete` validates the active node contract and submits its typed
artifact. For a change node it resolves and pushes the run-specific branch and
pins the artifact to HEAD. Task-planning nodes also pass
`--output-file .flow/session/TASK_SET.json`; handoff nodes need only the Markdown
summary. Flow-generated summaries and manifests belong under `.flow/session/`,
which worker worktrees add to `.git/info/exclude`, omit from review diffs, and
protect from landing on the base branch. Squash merges also reject newly added
files matched by the repository's
committed `.gitignore`, so project-specific generated state cannot be landed by
an older task branch. The command is idempotent for the active node run.

Flow-owned reviewer, discovery, aggregation, and verifier jobs use the same
command without flags. They run as interactive check terminals: write the
structured verdict to `$FLOW_VERDICT_FILE`, then run `flow complete`. The
command validates the role-specific verdict schema and seals its exact bytes;
the worker closes the terminal and reports the captured result only after that
seal is valid. A validation error is printed in the live terminal so the agent
or an operator can correct the verdict and retry without starting another
agent run. Repository-owned custom checks retain their configured process-exit
behavior and do not require `flow complete`.

Attach to a live author session or worker job:

```sh
flow attach SESSION_ID
flow attach --web SESSION_ID
flow attach --job JOB_ID
```

Review threads:

```sh
flow thread list CHANGE_ID
flow thread reply THREAD_ID BODY
flow thread claim THREAD_ID fixed|not_warranted|superseded
flow thread certify THREAD_ID
flow thread reopen THREAD_ID
```

## Onboarding agents

`flow quickstart` prints flow's agent operating contract — the
prompt → work → `flow complete` loop, gate responses, machine output, event-log
cursor discipline, and the mutations agents must not run unprompted. Under
`--agent` it emits a compact form suitable for stuffing into a system prompt.

`flow init --with-agents` writes a marker-delimited block
(`<!-- flow:begin -->` / `<!-- flow:end -->`) with the compact contract into the
repo's `AGENTS.md` (created if absent, or appended below existing content) and
into `CLAUDE.md` when one already exists. Rerunning refreshes only the block;
handwritten content outside the markers is untouched. Symlinked instruction
files are refused.

## Multi-Agent Nodes and Check Configuration

Repo-versioned CI configuration lives in `.flow/checks/*.yaml`. CI jobs use the
`ephemeral` workload bucket. Review and verification agents are selected by
their graph nodes and use the `persistent_agent` bucket. Orchestrator profiles
must accept the needed buckets and advertise the required Harness label, for
example `agent.harness.harness: "true"`. Flow verifies that label from the
launched worker's executable probe rather than trusting profile configuration.
An explicit model is verified against the live qualified-ID catalog; reasoning
level never affects scheduling. Bucket names classify jobs; every selected job
still gets its own one-shot capacity slot and worker process.

An `automated_checks` node runs the repository CI definitions. A
`change_review` or `verify_change` node is a multi-agent node: it fans out one
child job per configured focus agent while remaining the workflow run's one
active graph node. Successor graph nodes cannot start until that internal
barrier closes.

The barrier awaits every child, including advisory agents. In a
`change_review` node, configured reviewers are parallel discovery inputs: they
cannot create review threads directly. Once they all report, Flow runs one
aggregation job using the agent definition selected by
`aggregator_agent_def_id`. Its independently editable runtime and prompt control
the synthesis pass. The aggregate deduplicates the candidate reports and is the
only reviewer result that can create blocking threads or select
`changes_requested`; whether it may block is derived from the discovery agents'
blocking settings.

Agent entries are blocking inputs by default when the flag is omitted. A
finding from an advisory input is recorded and shown but cannot become a
blocking aggregate finding. Use `blocking: false` for an advisory entry. It
does not skip or detach the job: the agent is always dispatched and the
barrier still waits for its terminal result. The older `required` spelling is
a deprecated compatibility alias (`required: true` means blocking and
`required: false` means advisory); new flow files should use `blocking` and
should not set both names.

Each node visit receives distinct check identities, so a loop back through
review or verification executes every configured child again. All of those
children use the `persistent_agent` workload bucket with author and other agent
jobs; the barrier can therefore wait until the orchestrator can bind and launch
enough capacity for another assignment.

Set `requires` labels on CI definitions to match workers that can run the
entrypoint. Review and verifier instructions belong in agent definitions, not
repository check configuration.

Example CI check:

```yaml
name: unit
kind: ci
required: true
entrypoint:
  argv: ["go", "test", "./..."]
  cwd: "."
requires: []
```

`flow-worker` sets `FLOW_WORKER_HARNESS` from an agent job's entrypoint. Use
`flow fetch-prompt --harness harness|agents` only when overriding that
automatic selection.

## Web UI Routes

The coordinator serves a dependency-light web app under `/ui/*`:

- `/ui/board`
- `/ui/projects/<project-id>/tasks/<task-id>`
- `/ui/features`
- `/ui/projects/<project-id>/features/<feature-id>`
- `/ui/changes/<change-id>`
- `/ui/console`
- `/ui/flows`
- `/ui/workers`
- `/ui/jobs`
- `/ui/done`
- `/ui/sessions/<session-id>/terminal`

The board shows every project's tasks as cards. Each card links to the
project-scoped `/ui/projects/<project-id>/tasks/<task-id>` route. A topbar
project picker filters the board by project and persists the selection in the
browser.

## Transcripts

Workers pipe each tmux session's pane output to a transcript log and, when a job
finishes, upload its last 10MB to the coordinator. The coordinator stores it
under `$FLOW_DATA_DIR/projects/<project-id>/transcripts/<id>.log` and records
the path on the owning session or job.

The task-detail session block and `/ui/jobs` page show a Transcript link
whenever a stored transcript exists. Clicking it loads the captured output
inline.

The underlying API routes are:

- `PUT /v2/sessions/<session-id>/transcript`: author sessions upload with the
  session token or owner token; `GET` returns `text/plain` with owner scope.
- `PUT /v2/jobs/<job-id>/transcript?lease_id=<lease-id>`: check jobs upload
  with the worker token and a live lease; `GET` returns `text/plain` with owner
  scope.

A failed upload is logged to the job's stdout and never fails the job.

## Capacity and assignment operations

Global capacity slots own provider resources and direct worker credentials.
Ready slots bind once to coordinator-owned assignments in the selected job's
project database. Kubernetes creates one Job plus private Secret per slot;
Darwin creates one child and private state directory. The runtime exact-claims
its bound job, reports, and exits.

Active concurrency is bounded by `max_concurrency`; `idle_capacity` adds warm,
verified slots beyond that active bound. Worker selection belongs to the
profile, and generated configs are private to one slot.

After an orchestrator restart, preserve and reuse profile names, provider IDs,
Kubernetes namespaces, and Darwin state directories. Readiness stays false until
a complete recovery-first cycle succeeds. Closed assignments remain pending
cleanup until their provider resource is deleted, worker credentials are
revoked, the worker-directory row is removed, and cleanup is recorded. Claimed
work remains governed by lease expiry and job recovery even if its provider
resource disappears.

All binary telemetry endpoints (`/readyz`, `/livez`, and `/metrics`) are
unauthenticated. Keep them loopback-only by default or cluster-internal in
Kubernetes; never expose them through a public Ingress or LoadBalancer.

## Post-commit hooks

`flow-server` can run an external executable after matching entries commit to
a project's event log — for example, to post a Slack message when a task
completes. Configure hooks in the coordinator config file:

```json
{
  "hooks": [
    {
      "events": ["task.done"],
      "command": ["/usr/local/bin/flow-on-task-done", "--channel", "#eng"]
    }
  ]
}
```

`events` holds exact event kinds (`task.done`) or a single trailing glob
prefix (`task.*` matches every `task.` kind). `command` is an absolute
executable path plus arguments; the server validates at startup that every
hook has at least one pattern and an existing absolute executable, and
refuses to serve otherwise. The executable is run directly — never through a
shell — with the event as a JSON envelope on stdin:
`{"seq","kind","project_id","task_id","actor","occurred_at","payload"}`
(the raw payload is byte-capped at 64 KiB and the whole document at
256 KiB). The environment adds `FLOW_EVENT_KIND`, `FLOW_EVENT_SEQ`,
`FLOW_PROJECT_ID`, and `FLOW_TASK_ID`. Each run has a 30s timeout, and hook
stderr is captured to the server log at debug level.

Failure modes to plan around:

- Dispatch is **at-most-once and fully asynchronous**: hooks fire after the
  event commits and never block or roll back the mutation that produced it.
- A crash between commit and dispatch **loses** the hook run, and on restart
  the dispatcher resumes from the latest event — events that committed while
  the server was down never fire.
- A full dispatch queue drops runs; `flow_hook_runs_dropped_total` and
  `flow_hook_run_failures_total` on `/metrics` count drops and failures.
- **Loops are possible**: a hook that writes back into flow (e.g. via the
  CLI) emits new events that can re-trigger hooks. Keep hook chains
  convergent.

## Backup and restore

```sh
flow-server backup [--config PATH] [--data-dir PATH] (--project ID | --all) --output DIR
flow-server restore [--config PATH] [--data-dir PATH] --input DIR [--force]
```

`backup` snapshots a project: an online (`VACUUM INTO`) copy of `flow.db`, a
`git bundle` of the exchange repo, a tar of `attachments/`, and a
`manifest.json` (project id, timestamps, flow version, schema version). The
whole backup is written to `DIR.tmp` then renamed into place. `--all` backs up
every project plus the global database. Backup can run against a live server —
the SQLite snapshot is consistent — but the exchange refs and attachments may
have advanced past the database snapshot (crash-consistent, not a point-in-time
cut across all three).

`restore` is offline: stop the server first. It verifies the manifest schema
version (a backup from a newer flow fails with an actionable error), restores
`flow.db`, extracts the exchange bundle, and untars attachments. It refuses to
overwrite a non-empty project directory without `--force`.

Take a backup before upgrades. Test restores by booting a server on the
restored data dir. Portable cross-server project moves (JSONL with ID
remapping) are deliberately deferred.

## Notes

- Flow is designed for local/private coordination. The exchange remote is
  private application state.
- The CLI remains the fallback for every core action.
- Terminal attach requires a running author session or worker job. The worker
  streams terminal data to the coordinator over its control WebSocket, so no
  worker-side listener or inbound reachability is required.
- Direct protected-base pushes are rejected by Flow exchange hooks; merge
  through Flow after checks/review pass.
