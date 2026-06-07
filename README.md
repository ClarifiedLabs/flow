# Flow

Flow is a local coordinator for issue-driven agent work. It keeps durable state
in SQLite, uses a private git exchange remote for worker branches, runs agent
jobs in tmux-backed workers, and serves a browser UI for managing the work.

One `flow-server` can serve many projects. Each project gets its own database
and exchange remote, while one worker can execute jobs across all registered
projects.

## Quickstart

Install a release build with one of the supported package paths.

### Homebrew

```sh
brew install clarifiedlabs/tap/flow-full
```

`flow-full` installs the CLI, server, and worker packages. For role-specific
installs, use `clarifiedlabs/tap/flow`, `clarifiedlabs/tap/flow-server`, or
`clarifiedlabs/tap/flow-worker`.

### Linux Packages

Download the `.deb` or `.rpm` for the current release from
[GitHub Releases](https://github.com/ClarifiedLabs/flow/releases).

Debian or Ubuntu:

```sh
VERSION=vX.Y.Z
ARCH="$(dpkg --print-architecture)" # amd64 or arm64
curl -LO "https://github.com/ClarifiedLabs/flow/releases/download/${VERSION}/flow_${VERSION#v}_${ARCH}.deb"
sudo apt install "./flow_${VERSION#v}_${ARCH}.deb"
```

Fedora, RHEL, or compatible distributions:

```sh
VERSION=vX.Y.Z
ARCH="$(uname -m)" # x86_64 or aarch64
sudo dnf install "https://github.com/ClarifiedLabs/flow/releases/download/${VERSION}/flow-${VERSION#v}-1.${ARCH}.rpm"
```

### Docker Containers

Use `latest` for the newest published release, or replace it with a pinned
release tag such as `vX.Y.Z`.

```sh
docker pull ghcr.io/clarifiedlabs/flow-server:latest
docker pull ghcr.io/clarifiedlabs/flow-worker:latest

docker network create flow
docker volume create flow-data
docker volume create flow-worker-data
docker volume create flow-worker-docker
FLOW_OWNER_TOKEN="$(openssl rand -hex 32)"
FLOW_WORKER_JOIN_TOKEN="$(openssl rand -hex 32)"

docker run -d --name flow-server --network flow \
  -p 127.0.0.1:8421:8421 \
  -v flow-data:/var/lib/flow \
  ghcr.io/clarifiedlabs/flow-server:latest \
  flow-server serve \
    --config /usr/share/flow/examples/docker/flow-server.yaml \
    --exchange-base-url http://127.0.0.1:8421 \
    --owner-token "$FLOW_OWNER_TOKEN" \
    --worker-join-token "$FLOW_WORKER_JOIN_TOKEN"

docker run -d --name flow-worker --network flow --privileged \
  --tmpfs /run/user/1000:uid=1000,gid=1000,mode=700 \
  -v flow-worker-data:/home/flow/.local/share/flow \
  -v flow-worker-docker:/home/flow/.local/share/docker \
  -e FLOW_WORKER_JOIN_TOKEN="$FLOW_WORKER_JOIN_TOKEN" \
  -e FLOW_WORKER_COORDINATOR_URL=http://flow-server:8421 \
  -e FLOW_WORKER_DOCKERD=auto \
  -e FLOW_WORKER_GIT_URL_REWRITE_FROM=http://127.0.0.1:8421 \
  -e FLOW_WORKER_GIT_URL_REWRITE_TO=http://flow-server:8421 \
  -e FLOW_WORKER_TERMINAL_PUBLIC_BASE_URL=auto \
  -e FLOW_WORKER_TERMINAL_BIND_ADDRESS=0.0.0.0 \
  ghcr.io/clarifiedlabs/flow-worker:latest \
  flow-worker -c /usr/share/flow/examples/docker/flow-worker.yaml
```

The owner token authorizes human/admin CLI calls and web UI login bootstrap.
Changing the owner token value and restarting `flow-server` rotates the active
owner credential. The join token is reusable for additional workers. Each
worker presents it once at startup to receive its own worker token; the worker
token is not stored in a shared server volume. Use a distinct `worker_id` for
each concurrent worker; joining with an existing `worker_id` rotates that
worker's previous token.

For Homebrew or Linux package installs, prerequisites are Git, tmux, Codex on
`PATH`, and ttyd on `PATH`. Start Flow with two long-running processes.

```sh
# Terminal 1
mkdir -p .flow-local
openssl rand -hex 32 > .flow-local/owner.token
openssl rand -hex 32 > .flow-local/worker-join.token
chmod 600 .flow-local/owner.token
chmod 600 .flow-local/worker-join.token
flow-server serve \
  --owner-token-file .flow-local/owner.token \
  --worker-join-token-file .flow-local/worker-join.token
```

```sh
# Terminal 2
# Use /usr/share/flow/examples/flow-worker.yaml for Linux packages,
# "$(brew --prefix flow-worker)/share/flow-worker/examples/flow-worker.yaml" for Homebrew,
# or examples/flow-worker.yaml from a source checkout.
cp examples/flow-worker.yaml .flow-local/worker.yaml
FLOW_WORKER_JOIN_TOKEN="$(tr -d '\r\n' < .flow-local/worker-join.token)" \
  flow-worker -c .flow-local/worker.yaml
```

In the git repository you want Flow to manage, run `flow init`. The repo must
already have at least one commit. For Homebrew or Linux package installs on the
same machine as `flow-server`, the local client config discovers the owner
token:

```sh
cd /path/to/your/repo
flow init
```

For Docker, pass the owner token generated before `docker run`:

```sh
cd /path/to/your/repo
flow init --server http://127.0.0.1:8421 --token "$FLOW_OWNER_TOKEN"
```

Open the web UI:

```sh
flow ui | tee /tmp/flow-ui-url.txt
```

On macOS:

```sh
open "$(tr -d '\r\n' < /tmp/flow-ui-url.txt)"
```

In the browser, click **New Issue**, choose a project, fill in the title/body
and acceptance criteria, leave **Queue after creation** checked, and submit the
form. Flow will place the issue in the queue for an available worker.

## Basic Web UI Usage

The web UI is served by `flow-server`; there is no separate web server to start.
Use `flow ui` to create a short-lived browser login URL.

- **Board** shows issues across registered projects. When more than one project
  is registered, use the topbar project picker to filter the board.
- **New Issue** creates work items, selects the agent harness, attaches initial
  files, and optionally queues the issue immediately.
- **Issue detail** shows the issue body, acceptance criteria, checks, changes,
  attachments, lifecycle timeline, and active human-attention items.
- **Terminal** buttons open live tmux sessions for running agent jobs when a
  terminal is available.
- **Feedback**, **Merge**, **Workers**, and **Jobs** show human waits, ready
  merges, worker capacity, and job diagnostics.

## Documentation

- [Detailed setup](docs/setup.md)
- [Usage reference](docs/usage.md)
- [Development docs](docs/development.md)
- [Architecture and design](docs/flow-design.md)
- [Release process](docs/release.md)
