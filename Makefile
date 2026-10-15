GO ?= go
# golangci-lint runs from its own module, pinned in tools/go.mod.
GOLANGCI_LINT := $(GO) tool -modfile=tools/go.mod golangci-lint
# make lint reports only issues new since the merge base with this ref.
LINT_BASE ?= develop
CONFIG ?= config/bbb-webrtc-recorder.yml
APP_VERSION := $(shell sed 's/ /-/g' VERSION)
LDFLAGS := -X 'github.com/bigbluebutton/bbb-webrtc-recorder/internal.AppVersion=v$(APP_VERSION)'

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show the available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the recorder into build/
	$(GO) build -ldflags="$(LDFLAGS)" -o build/bbb-webrtc-recorder ./cmd/bbb-webrtc-recorder

.PHONY: run
run: ## Run the recorder (needs Redis; CONFIG= overrides the config file)
	$(GO) run -ldflags="$(LDFLAGS)" ./cmd/bbb-webrtc-recorder -c $(CONFIG)

.PHONY: test
test: ## Run the unit tests with -race, as CI does
	$(GO) test -race ./...
	cd third_party/media-sdk && $(GO) test -race ./...

.PHONY: test-e2e
test-e2e: ## Run the e2e suite (needs livekit-server and redis-server; see tests/e2e/README.md)
	$(GO) test -tags e2e -v -timeout 15m ./tests/e2e/...

.PHONY: lint
lint: ## Lint what is new since the merge base with develop (LINT_BASE=), as CI does
	$(GOLANGCI_LINT) run --build-tags e2e --new-from-merge-base $(LINT_BASE) ./...

.PHONY: lint-all
lint-all: ## Lint the whole tree, including issues that predate a change
	$(GOLANGCI_LINT) run --build-tags e2e ./...

.PHONY: format
format: ## Format the code with the formatters in .golangci.yml
	$(GOLANGCI_LINT) fmt ./...
