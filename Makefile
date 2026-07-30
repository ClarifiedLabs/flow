.DEFAULT_GOAL := build

.PHONY: build ci format install test js-test release web-smoke

COMMAND_PACKAGES := ./cmd/flow ./cmd/flow-server ./cmd/flow-worker ./cmd/flow-orchestrator
BINDIR ?= $(HOME)/bin
GO_TEST_P ?= 4

build:
	mkdir -p bin
	go build -o ./bin $(COMMAND_PACKAGES)

ci: test js-test

format:
	go fmt ./...

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/flow bin/flow-server bin/flow-worker bin/flow-orchestrator $(BINDIR)/

test:
	go test -p $(GO_TEST_P) ./...

# js-test runs the web UI's native-ESM Node tests (app.js is split into ES
# modules served as-is to the browser; these tests import them directly).
js-test:
	node --test internal/web/assets/app.test.mjs
	node --test internal/web/assets/elements.test.mjs
	node --test internal/web/assets/task-relations.test.mjs
	node --test internal/web/assets/tasks-view.test.mjs
	node internal/web/assets/harness_models.test.mjs

release:
ifndef VERSION
	$(error VERSION is required; use VERSION=patch|minor|major|x.y.z [AUTOPUSH=1])
endif
	scripts/release/check-clean.sh
	go build ./...
	go vet ./...
	$(MAKE) test
	VERSION="$(VERSION)" AUTOPUSH="$(AUTOPUSH)" scripts/release/tag.sh

web-smoke:
	FLOW_BROWSER_BIN="$(FLOW_BROWSER_BIN)" go test ./internal/api -run TestWebUI -count=1
