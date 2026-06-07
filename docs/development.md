# Development

This guide covers local build, test, and asset generation commands for Flow
contributors.

## Build

Build the commands:

```sh
make build
```

Install the locally built commands:

```sh
make install BINDIR="$HOME/bin"
```

Use the `BINDIR` that appears first on the workers' `PATH`. Prompt and worker
behavior is compiled into the local `flow`, `flow-server`, and `flow-worker`
binaries. After changing prompt construction, worker environment setup, or role
instructions, rebuild and reinstall the binaries that your workers use, then
restart any long-running `flow-server` and `flow-worker` processes before
launching new jobs.

## Test

Run the default CI target:

```sh
make ci
```

Run the Go test suite:

```sh
make test
```

Run the web UI's native-ESM Node tests:

```sh
make js-test
```

Run the local lifecycle integration and E2E tests:

```sh
make lifecycle-test
```

The lifecycle tests build local binaries, start `flow-server`, onboard two
throwaway git repositories as separate projects, and drive `flow-worker` through
tmux for both. The browser E2E test uses Chrome or Chromium when available; set
`FLOW_BROWSER_BIN=/path/to/chrome` if it is not on a standard path.

Run the isolated web UI smoke test:

```sh
make web-smoke
```

## Web Assets

Regenerate embedded web assets after editing `internal/web/src/app.module.css`:

```sh
go generate ./internal/web
```

The web app uses browser-native custom elements, a small JavaScript module
graph, and CSS Modules generated into embedded assets served by
`flow-server`.

## Design And Release Docs

Architecture and design details live in [flow-design.md](flow-design.md).
Release packaging and tagging details live in [release.md](release.md).
