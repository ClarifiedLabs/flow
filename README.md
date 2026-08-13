# Flow

Flow is a local coordinator for task-driven agent work. It keeps durable state
in SQLite, creates a private git exchange remote for worker branches, runs agent
jobs in tmux-backed workers, and serves a browser UI for creating, reviewing,
and merging work.

One `flow-server` can manage many repositories. Each repository becomes a Flow
project with its own database and exchange remote. `flow-orchestrator` reserves
queued jobs and creates one short-lived `flow-worker` process for each assignment.

## Install

### Homebrew

```sh
brew install clarifiedlabs/tap/flow-full
```

`flow-full` installs the CLI, server, worker, and assignment orchestrator.
Role-specific formulae are also available: `clarifiedlabs/tap/flow`,
`clarifiedlabs/tap/flow-server`, `clarifiedlabs/tap/flow-worker`, and
`clarifiedlabs/tap/flow-orchestrator`.

### Linux packages

Download the `.deb` or `.rpm` for the current release from
[GitHub Releases](https://github.com/ClarifiedLabs/flow/releases).

### Docker

Flow publishes separate server, worker, and orchestrator images:

```sh
docker pull ghcr.io/clarifiedlabs/flow-server:latest
docker pull ghcr.io/clarifiedlabs/flow-worker:latest
docker pull ghcr.io/clarifiedlabs/flow-orchestrator:latest
```

Docker Compose provides a server-only local control plane. Use the
[Local Kind quickstart](docs/kubernetes.md#local-kind-quickstart) for a complete
assignment-based stack. See [Detailed setup](docs/setup.md) and
[Kubernetes operations](docs/kubernetes.md) for assignment-created one-shot
worker Jobs, private credentials, probes, and recovery.

## Quickstart

Prerequisites for local package installs are Git, tmux, and the `harness`
agent CLI on the worker `PATH`.

Start the coordinator and an assignment provider as described in
[Detailed setup](docs/setup.md#local-binaries). The provider reserves one exact
queued job, launches one Kubernetes Job or Darwin process with short-lived
assignment-scoped credentials, and runs:

```sh
flow-worker run --one-shot --config PATH
```

The worker exact-claims its reserved job, reports the result, and exits. The
orchestrator recovers durable assignments before reserving new work, so preserve
its state and the coordinator databases across restarts.

Register the git repository you want Flow to manage. The repository must already
have at least one commit.

```sh
cd /path/to/your/repo
flow init
```

If the Flow remote or saved Git credential is lost, run `flow init --repair`.
To attach another clone to the same project without creating or seeding a new
one, run `flow init --project PROJECT_ID_OR_NAME`.

Open the web UI:

```sh
flow ui
```

Open the printed login URL, create a **New Task**, leave the default `direct`
flow selected (or choose `planned` for a human-approved planning phase), and
queue the task for a worker.

## Everyday use

Most users can stay in the web UI:

- **Board** shows work across all registered projects.
- **New Task** creates work, attaches files, chooses a Flow, and optionally
  queues the task immediately.
- **Task detail** shows requirements, checks, changes, lifecycle history,
  attachments, and terminal links. Its **Review** tab answers human gates:
  it renders the artifact under review (a plan's summary and task list, or a
  link to the change), a feedback box, and one button per gate outcome. In
  the `planning` flow the planning agent holds its session open during the
  review — comments are delivered to it and you can open its terminal — so
  the plan is refined in one conversation instead of fresh runs.
- **Feedback**, **Merge**, **Workers**, and **Jobs** show human waits, ready
  merges, assignment workers, and job diagnostics.
- **Flows** configures coordinator-global and project-specific agent definitions,
  plus project work/review flows.
- **Features** groups a set of tasks behind one long-lived feature branch.
  Tasks assigned to a feature merge back into the feature branch instead of
  the base branch; the feature branch can be rebased onto the base branch on
  demand (agent-assisted on conflicts) and eventually landed as one squash
  merge. Features can nest: a child branches from, rebases onto, and lands into
  its nearest parent feature. The CLI mirrors the page:
  `flow feature create|list|show|edit|start|rebase|land|archive`.
- **Epics** are first-class non-executable containers with automatic or manual
  completion. Use `flow epic create|list|show|edit|start|complete|reopen|archive`
  and `flow work-item tree|link|unlink|relations` to manage the unified
  task/epic/feature hierarchy. `flow doctor work-items` validates registry,
  hierarchy, blocker-cycle, feature-scope, and subtype consistency.

Flow derives readable IDs from the project name: `--name "My Project"` creates
project `p-my-project`, and its tasks are `t-my-project-0001`,
`t-my-project-0002`, and so on. Full task IDs work from any directory.

Useful CLI shortcuts:

```sh
flow task create --title "Implement feature" --flow direct
flow task schedule t-my-project-0001 up_next
flow board
flow checks t-my-project-0001
flow transitions t-my-project-0001
flow workers
flow jobs
flow merge t-my-project-0001
```

## How Flow works

- `flow-server` owns the HTTP API, web UI, global worker registry, per-project
  service bundles, lifecycle engine, and git exchange hooks.
- `flow-orchestrator` reserves durable assignments and creates exactly one
  one-shot worker runtime for each selected job.
- Managed `flow-worker` processes receive short-lived assignment-scoped
  credentials, run as `flow-worker run --one-shot --config PATH`, exact-claim the
  reserved job, clone its task branch, run it in tmux, report, and exit.
- `flow` is the human and in-session CLI for project onboarding, task commands,
  prompts, handoffs, terminal attach, review threads, and merge.

Flow stores operational state under the Flow data directory. The global database
tracks projects, workers, tokens, web sessions, and inherited agent definitions;
each project has its own SQLite database plus private bare `exchange.git` remote.

## Documentation

- [Detailed setup](docs/setup.md)
- [Usage reference](docs/usage.md)
- [Current architecture](docs/architecture.md)
- [Kubernetes operations](docs/kubernetes.md)
- [Development guide](docs/development.md)
- [Design history](docs/flow-design.md)
- [Release process](docs/release.md)
