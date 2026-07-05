# ==============================================================================
# shuck — developer Makefile
#
# Run `make help` to see all targets.
# Tool versions are pinned below and mirrored in the GitHub Actions workflows
# and .goreleaser.yaml. Bump them together.
# ==============================================================================

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# ---- Project ----------------------------------------------------------------
BINARY   := shuck
MAIN_PKG := .
BIN_DIR  := bin
DIST_DIR := dist
COVERAGE := coverage.out

# ---- Version / build metadata ----------------------------------------------
# shuck derives its version from `git describe` (no VERSION file) and injects it
# into internal/cli.version. COMMIT/DATE are exposed for the container build args
# but are not wired into ldflags (the CLI only carries a version string).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/justanotherspy/shuck/internal/cli.version=$(VERSION)

# ---- Pinned tool versions ---------------------------------------------------
GOLANGCI_LINT_VERSION := v2.12.2
GORELEASER_VERSION    := v2.16.0
GOTESTSUM_VERSION     := v1.13.0
GOVULNCHECK_VERSION   := latest
GOPLS_VERSION         := latest
ACTIONLINT_VERSION    := latest
BENCHSTAT_VERSION     := latest

GO    ?= go
GOBIN := $(shell $(GO) env GOPATH)/bin
export GOTOOLCHAIN ?= auto
export PATH := $(GOBIN):$(PATH)

# ---- Test / benchmark / fuzz / profile knobs --------------------------------
# Override on the command line, e.g. `make bench BENCH=BenchmarkParse`.
# (Keep these free of trailing inline comments: Make bakes the trailing
# whitespace into the value, which corrupts paths and flags.)
FUZZ        ?=
FUZZPKG     ?= ./...
FUZZTIME    ?= 30s
FUZZTIME_CI ?= 1m
BENCH       ?= .
BENCHPKG    ?= ./...
BENCHTIME   ?= 1s
BENCHCOUNT  ?= 6
BENCHFILE   ?= bench-new.txt
PROFPKG     ?= ./internal/logs
PROFILE_DIR ?= profiles

# Coverage filter & threshold. main.go and the cmd/ entrypoints are thin
# mains with no unit tests, so they are dropped from coverage.out — the
# numbers (and the gate below) reflect the internal/ packages only.
# COVER_EXCLUDE is an extended regexp matched against coverage.out paths;
# COVER_THRESHOLD is the minimum total coverage percentage `make cover-check`
# accepts (CI fails below it).
COVER_EXCLUDE   ?= ^github\.com/justanotherspy/shuck/(main\.go:|cmd/)
COVER_THRESHOLD ?= 80

# ==============================================================================
.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ---- Dependencies & tooling -------------------------------------------------
.PHONY: deps
deps: ## Download and verify Go module dependencies
	$(GO) mod download
	$(GO) mod verify

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

.PHONY: tools
tools: golangci-lint goreleaser gotestsum govulncheck-install lsp benchstat ## Install all pinned dev tools

.PHONY: check-tools
check-tools: ## Verify required tools are installed
	@missing=0; \
	for t in $(GO) git golangci-lint goreleaser govulncheck gopls; do \
		if command -v $$t >/dev/null 2>&1; then echo "  [ok] $$t"; \
		else echo "  [--] $$t  (run: make tools)"; missing=1; fi; \
	done; \
	exit $$missing

.PHONY: hooks
hooks: ## Install git pre-commit/pre-push hooks (requires pre-commit)
	@command -v pre-commit >/dev/null 2>&1 || { \
		echo ">> pre-commit not found; install from https://pre-commit.com"; exit 1; }
	pre-commit install --install-hooks
	pre-commit install --hook-type pre-push

.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint (v2) if missing
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo ">> installing golangci-lint $(GOLANGCI_LINT_VERSION)"; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); }

# lint-install is the historical name (referenced by CONTRIBUTING/CLAUDE); keep
# it as an alias of the golangci-lint installer target.
.PHONY: lint-install
lint-install: golangci-lint ## Alias of `golangci-lint` (install the linter)

.PHONY: goreleaser
goreleaser: ## Install GoReleaser if missing
	@command -v goreleaser >/dev/null 2>&1 || { \
		echo ">> installing goreleaser $(GORELEASER_VERSION)"; \
		$(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION); }

.PHONY: gotestsum
gotestsum: ## Install gotestsum if missing
	@command -v gotestsum >/dev/null 2>&1 || { \
		echo ">> installing gotestsum $(GOTESTSUM_VERSION)"; \
		$(GO) install gotest.tools/gotestsum@$(GOTESTSUM_VERSION); }

.PHONY: govulncheck-install
govulncheck-install: ## (internal) install govulncheck if missing
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo ">> installing govulncheck $(GOVULNCHECK_VERSION)"; \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); }

.PHONY: lsp
lsp: ## Install gopls (Go language server for editors & Claude Code)
	@command -v gopls >/dev/null 2>&1 || { \
		echo ">> installing gopls $(GOPLS_VERSION)"; \
		$(GO) install golang.org/x/tools/gopls@$(GOPLS_VERSION); }

.PHONY: benchstat
benchstat: ## Install benchstat (benchmark comparison) if missing
	@command -v benchstat >/dev/null 2>&1 || { \
		echo ">> installing benchstat $(BENCHSTAT_VERSION)"; \
		$(GO) install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION); }

# ---- Quality ----------------------------------------------------------------
.PHONY: fmt
fmt: golangci-lint ## Format code (gofmt + goimports via golangci-lint)
	golangci-lint fmt

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint
	golangci-lint run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix
	golangci-lint run --fix

.PHONY: modernize
modernize: ## Apply go1.26 modernizers in place (go fix)
	$(GO) fix ./...

.PHONY: modernize-check
modernize-check: ## Report code that go fix would modernize (fails if any; CI)
	@out="$$($(GO) fix -diff ./... 2>/dev/null)"; \
	if [ -n "$$out" ]; then \
		echo "go fix would modernize the following; run 'make modernize':"; \
		echo "$$out"; \
		exit 1; \
	fi

# fix / fix-check are the historical names (still used by CI and muscle memory);
# keep them working as aliases of modernize / modernize-check.
.PHONY: fix
fix: modernize ## Alias of `modernize`

.PHONY: fix-check
fix-check: modernize-check ## Alias of `modernize-check`

.PHONY: actionlint
actionlint: ## Lint GitHub Actions workflows (runs shellcheck on run: blocks if present)
	@command -v actionlint >/dev/null 2>&1 || { \
		echo ">> installing actionlint $(ACTIONLINT_VERSION)"; \
		$(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION); }
	actionlint

.PHONY: shellcheck
shellcheck: ## Run shellcheck over the repo's shell scripts
	@command -v shellcheck >/dev/null 2>&1 || { \
		echo ">> shellcheck not found; install from https://www.shellcheck.net"; exit 1; }
	find . -type f -name '*.sh' -print0 | xargs -0 -r shellcheck

.PHONY: plugin-validate
plugin-validate: ## Validate the plugin + marketplace manifests (requires the claude CLI)
	@command -v claude >/dev/null 2>&1 || { \
		echo ">> claude CLI not found; install with: npm install -g @anthropic-ai/claude-code"; exit 1; }
	claude plugin validate --strict ./plugins/shuck
	claude plugin validate --strict ./plugins/shuck-channel
	claude plugin validate --strict .claude-plugin/marketplace.json

.PHONY: shim-check
shim-check: ## Typecheck + test the channel shim plugin (requires bun)
	@command -v bun >/dev/null 2>&1 || { \
		echo ">> bun not found; install from https://bun.sh"; exit 1; }
	cd plugins/shuck-channel && bun install --frozen-lockfile && bun run typecheck && bun test

.PHONY: terraform-check
terraform-check: ## fmt-check + validate the deploy/terraform module (requires terraform)
	@command -v terraform >/dev/null 2>&1 || { \
		echo ">> terraform not found; install from https://developer.hashicorp.com/terraform/install"; exit 1; }
	terraform -chdir=deploy/terraform fmt -check -recursive
	terraform -chdir=deploy/terraform init -backend=false -input=false >/dev/null
	terraform -chdir=deploy/terraform validate

# ---- Tests ------------------------------------------------------------------
# Drop excluded files from the profile in place, preserving the leading
# `mode:` line. No-op when COVER_EXCLUDE is empty.
define filter_coverage
	@if [ -n "$(COVER_EXCLUDE)" ] && [ -f "$(COVERAGE)" ]; then \
		grep -v -E "$(COVER_EXCLUDE)" "$(COVERAGE)" > "$(COVERAGE).tmp" && mv "$(COVERAGE).tmp" "$(COVERAGE)"; \
	fi
endef

.PHONY: test
test: ## Run tests with the race detector and coverage
	$(GO) test -race -covermode=atomic -coverprofile=$(COVERAGE) ./...
	$(filter_coverage)

.PHONY: test-pretty
test-pretty: gotestsum ## Run tests with pretty output (gotestsum)
	gotestsum -- -race -covermode=atomic -coverprofile=$(COVERAGE) ./...
	$(filter_coverage)

.PHONY: cover
cover: test ## Print per-function coverage summary
	$(GO) tool cover -func=$(COVERAGE)

.PHONY: cover-html
cover-html: test ## Open the HTML coverage report
	$(GO) tool cover -html=$(COVERAGE)

.PHONY: cover-total
cover-total: ## Print the total coverage percentage (needs an existing coverage.out)
	@$(GO) tool cover -func=$(COVERAGE) | awk '/^total:/ {print $$3}'

.PHONY: cover-check
cover-check: ## Fail if total coverage is below COVER_THRESHOLD% (needs an existing coverage.out)
	@total=$$($(GO) tool cover -func=$(COVERAGE) | awk '/^total:/ {sub(/%/, "", $$3); print $$3}'); \
	echo ">> total coverage: $$total% (threshold: $(COVER_THRESHOLD)%)"; \
	awk -v total="$$total" -v min="$(COVER_THRESHOLD)" 'BEGIN { exit !(total+0 >= min+0) }' || { \
		echo ">> FAIL: coverage $$total% is below the required $(COVER_THRESHOLD)%"; \
		exit 1; \
	}

.PHONY: cover-report
cover-report: ## Emit a Markdown coverage report to stdout (used by CI to comment on PRs)
	@total=$$($(GO) tool cover -func=$(COVERAGE) | awk '/^total:/ {print $$3}'); \
	printf '### 🧪 Code coverage: %s (threshold: %s%%)\n\n' "$$total" "$(COVER_THRESHOLD)"; \
	printf '<details><summary>Per-function coverage</summary>\n\n'; \
	printf '```\n'; \
	$(GO) tool cover -func=$(COVERAGE); \
	printf '```\n\n</details>\n'

# ---- Fuzzing ----------------------------------------------------------------
# Seed corpora run as ordinary unit tests under `make test`. These targets do
# active, mutation-based fuzzing; crashers are written to testdata/fuzz/<Fuzz>/
# next to the test — commit them as regression seeds.
# `go test -fuzz` only accepts a single package, so when FUZZPKG is the ./...
# default the target's package is discovered by listing every package's Fuzz*
# functions and matching FUZZ against them.
.PHONY: fuzz
fuzz: ## Actively fuzz ONE target: make fuzz FUZZ=FuzzName [FUZZPKG=./pkg FUZZTIME=1m]
	@if [ -z "$(FUZZ)" ]; then echo ">> set FUZZ=FuzzName (a single fuzz target)"; exit 2; fi
	@set -euo pipefail; \
	pkg='$(FUZZPKG)'; \
	if [ "$$pkg" = "./..." ]; then \
		pkg=""; \
		for p in $$($(GO) list ./...); do \
			if $(GO) test -list '^$(FUZZ)$$' $$p 2>/dev/null | grep -E '^$(FUZZ)$$' >/dev/null; then pkg=$$p; break; fi; \
		done; \
		if [ -z "$$pkg" ]; then echo ">> no package defines $(FUZZ)"; exit 2; fi; \
	fi; \
	echo ">> fuzzing $(FUZZ) ($$pkg) for $(FUZZTIME)"; \
	$(GO) test -run '^$$' -fuzz '^$(FUZZ)$$' -fuzztime $(FUZZTIME) $$pkg

.PHONY: fuzz-all
fuzz-all: ## Briefly fuzz every target in the module (used by the nightly workflow)
	@set -euo pipefail; \
	for pkg in $$($(GO) list ./...); do \
		for fn in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep -E '^Fuzz' || true); do \
			echo ">> fuzzing $$fn ($$pkg) for $(FUZZTIME_CI)"; \
			$(GO) test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME_CI) $$pkg; \
		done; \
	done

# ---- Benchmarks & profiling -------------------------------------------------
.PHONY: bench
bench: ## Run benchmarks: make bench [BENCH=BenchmarkX BENCHPKG=./... BENCHTIME=1s]
	$(GO) test -run '^$$' -bench '$(BENCH)' -benchmem -benchtime $(BENCHTIME) $(BENCHPKG)

.PHONY: bench-save
bench-save: ## Run benchmarks BENCHCOUNT times into BENCHFILE (for benchstat)
	$(GO) test -run '^$$' -bench '$(BENCH)' -benchmem -count=$(BENCHCOUNT) $(BENCHPKG) | tee $(BENCHFILE)

.PHONY: benchstat-cmp
benchstat-cmp: benchstat ## Compare bench-old.txt vs bench-new.txt with benchstat
	benchstat bench-old.txt bench-new.txt

.PHONY: profile
profile: ## CPU+mem profile a benchmark into PROFILE_DIR: make profile BENCH=BenchmarkX
	@mkdir -p $(PROFILE_DIR)
	$(GO) test -run '^$$' -bench '$(BENCH)' -benchmem \
		-cpuprofile $(PROFILE_DIR)/cpu.prof \
		-memprofile $(PROFILE_DIR)/mem.prof \
		-o $(PROFILE_DIR)/bench.test $(PROFPKG)
	@echo ">> open: make pprof-cpu   (or)   go tool pprof -http=: $(PROFILE_DIR)/cpu.prof"

.PHONY: pprof-cpu
pprof-cpu: ## Open the CPU profile in the pprof web UI (defaults to the flame graph)
	$(GO) tool pprof -http=: $(PROFILE_DIR)/cpu.prof

.PHONY: pprof-mem
pprof-mem: ## Open the memory profile in the pprof web UI
	$(GO) tool pprof -http=: $(PROFILE_DIR)/mem.prof

# ---- Build / run ------------------------------------------------------------
.PHONY: build
build: ## Build the binaries into ./bin (shuck + the self-hosted ingest, gateway, worker & portal)
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(MAIN_PKG)
	$(GO) build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/shuck-ingest ./cmd/shuck-ingest
	$(GO) build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/shuck-gateway ./cmd/shuck-gateway
	$(GO) build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/shuck-worker ./cmd/shuck-worker
	$(GO) build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/shuck-portal ./cmd/shuck-portal

.PHONY: install
install: ## go install the binary
	$(GO) install -trimpath -ldflags '$(LDFLAGS)' $(MAIN_PKG)

.PHONY: run
run: ## Run the CLI (pass args via ARGS="...")
	$(GO) run -ldflags '$(LDFLAGS)' $(MAIN_PKG) $(ARGS)

# ---- Container --------------------------------------------------------------
IMAGE     ?= shuck
IMAGE_TAG ?= dev

.PHONY: docker-build
docker-build: ## Build a local container image (IMAGE=shuck IMAGE_TAG=dev)
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE):$(IMAGE_TAG) .

.PHONY: docker-run
docker-run: ## Run the local container image (pass args via ARGS="...")
	docker run --rm $(IMAGE):$(IMAGE_TAG) $(ARGS)

# ---- Security ---------------------------------------------------------------
.PHONY: vuln
vuln: govulncheck-install ## Scan dependencies for known vulnerabilities
	govulncheck ./...

.PHONY: secrets
secrets: ## Scan the working tree for committed secrets (requires trufflehog)
	@command -v trufflehog >/dev/null 2>&1 || { \
		echo ">> trufflehog not found; install from https://github.com/trufflesecurity/trufflehog"; exit 1; }
	trufflehog --no-update filesystem . --results=verified,unknown --fail

.PHONY: zizmor
zizmor: ## Audit GitHub Actions workflows for security issues (requires zizmor; `pipx install zizmor`)
	@command -v zizmor >/dev/null 2>&1 || { \
		echo ">> zizmor not found; install with: pipx install zizmor"; exit 1; }
	zizmor --min-severity=high .github/workflows

.PHONY: security
security: vuln ## Run all local security checks
	@echo ">> security checks complete"

# ---- Release ----------------------------------------------------------------
.PHONY: release-check
release-check: goreleaser ## Validate the GoReleaser configuration
	goreleaser check

.PHONY: snapshot
snapshot: goreleaser ## Build a local snapshot release (no publish)
	goreleaser release --snapshot --clean --skip=sign,sbom

# ---- Aggregates -------------------------------------------------------------
.PHONY: ci
ci: deps lint modernize-check test cover-check build ## Run the pipeline that CI runs

.PHONY: all
all: tidy fmt modernize lint test build ## Tidy, format, modernize, lint, test, and build

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(DIST_DIR) $(COVERAGE) $(PROFILE_DIR) bench-*.txt *.prof *.test $(BINARY)
