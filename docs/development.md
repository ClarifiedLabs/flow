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

Go tests isolate themselves: every test package's TestMain routes through
`internal/testenv`, which clears the inherited environment, substitutes
temporary home and XDG directories, and disables external Go and Git
configuration before any test runs. Plain `go test` is therefore safe for
targeted runs:

```sh
go test ./internal/config -run TestLoadClient -count=1
```

Variables a test intentionally consumes (currently `FLOW_BROWSER_BIN`) are
preserved through the isolation layer, so they can be passed the ordinary way:

```sh
FLOW_BROWSER_BIN=/path/to/chrome go test ./internal/api -run TestWebUI -count=1
```

Run the web UI's native-ESM Node tests:

```sh
make js-test
```

Run the browser smoke tests, which drive the real UI through Chrome or
Chromium; set `FLOW_BROWSER_BIN=/path/to/chrome` if it is not on a standard
path:

```sh
make web-smoke
```

## Web Assets

The web app is built from hand-written custom elements in
`internal/web/assets/elements/`, browser-native ES modules, and one stylesheet
per component in `internal/web/src/`. `flow-server` embeds the source files and
the Go helper in `internal/web/webassetbuild` scopes each stylesheet to its own
element at serve/test time, so there is no separate asset generation step after
CSS or JavaScript edits.

The module layout under `internal/web/assets/`:

- `app.js`: the entry module and composition root — `FlowApp` wiring, the shell,
  routing and polling. Its collaborators live in `app/`: `routes.js` (the route
  table), `settle.js` (the settle-burst state machine), `sidebar.js` (the
  `/v2/sidebar` poll loop), `caches.js` (the per-project caches).
- `*-route.js`: thin routes that fetch and `mount()` an element.
- `elements/*.js`: one custom element per UI island, plus their view helpers
  (e.g. `flows-view.js`, `flow-editor-view.js`, `task-detail-view.js`). Each
  element renders from a pure function that returns an HTML string, so markup
  is testable in Node without a DOM; the element class is a thin shell over it.
- `models/`: pure projections — task-run, review, relations, lifecycle options,
  now-card, and the harness catalog/selection form helpers.
- `actions/`: the delegated action table — `registry.js` (in-flight/busy
  machinery), `dispatch.js` (the dispatcher), and one domain table per area
  (workflow, review, features, console, relations). `actions.js` is the facade
  that merges the tables.
- `src/*.module.css`: one sheet per element, scoped by tag name; the flow-app
  chrome is split into domain sheets (`chrome`, `nav`, `shared`, `detail`,
  `forms`, `markdown`, `terminal`, `timeline`, `charts`, `diff-tables`).
- Tests: `node --test` runs every `*.test.mjs` under `assets/` (`make js-test`).
  The shared harness lives in `test-helpers.mjs` (the scriptContext sandbox,
  smoke DOM stubs) and `test-dom.mjs` (the hand-written DOM); `test-surface.mjs`
  holds the re-exports the scriptContext tests consume. Lifecycle behaviour
  that needs a DOM is tested against `test-dom.mjs`.

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
