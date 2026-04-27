# Signatory Makefile.
#
# Go's built-in tooling covers most of what we need — `go test`, `go build`,
# `go run` are short enough on their own that wrapping them as aliases is
# more noise than signal. This Makefile exists for two concrete wins:
#
#   1. `make install` stamps the real build version via ldflags, so
#      `signatory version` and the MCP handshake's serverInfo.version
#      carry a useful string instead of "dev".
#   2. `make check` bundles the pre-commit gauntlet (format + lint + test
#      + smoke) behind one command so nothing gets skipped by accident.
#
# Everything else — running a single test, experimenting with build flags,
# poking at one package — is still cleaner as a direct `go` invocation.
# If you're tempted to add a thin alias here, ask first whether the `go`
# command is really that long.

SHELL := /bin/bash

# Version stamp: the git-describe flavour matters.
#
# We deliberately DON'T pass --always. Without a tag, --always falls back
# to the short SHA, which duplicates COMMIT and makes `signatory version`
# report "abc1234 (abc1234)". Failing describe and falling back to a
# static "v0.1.0-dev" is more useful — you can tell at a glance that
# the build is pre-release without inspecting the SHA.
#
# Flavours with this setup:
#   no tag, any tree     → v0.1.0-dev
#   tag at HEAD, clean   → v0.1.0
#   tag at HEAD, dirty   → v0.1.0-dirty
#   tag + N commits      → v0.1.0-N-gabc1234
#   tag + N, dirty       → v0.1.0-N-gabc1234-dirty
VERSION := $(shell git describe --tags --dirty 2>/dev/null || echo v0.1.0-dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
# UTC RFC3339 timestamp of this install. Surfaces in `signatory version`
# so a stale binary is one command away from being spotted (dogfood
# 2026-04-21: a 5-hour-old binary emitted pre-M6 templates and the
# drift wasn't caught until the output itself was weird).
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

.PHONY: help install install-hooks check vet test lint fmt-check smoke

help:  ## Show available targets.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

install:  ## Install signatory + dogfood-metrics to $GOBIN.
	@echo "installing signatory $(VERSION) ($(COMMIT))"
	go install -ldflags "$(LDFLAGS)" ./cmd/signatory
	@echo "installing dogfood-metrics"
	go install ./cmd/dogfood-metrics

# install-hooks wires the .githooks/ directory into this clone's git
# config so the pre-commit hook (gofmt + vet + test -race) actually
# runs on commit. The hook itself is tracked in the repo; what's not
# tracked — and therefore not portable — is git's per-clone
# core.hooksPath setting. Running this target once per fresh clone
# activates it; it's idempotent to re-run.
install-hooks:  ## Wire .githooks/ into this clone's core.hooksPath so pre-commit + post-commit fire.
	@git config core.hooksPath .githooks
	@echo "git hooks: .githooks/ activated"
	@echo "  pre-commit:  gofmt + vet + test -race"
	@echo "  post-commit: make install (keeps \$$GOBIN/signatory in sync with HEAD)"

check: fmt-check vet test smoke  ## Run the full pre-commit gauntlet (matches what CI enforces).

vet:  ## Run `go vet` — quick static checks beyond what the compiler does.
	go vet ./...

test:  ## Run the full test suite with the race detector.
	go test -race -count=1 ./...

# `make lint` runs golangci-lint across the whole tree. There's a
# known baseline of pre-existing issues in non-MCP code (defer Close
# errcheck + a couple of staticcheck nits) that aren't tracked in a
# central file today — when one annoys you enough to fix, fix it in
# the same commit as your in-range work (per the "fix gofmt drift in
# files I touch" rule). The MCP packages themselves are clean; narrow
# with `golangci-lint run ./internal/mcp/...` while iterating there.
# lint is NOT in `make check` for exactly this reason — CI doesn't
# gate on it, and we don't want the pre-existing backlog to make
# `make check` useless as a pre-commit signal.
lint:  ## Run golangci-lint on the whole tree (has known baseline noise; see comment).
	golangci-lint run ./...

fmt-check:  ## Verify gofmt has no pending changes (does not rewrite files).
	@diff=$$(gofmt -l .); \
	if [ -n "$$diff" ]; then \
		echo "gofmt changes needed in:"; \
		echo "$$diff"; \
		exit 1; \
	fi

# `make smoke` runs the project's end-to-end smoke drivers. Each
# driver builds its own target binary into a tempdir and exercises it
# as a real subprocess — not unit tests, not requiring an install.
# Add new entries here when a new cmd/*-smoke driver lands.
smoke:  ## Run all end-to-end smoke drivers (MCP + dogfood-metrics).
	go run ./cmd/smoke-mcp
	go run ./cmd/dogfood-metrics-smoke
