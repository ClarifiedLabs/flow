# Setup

This guide covers local binary setup, Docker setup, project onboarding, owner
tokens, worker configuration, and terminal attach.

## Prerequisites

- Go 1.26.4 or newer.
- Git.
- tmux for worker jobs.
- Codex on `PATH` for the default author agent entrypoint.
- ttyd on `PATH` for worker terminal attach.

## Local Binaries

Build the three commands and put them on `PATH`:

```sh
make build
export PATH="$PWD/bin:$PATH"
```

Create private owner and worker join tokens. The owner token authorizes
human/admin CLI calls and web UI bootstrap. The server accepts the reusable
worker join token from workers and issues each joined worker its own worker
token.

```sh
mkdir -p .flow-local
openssl rand -hex 32 > .flow-local/owner.token
openssl rand -hex 32 > .flow-local/worker-join.token
chmod 600 .flow-local/owner.token
chmod 600 .flow-local/worker-join.token
```

Start the coordinator in terminal 1:

```sh
flow-server serve \
  --owner-token-file .flow-local/owner.token \
  --worker-join-token-file .flow-local/worker-join.token
```

On startup `flow-server serve` opens the local Flow data dir
(`$FLOW_DATA_DIR` when set, otherwise `${XDG_DATA_HOME:-$HOME/.local/share}/flow`),
uses the supplied owner token as the active owner credential, creates the hook
token file, writes `$XDG_CONFIG_HOME/flow/config.yaml` for local CLI discovery,
opens every already-registered project, and prints the paths it manages:

```text
database: /path/to/flow/global.db
projects: 0
owner_token_file: /path/to/repo/.flow-local/owner.token
hook_token_file: /path/to/flow/hook.token
exchange_base_url: http://127.0.0.1:8421
client_config_file: /path/to/config/flow/config.yaml
```

Token files must be mode `0600`; do not commit them. The worker config is
supplied separately. From a source checkout, use `examples/flow-worker.yaml` as
the local starting point. Package installs also include these examples under
their package share directory.

Start the global worker in terminal 2:

```sh
# Use /usr/share/flow/examples/flow-worker.yaml for Linux packages,
# "$(brew --prefix flow-worker)/share/flow-worker/examples/flow-worker.yaml" for Homebrew,
# or examples/flow-worker.yaml from a source checkout.
cp examples/flow-worker.yaml .flow-local/worker.yaml
FLOW_WORKER_JOIN_TOKEN="$(tr -d '\r\n' < .flow-local/worker-join.token)" \
  flow-worker -c .flow-local/worker.yaml
```

The example worker config has worker id `w-local`, five persistent-agent slots,
five ephemeral slots, and no long-lived worker token. On startup `flow-worker`
uses `FLOW_WORKER_JOIN_TOKEN` to join the server, receives a scoped worker
token, clears the join token from its environment, and registers itself.

Onboard a repository in terminal 3 while the server is running:

```sh
cd /path/to/your/repo
flow init
```

`flow init` registers the project through the running coordinator, adds the
`flow` exchange remote, pushes the base-branch seed, and refreshes
`$XDG_CONFIG_HOME/flow/config.yaml` so later commands usually need no
`--server` or `--token` flags. Re-running it is idempotent.

Open the web UI from inside any registered repository:

```sh
flow ui | tee /tmp/flow-ui-url.txt
```

On macOS:

```sh
open "$(tr -d '\r\n' < /tmp/flow-ui-url.txt)"
```

## Docker Compose

The Docker setup runs `flow-server` and `flow-worker` as separate non-root
Debian Trixie-based containers with separate server and worker volumes:

```sh
cp .env.example .env
sed -i.bak \
  -e "s/^FLOW_OWNER_TOKEN=.*/FLOW_OWNER_TOKEN=$(openssl rand -hex 32)/" \
  -e "s/^FLOW_WORKER_JOIN_TOKEN=.*/FLOW_WORKER_JOIN_TOKEN=$(openssl rand -hex 32)/" \
  .env
docker compose up -d --build
```

The server stores coordinator state in `flow-data`. The worker stores its work
directory in `flow-worker-data` and its rootless Docker state in
`flow-worker-docker`; it does not mount the server data volume.

The `flow-server` image stays minimal. The `flow-worker` image includes
`claude`, `codex`, `harness`, Go, nvm with Node.js LTS, Rust, Temurin JDK,
LLVM/clang/lld, build tools, Docker CLI/Compose/buildx, rootless
Docker-in-Docker, age, GnuPG, OpenSSH, Python, GitHub CLI, and common
build/debug utilities. Each pinned third-party package version and
architecture-specific SHA256 is declared as a Docker build argument in
`Dockerfile`.

The worker starts a rootless Docker daemon by default and advertises
`docker=true` only after `docker info` succeeds. It rewrites the public host
exchange URL to the internal Compose URL for worker-side Git clones while
leaving host-facing project remotes pointed at `http://127.0.0.1:8421`.

To run Docker-hosted agents with Claude and Codex subscription auth, add the
auth values to `.env` and use the auth compose overlay:

```sh
FLOW_OWNER_TOKEN=generated-hex-token
FLOW_WORKER_JOIN_TOKEN=generated-hex-token
FLOW_CODEX_AUTH_JSON=/absolute/path/to/your/.codex/auth.json
CLAUDE_CODE_OAUTH_TOKEN=your-claude-code-oauth-token
```

For Codex, sign in on the host with file-backed credentials before starting the
worker. Make sure your host `~/.codex/config.toml` contains:

```toml
cli_auth_credentials_store = "file"
```

Then run:

```sh
codex login --device-auth
codex login status
```

For Claude Code, generate the subscription token on the host:

```sh
claude setup-token
```

Put the printed token in `.env` as `CLAUDE_CODE_OAUTH_TOKEN`, then start Flow
with the auth overlay:

```sh
docker compose -f compose.yaml -f compose.auth.yaml up -d --build
```

The Codex auth file is mounted writable because Codex refreshes ChatGPT
subscription tokens during normal use and writes the updated token bundle back
to `auth.json`. Keep `.env` and `auth.json` private; `.env` is ignored by git.

Load the generated owner token from `.env`, then onboard a repository from your
normal host checkout:

```sh
set -a
. ./.env
set +a

cd /path/to/your/repo
flow init --server http://127.0.0.1:8421 --token "$FLOW_OWNER_TOKEN"
```

`flow init` registers a Docker-hosted HTTP Git exchange remote and stores a
path-scoped Git credential through your configured Git credential helper. If no
helper is configured, `flow init` prints the `git credential approve` command to
run after you configure one. The token is not written into the repository's Git
config.

## What Init Creates

`flow init` is the single command for onboarding a repository. It runs against a
running `flow-server`. It does not write role-skill files into your repository,
rewrite your project, or replace `origin`. Through the server it:

- Creates a per-project SQLite database at
  `<data-dir>/projects/<project-id>/flow.db`.
- Creates a private bare exchange remote at
  `<data-dir>/projects/<project-id>/exchange.git`.
- Adds that exchange remote to the worktree as `flow` unless you pass another
  `--exchange-name`.
- Seeds only the configured base branch into the exchange remote.
- Installs exchange-remote hooks.
- Stores HTTP exchange credentials through Git's credential helper when the
  exchange URL is HTTP(S) and an owner token is available.
- Refreshes the client config at `$XDG_CONFIG_HOME/flow/config.yaml` with the
  server URL, data dir, and owner credential.

The base branch defaults to the worktree's current branch; override it with
`--base`. The owner token used for registration is resolved from `--token`, then
session/worker/owner token environment variables, then client config, then
`<data-dir>/owner.token`.

## Owner Token

`OWNER_TOKEN` is the owner-scoped bearer token for human/admin API calls. Owner
commands include issue scheduling, board reads, worker/job diagnostics, merge,
and web UI bootstrap.

For the normal local setup above, owner commands read the token from
`$XDG_CONFIG_HOME/flow/config.yaml`, which references `.flow-local/owner.token`.
If you omit `--owner-token-file`, `flow-server serve` creates and uses
`<data-dir>/owner.token`. To inspect that fallback token manually:

```sh
export OWNER_TOKEN="$(tr -d '\r\n' < "${FLOW_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/flow}/owner.token")"
```

Owner token files must use mode `0600`. Do not commit them. The owner token
selected at `flow-server serve` startup is stored as the active owner
credential; replacing the token file or `--owner-token` value and restarting the
coordinator rotates the owner credential and revokes previous live owner
tokens.

## Worker Setup

One worker can serve every project. Start from the example config and pass a
join token through the environment:

```sh
FLOW_WORKER_JOIN_TOKEN="$(tr -d '\r\n' < .flow-local/worker-join.token)" \
  flow-worker -c .flow-local/worker.yaml
```

Each job payload carries the exchange URL, project id, and project name of its
owning project, so the worker clones that job's exchange into its `work_dir`,
checks out the per-job branch, and runs the job in tmux.

The example `worker.yaml` configures one local worker and deliberately omits the
long-lived worker token:

```yaml
worker_id: w-local
coordinator_url: http://127.0.0.1:8421
labels:
  local: "true"
capacity:
  persistent_agent: 5
  ephemeral: 5
```

At registration time, `flow-worker` probes its environment and advertises agent
harness capabilities as labels:

- `agent.harness.codex: "true"` when `codex login status` passes.
- `agent.harness.claude: "true"` when `claude auth status` passes.
- `agent.harness.harness: "true"` when `harness --check-model-proxy` passes.
- `capacity.persistent_agent` controls concurrent author, reviewer, and
  verifier agent jobs.
- `capacity.ephemeral` controls concurrent CI/check jobs.

Capacity is the concurrency limit for this configured worker. A single
`flow-worker` process starts one internal claim loop per configured slot, so
`persistent_agent: 1` and `ephemeral: 2` can run up to three jobs at the same
time across all projects. Use separate configs with distinct `worker_id` values
when you want separate labels, capacity, credentials, hosts, or work
directories.

Edit the YAML to change labels, capacity, `coordinator_url`, `work_dir`, or
terminal settings. Keep the join token private; it can be reused to start more
workers, but anyone with it can mint a worker token for a configured worker id.
Use a distinct `worker_id` for each concurrent worker; joining with an existing
`worker_id` rotates that worker's previous token.

```sh
chmod 600 .flow-local/worker-join.token
```

Issues default to Codex and can be set to Claude Code or Harness with
`flow issue create --agent-harness claude|harness` or
`flow issue edit --agent-harness claude|harness`. The web UI exposes the same
Agent field on the issue form and only lists Codex, Claude Code, and Harness
when at least one live worker has advertised the corresponding
`agent.harness.*` label.

The worker environment needs `flow`, `ttyd`, and the selected harness CLI
(`codex`, `claude`, or `harness`) available on `PATH`. To use a different
author command, serve with a coordinator config that overrides
`author_entrypoint`.

Example coordinator config fragment:

```yaml
author_entrypoint:
  argv: ["claude"]
  cwd: "."
  env: {}
  shell: false
```

## Lifecycle Deadlines

The coordinator arms durable timers that bound otherwise-unobservable waits: a
hung-but-heartbeating planning/authoring session and a check job that never
reports. The timers ride the same at-least-once claim/confirm machinery as
auto-merge retries and fire from the background lifecycle ticker.

- `deadlines.check_pending` defaults to `30m`. When a check stays `pending`
  past this window with no report, the coordinator reports it `blocked` with
  details `timed out after <window>`.
- `deadlines.authoring_stall` defaults to `2h`. When an issue sits in
  `planning` or `authoring` for this long with no agent activity, the
  coordinator reports a non-required `phase-deadline` check as `blocked` and
  writes a blocker status entry.

Each value is a Go duration string. An empty or omitted value takes the default;
`"0"` disables that deadline.

```yaml
deadlines:
  check_pending: "30m"
  authoring_stall: "2h"
```

## Remote Workers

For a remote worker, use exchange remotes and a coordinator URL that the remote
machine can reach. `flow-server serve` stamps HTTP exchange URLs on job payloads
by default; set `exchange_base_url` or `--exchange-base-url` to the address
remote machines should use.

A remote worker does not need its own checkout of your project, but it does need
a worker config with a unique `worker_id`, a reachable `coordinator_url`, an
appropriate `work_dir`, and either `FLOW_WORKER_JOIN_TOKEN` or an existing
worker token in the config.

Terminal attach is required for workers. Same-machine browser attach works when
`ttyd` is installed and the coordinator URL is loopback. Remote browser attach
requires:

```yaml
terminal:
  bind_address: 100.64.1.2
  public_base_url: http://100.64.1.2
```

Use a private or tailnet address that the coordinator can reach.

## Web Terminal

The browser terminal runs the agent inside a tmux session exposed over ttyd.
Each job's tmux session is configured with mouse on, a 100k-line history limit,
and `set-clipboard on`.

- Copy text by holding Shift while dragging to make a native browser selection,
  then press Ctrl/Cmd+C.
- Scroll with the mouse wheel.
- Press Ctrl-b [ to enter tmux copy mode and move through the full pane
  history; press q to leave copy mode.
