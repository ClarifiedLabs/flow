.DEFAULT_GOAL := build

.PHONY: build ci format install test js-test lifecycle-test release web-smoke

COMMAND_PACKAGES := ./cmd/flow ./cmd/flow-server ./cmd/flow-worker
BINDIR ?= $(HOME)/bin
GO_TEST_P ?= 4
TEST_ENV := ./scripts/test-env.sh

build:
	mkdir -p bin
	go build -o ./bin $(COMMAND_PACKAGES)

ci: test js-test

format:
	go fmt ./...

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/flow bin/flow-server bin/flow-worker $(BINDIR)/

test:
	$(TEST_ENV) go test -p $(GO_TEST_P) ./...

# js-test runs the web UI's native-ESM Node tests (app.js is split into ES
# modules served as-is to the browser; these tests import them directly).
js-test:
	$(TEST_ENV) node --test internal/web/assets/app.test.mjs
	$(TEST_ENV) node internal/web/assets/harness_models.test.mjs

lifecycle-test:
	$(TEST_ENV) go test ./tests/lifecycle -count=1

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
	$(TEST_ENV) /usr/bin/env FLOW_BROWSER_BIN="$(FLOW_BROWSER_BIN)" \
		go test ./internal/api -run TestWebUIBrowserSmokeRoutesAndDeepLinks -count=1
