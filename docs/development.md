# Development

This guide covers local build, test, and web asset commands for Flow
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

All canonical test targets run with an empty inherited environment, temporary
home and XDG directories, and external Go and Git configuration disabled. For
a targeted test, use the same isolation wrapper directly:

```sh
scripts/test-env.sh go test ./internal/config -run TestLoadClient -count=1
```

When a test intentionally needs an environment variable, inject it explicitly
inside the isolated environment:

```sh
scripts/test-env.sh env FLOW_BROWSER_BIN=/path/to/chrome go test ./internal/api -run TestWebUIBrowserSmokeRoutesAndDeepLinks -count=1
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

The web app uses browser-native custom elements, browser-native ES modules, and
`internal/web/src/app.module.css`. `flow-server` embeds the source files and the
Go helper in `internal/web/webassetbuild` scopes the CSS at serve/test time, so
there is no separate asset generation step after CSS or JavaScript edits.

Run the native-ESM tests after web UI changes:

```sh
make js-test
```

Run the browser smoke test for route and deep-link coverage when the change is
routing- or DOM-sensitive:

```sh
make web-smoke
```

## Architecture, Design, And Release Docs

Current architecture details live in [architecture.md](architecture.md). The
longer historical design narrative lives in [flow-design.md](flow-design.md).
Release packaging and tagging details live in [release.md](release.md).
