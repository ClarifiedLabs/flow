# Architecture

This guide describes Flow's current implementation for contributors and operators
who want to understand or extend the system. For the longer historical design
narrative, see [flow-design.md](flow-design.md).

## Process model

Flow ships three Go commands from one module:

| Command | Role |
| --- | --- |
| `flow` | Human and in-session CLI. It registers projects, drives issue/review/merge commands, fetches prompts, submits handoffs, and attaches to terminals. |
| `flow-server` | Coordinator daemon. It serves the HTTP API, browser UI, Git HTTP exchange endpoints, web/terminal proxy routes, project registry, lifecycle engine, scheduler entrypoints, and exchange git hooks. |
| `flow-worker` | Worker supervisor. It joins the coordinator, advertises capacity and harness labels, claims jobs across projects, clones exchange branches, runs jobs in tmux, heartbeats leases, uploads transcripts, and reports results. |

A single `flow-server` can serve many projects. A single `flow-worker` can run
jobs for any registered project, subject to labels, taints, and capacity.

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
      flow.db                       # issue/session/check/review state
      exchange.git/                 # private bare git exchange remote
        flow-spool/post-receive.jsonl
      transcripts/
      attachments/
```

Global state lives in `global.db`; project state is intentionally isolated in one
SQLite database per project. Because no database transaction spans projects, the
API registry serializes job claims while checking a worker's aggregate live
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

## API and project routing

`internal/api.Server` handles all coordinator HTTP traffic:

- `/v1/health` for health checks.
- Git HTTP exchange routes before normal API auth.
- `/ui/*` and `/ui/api/*` for the embedded browser UI.
- `/v1/workers/join` for join-token based worker registration.
- Owner, worker, session, console, and hook-token authenticated API routes.
- Project-scoped routes under `/v1/projects/<project-id>/...`.

Per-project handlers are built around a `ProjectBundle` from
`internal/api/registry.go`. A bundle owns the services for exactly one project:
issues, sessions, checks, review threads, flows, transcripts, attachments,
merges, git events, worker queue, and lifecycle engine.

Some routes, such as `/v1/issues/<id>`, can be resolved implicitly only when the
principal or server context identifies a single project. Project-qualified routes
are the unambiguous form because issue IDs restart per project.

The API is HTTP/JSON and carries a `Flow-Protocol-Version` header. Mutating
endpoints use idempotency records where repeated client requests must be safe.

## Lifecycle engine

The lifecycle engine in `internal/lifecycle` is the only entry point for
lifecycle-changing events. It:

1. Resolves the issue.
2. Journals externally submitted events into a durable inbox.
3. Loads the current workflow snapshot.
4. Looks up the transition for `(phase, event)`.
5. Evaluates guards and runs actions through `Effects`.
6. Writes the new phase and transition log.
7. Confirms the inbox row after the cascade commits.

The transition log backs `flow transitions` and the web UI lifecycle timeline.
Durable timers bound otherwise invisible waits, such as a check that never
reports or an authoring phase with no activity.

The stored phase enum (`internal/coordinator/phase.go`) is triage, backlog,
up_next, working, critique, acceptance, approved, merged_closed, rejected_closed,
and abandoned. `working` is a container: the position within the issue's flow
(which phase, running or paused at a human gate) lives on the flow cursor, and
the board and transition timeline refine `working` into the `planning` and
`authoring` display phases. `blocked` is a derived overlay, not a stored phase.
See [usage.md](usage.md#issue-lifecycle) for the lifecycle diagram.

## Worker scheduling and execution

Workers register globally in `global.db` with:

- labels such as `agent.harness.codex=true`;
- taints;
- `persistent_agent` and `ephemeral` capacity;
- heartbeat expiry;
- optional harness model catalogs.

Project databases hold jobs and leases. `flow-worker` long-polls the coordinator
for work. The coordinator orders project queues by their oldest queued eligible
job and claims only when the worker has remaining aggregate capacity in the
requested capacity bucket.

Capacity buckets have different purposes:

- `persistent_agent`: author, reviewer, verifier, and console agent sessions.
- `ephemeral`: CI/check commands.

For each claimed job, the worker:

1. Clones or fetches the project's exchange remote into its work directory.
2. Checks out the job's issue branch.
3. Builds the job environment, including Flow session/job variables.
4. Configures harness hooks and client-side git hooks where needed.
5. Starts a tmux session and a ttyd terminal proxy.
6. Runs the entrypoint.
7. Heartbeats the lease and reports session/check/job events.
8. Uploads the tail of the transcript when the job finishes.

The worker clears `FLOW_WORKER_JOIN_TOKEN` after it obtains a scoped worker token,
so normal job environments do not inherit the reusable join secret.

## Git exchange and hooks

Each project has a private bare exchange remote. Flow-managed refs include:

- the protected base branch;
- `refs/heads/issue/i-....` issue branches;
- coordinator-owned tags and future internal `refs/flow/*` refs.

Server-side `pre-receive` hooks enforce guardrails:

- protected base updates require owner or coordinator principal;
- non-fast-forward issue branch updates require coordinator principal;
- issue branch pushes are allowed from owner, worker, session, coordinator, or
  local same-user principals when they are fast-forward;
- unknown Flow-managed namespaces are rejected;
- `.flow/session/**` is not allowed on the protected base branch.

`post-receive` writes JSONL events to `exchange.git/flow-spool/post-receive.jsonl`.
The coordinator drains those events into SQLite and uses them to update change
heads, stale checks, and review-thread trailer claims. `flow reconcile` remains a
manual recovery path when git and SQLite drift.

Handoffs are not git artifacts. `flow ready` and `flow handoff write` submit them
to the coordinator, which stores snapshots in SQLite and injects them into later
prompts.

## Flows, agent definitions, and checks

Each project stores flow configuration in SQLite:

- **Agent definitions** combine a harness, optional model/reasoning selection,
  and prompt instructions.
- **Flows** define ordered work phases, optional human gates, review agents, and
  the fix agent.

Fresh projects seed built-in planner, author, reviewer, and verifier agent
definitions plus two flows:

- `direct`: implement immediately, then agent review before merge.
- `planned`: run a human-gated planning phase, then implementation and review.

Repo-versioned automated check configuration lives in `.flow/checks/*.yaml`.
Checks belong with the code; agent definitions and flows are coordinator-owned so
they can reflect live worker harness availability and can be edited through the
web UI or CLI.

## Authentication and credentials

Flow uses bearer tokens for API clients and short-lived cookies for the web UI:

- **Owner token**: human/admin CLI calls and web UI bootstrap.
- **Worker join token**: reusable secret a worker presents once to mint its
  scoped worker token.
- **Worker token**: scoped token for worker heartbeat, job claim/report, and
  transcript upload.
- **Session/console tokens**: scoped tokens injected into agent sessions.
- **Hook token**: exchange hook/coordinator integration.
- **Web session cookie + CSRF cookie/header**: browser UI authentication after a
  one-time `flow ui` bootstrap URL.

Token files must be private (`0600`) when read from disk. HTTP Git remotes use
bearer credentials stored through Git's credential helper when available.

## Web UI

`flow-server` serves the browser UI directly; there is no separate frontend
server. The implementation lives under `internal/web`:

- `internal/web/assets/*.js`: browser-native ES modules and custom elements.
- `internal/web/assets/index.html`: embedded shell page.
- `internal/web/src/app.module.css`: source CSS.
- `internal/web/webassetbuild`: small Go CSS scoping step used by embedded asset
  serving and tests.

The UI treats coordinator read models as the source of truth. It keeps a small
client-side cache for convenience, but issue lanes, review state, checks,
terminal state, and flow state are authoritative on the server.

## Source map

| Area | Packages/files |
| --- | --- |
| Commands | `cmd/flow`, `cmd/flow-server`, `cmd/flow-worker` |
| HTTP API and project registry | `internal/api` |
| API request/response contract aliases | `internal/api/contract` |
| CLI client plumbing | `internal/client` |
| Configuration loading and defaults | `internal/config` |
| Domain services | `internal/coordinator` |
| Lifecycle FSM, timers, inbox redelivery | `internal/lifecycle` |
| SQLite schema/migrations | `internal/db`, `internal/sqlitex` |
| Worker directory, queues, claims, leases | `internal/worker` |
| Worker checkout/tmux/entrypoint execution | `internal/worker/execution` |
| Git exchange, hooks, refs, merge helpers | `internal/git` |
| Harness definitions, hooks, model serialization | `internal/harness` |
| Prompt and handoff rendering/validation | `internal/prompt`, `internal/handoff`, `skills/` |
| ttyd/tmux terminal helpers | `internal/terminal` |
| Embedded web UI | `internal/web` |

## Development and verification

Common verification targets are documented in [development.md](development.md):

```sh
make test
make js-test
make web-smoke
make lifecycle-test
```

For changes that affect lifecycle transitions, worker execution, git exchange
hooks, or web routes, prefer a targeted package test first and then the broadest
relevant Make target.
