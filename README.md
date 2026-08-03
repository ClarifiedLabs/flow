# Flow

Flow is a local coordinator for task-driven agent work. It keeps durable state
in SQLite, creates a private git exchange remote for worker branches, runs agent
jobs in tmux-backed workers, and serves a browser UI for creating, reviewing,
and merging work.

One `flow-server` can manage many repositories. Each repository becomes a Flow
project with its own database and exchange remote, while a single `flow-worker`
can execute jobs across all registered projects.

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

Docker Compose provides explicit `legacy-workers` for compatibility; new local
stacks should use the [Local Kind quickstart](docs/kubernetes.md#local-kind-quickstart).
See [Detailed setup](docs/setup.md) and
[Kubernetes operations](docs/kubernetes.md) for assignment-created one-shot
worker Jobs, private credentials, probes, and recovery.

## Quickstart

Prerequisites for local package installs are Git, tmux, and the `harness`
agent CLI on the worker `PATH`.

Start the coordinator:

```sh
mkdir -p .flow-local
openssl rand -hex 32 > .flow-local/owner.token
openssl rand -hex 32 > .flow-local/worker-join.token
chmod 600 .flow-local/owner.token .flow-local/worker-join.token

flow-server serve \
  --owner-token-file .flow-local/owner.token \
  --worker-join-token-file .flow-local/worker-join.token
```

In another terminal, start a worker from the packaged example config:

```sh
# Homebrew example:
# "$(brew --prefix flow-worker)/share/flow-worker/examples/flow-worker.yaml"
# Linux package example: /usr/share/flow/examples/flow-worker.yaml
cp /path/to/flow-worker.yaml .flow-local/worker.yaml

FLOW_WORKER_JOIN_TOKEN="$(tr -d '\r\n' < .flow-local/worker-join.token)" \
  flow-worker -c .flow-local/worker.yaml
```

Register the git repository you want Flow to manage. The repository must already
have at least one commit.

```sh
cd /path/to/your/repo
flow init
```

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
  merges, worker capacity, and job diagnostics.
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
- Managed `flow-worker` processes receive direct assignment-scoped credentials,
  exact-claim that job, clone its task branch, run it in tmux, and exit. Reusable
  join credentials remain available for compatibility workers.
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
