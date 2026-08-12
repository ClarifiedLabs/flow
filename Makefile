.DEFAULT_GOAL := build

.PHONY: build ci format install test js-test release web-smoke

COMMAND_PACKAGES := ./cmd/flow ./cmd/flow-server ./cmd/flow-worker ./cmd/flow-orchestrator
BINDIR ?= $(HOME)/bin
# Package-level test parallelism. The suite is dominated by tests that wait
# on subprocesses (git/tmux) rather than burn CPU, so oversubscribing package
# binaries relative to core count is a win on typical machines.
GO_TEST_P ?= 8

build:
	mkdir -p bin
	go build -tags sqlite_fts5 -o ./bin $(COMMAND_PACKAGES)

ci: test js-test

format:
	go fmt ./...

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/flow bin/flow-server bin/flow-worker bin/flow-orchestrator $(BINDIR)/

test:
	go test -short -p $(GO_TEST_P) -count=1 -timeout 10m ./...

# js-test runs the web UI's native-ESM Node tests (app.js is split into ES
# modules served as-is to the browser; these tests import them directly).
# `node --test <glob>` runs every `*.test.mjs` under assets/ recursively, each
# in its own subprocess. Helper modules keep non-matching names
# (test-dom.mjs, test-helpers.mjs) so they are never collected as tests —
# and never inside a directory named `test`, which node matches wholesale.
js-test:
	node --test "internal/web/assets/**/*.test.mjs"

release:
ifndef VERSION
	$(error VERSION is required; use VERSION=patch|minor|major|x.y.z [AUTOPUSH=1])
endif
	scripts/release/check-clean.sh
	go build -tags sqlite_fts5 ./...
	go vet ./...
	$(MAKE) test
	VERSION="$(VERSION)" AUTOPUSH="$(AUTOPUSH)" scripts/release/tag.sh

web-smoke:
	FLOW_BROWSER_BIN="$(FLOW_BROWSER_BIN)" go test ./internal/api -run TestWebUI -count=1
