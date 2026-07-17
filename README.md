# Flow

Flow is a local coordinator for issue-driven agent work. It keeps durable state
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

`flow-full` installs the CLI, server, and worker. Role-specific formulae are also
available: `clarifiedlabs/tap/flow`, `clarifiedlabs/tap/flow-server`, and
`clarifiedlabs/tap/flow-worker`.

### Linux packages

Download the `.deb` or `.rpm` for the current release from
[GitHub Releases](https://github.com/ClarifiedLabs/flow/releases).

### Docker

Flow publishes separate server and worker images:

```sh
docker pull ghcr.io/clarifiedlabs/flow-server:latest
docker pull ghcr.io/clarifiedlabs/flow-worker:latest
```

For Docker Compose and container auth details, see
[Detailed setup](docs/setup.md#docker-compose).

## Quickstart

Prerequisites for local package installs are Git, tmux, ttyd, and at least one
supported agent CLI on the worker `PATH` (Codex is the default; Claude Code and
Harness are also supported through Flow configuration).

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

Open the printed login URL, create a **New Issue**, leave the default `direct`
flow selected (or choose `planned` for a human-approved planning phase), and
queue the issue for a worker.

## Everyday use

Most users can stay in the web UI:

- **Board** shows work across all registered projects.
- **New Issue** creates work, attaches files, chooses a Flow, and optionally
  queues the issue immediately.
- **Issue detail** shows requirements, checks, changes, lifecycle history,
  attachments, and terminal links.
- **Feedback**, **Merge**, **Workers**, and **Jobs** show human waits, ready
  merges, worker capacity, and job diagnostics.
- **Flows** configures project-specific agent definitions and work/review flows.

Useful CLI shortcuts:

```sh
flow issue create --title "Implement feature" --flow direct
flow issue schedule i-0001 up_next
flow board
flow checks i-0001
flow transitions i-0001
flow workers
flow jobs
flow merge i-0001
```

## How Flow works

- `flow-server` owns the HTTP API, web UI, global worker registry, per-project
  service bundles, lifecycle engine, and git exchange hooks.
- `flow-worker` joins with a reusable worker join token, receives a scoped worker
  token, advertises harness labels/capacity, claims jobs, clones the issue branch
  from the exchange remote, and runs the job in tmux.
- `flow` is the human and in-session CLI for project onboarding, issue commands,
  prompts, handoffs, terminal attach, review threads, and merge.

Flow stores operational state under the Flow data directory. The global database
tracks projects, workers, tokens, and web sessions; each project has its own
SQLite database plus private bare `exchange.git` remote.

## Documentation

- [Detailed setup](docs/setup.md)
- [Usage reference](docs/usage.md)
- [Current architecture](docs/architecture.md)
- [Development guide](docs/development.md)
- [Design history](docs/flow-design.md)
- [Release process](docs/release.md)
