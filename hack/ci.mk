# CI entry points, included by the root Makefile (`include hack/ci.mk`, after hack/image.mk and
# hack/deploy.mk). .github/workflows/ci.yml installs the pinned tools (hack/ci/install-tools.sh) and
# calls these targets and nothing else, so every CI job can be run on a developer machine the same
# way. Guide: docs/guides/ci.md.
#
#   make ci-static       gofmt, go mod tidy, build, vet, golangci-lint, generated files, OpenAPI,
#                        parity matrix, test report rules, tier and SDLC gate tests, shell scripts, workflow lint,
#                        secret scan (job "static")
#   make ci-unit         go test -json (-race unless CI_RACE=0) over every package except test/integration and
#                        test/deploy, then the skip/must-run report (job "unit")
#   make ci-scripts      the kind scripts' offline tests (envtest, no Docker) (job "unit")
#   make ci-integration  test/integration with PATROL_INTEGRATION=1 (job "integration")
#   make ci-deploy       test/deploy: chart and manifests against envtest (job "deploy")
#   make ci-ui           pnpm install, API types check, typecheck, lint, vitest, build (job "ui")
#   make ci-image        build the image and run the layout, smoke and rootfs checks (job "image")
#   make ci-flake-unit   ci-unit under -race with -count=$(CI_FLAKE_COUNT) -shuffle=on (scheduled.yml, "flake-unit")
#   make ci-flake-integration  ci-integration twice, -shuffle=on (scheduled.yml, "flake-integration")
#   make ci-flake-ui     vitest with --sequence.shuffle and a recorded seed (scheduled.yml, "flake-ui")
#   make ci-flake-seeds  collect the shuffle seeds, rapid seeds and new rapid fail files of the runs above
#
# The kind job runs the existing kind-* targets one per step (docs/guides/local-development.md).
# Tool versions: `hack/ci/install-tools.sh` (hack/ci/tools.lock), `make tools` (tools/go.mod), Go from
# go.mod. Reports and logs go to $(CI_OUT) (dist/ci, git-ignored).

CI_OUT ?= $(CURDIR)/dist/ci
# `go test` flags of the Go test jobs. -race costs, for a typical program, 5-10x the memory and 2-20x
# the execution time (go.dev/doc/articles/race_detector, "Runtime Overhead"); its cost on the 2 vCPU
# runner is not measured yet. The workflow turns it on for the full tier (CI_RACE=1, the default
# here) and off for ordinary pull requests. -p 1: every package runs on its own, because the envtest control planes of several
# packages starting at once on a 2 vCPU machine time out (kube-apiserver start, CRD installation);
# the tests inside a package still run in parallel. -json feeds hack/ci/testreport.
CI_RACE ?= 1
CI_GO_TEST_FLAGS ?= $(if $(filter 1,$(CI_RACE)),-race) -count=1 -p 1 -json
# -timeout is Go's limit per test binary. The WALL values bound a whole `go test` run with timeout(1)
# (GNU coreutils; not applied where it is missing, e.g. macOS without coreutils) so that the report and
# the event stream are still produced when a run is too slow: a job-level timeout cancels the job and
# skips every later step.
CI_UNIT_TIMEOUT ?= 30m
CI_INTEGRATION_TIMEOUT ?= 60m
CI_DEPLOY_TIMEOUT ?= 40m
CI_UNIT_WALL ?= 100m
CI_INTEGRATION_WALL ?= 75m
CI_DEPLOY_WALL ?= 50m
CI_TIMEOUT_BIN := $(shell command -v timeout 2>/dev/null || command -v gtimeout 2>/dev/null)
ci_wall = $(if $(and $(CI_TIMEOUT_BIN),$(1)),$(CI_TIMEOUT_BIN) --signal=TERM --kill-after=2m $(1))
# Prints one line per finished package from the `go test -json` stream while it is saved, so that a
# long job shows progress. jq is optional (runners have it).
ci_progress = { if command -v jq >/dev/null 2>&1; then jq -R -r --unbuffered 'fromjson? | select(.Test == null and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | "\(.Action) \(.Package) \(.Elapsed // 0)s"'; else cat >/dev/null; fi; }
# Packages of the unit job: everything except the two suites that have their own jobs.
CI_UNIT_PKGS = $$($(GO) list ./... | grep -v -e '/test/integration' -e '/test/deploy')
# The envtest + tool gates the Go jobs always set: a missing tool or API server binary fails the run
# instead of skipping tests (hack/ci/testreport reports the skips that remain).
CI_GO_ENV = KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" PATROL_REQUIRE_ENVTEST=1 \
	PATROL_REQUIRE_EXAMPLE_TOOLS=1 PATROL_REQUIRE_DEPLOY_TOOLS=1 \
	KUBEBUILDER_CONTROLPLANE_START_TIMEOUT=180s KUBEBUILDER_CONTROLPLANE_STOP_TIMEOUT=60s
GOLANGCI_LINT ?= golangci-lint
GITLEAKS ?= gitleaks
ACTIONLINT ?= actionlint

##@ CI

.PHONY: ci-static
ci-static: ci-gofmt ci-gomod ci-lint-go ci-generated ci-openapi ci-parity ci-rules ci-classify ci-sdlc ci-shell ci-actionlint ci-gitleaks ## Everything the "static" job checks.

.PHONY: ci-gofmt
ci-gofmt: ## Fail when a tracked Go file is not gofmt-formatted.
	@echo "==> ci-gofmt"
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
		if [ -n "$$out" ]; then echo "gofmt needed for:"; echo "$$out"; exit 1; fi

.PHONY: ci-gomod
ci-gomod: ## go mod tidy -diff and go mod verify for both modules.
	@echo "==> ci-gomod"
	$(GO) mod tidy -diff
	$(GO) mod verify
	cd tools && $(GO) mod tidy -diff

# The repository has no golangci-lint configuration: --no-config keeps one in HOME or a parent
# directory from applying, so a local run judges as CI does.
.PHONY: ci-lint-go
ci-lint-go: ## go build, go vet and golangci-lint (fails when golangci-lint is missing, unlike `make lint`).
	@echo "==> ci-lint-go"
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { echo "ci-lint-go: $(GOLANGCI_LINT) not found (hack/ci/install-tools.sh golangci-lint)" >&2; exit 1; }
	$(GO) build ./...
	$(GO) vet ./...
	$(GOLANGCI_LINT) run --no-config ./...

# verify-generated and generate rewrite deploy/crds and deploy/manifests in place: nothing else may
# touch the tree while this runs. The final `git status` catches any generated file that is
# untracked or modified after the checks (also files helm-docs writes outside the GENERATED list).
.PHONY: ci-generated
ci-generated: ## verify-generated, verify-manifests, helm-lint; fail if they changed anything in the tree.
	@echo "==> ci-generated"
	@before=$$(git status --porcelain); \
		$(MAKE) --no-print-directory verify-generated verify-manifests helm-lint || exit 1; \
		after=$$(git status --porcelain); \
		if [ "$$before" != "$$after" ]; then \
			echo "the tree changed while checking generated files:"; \
			diff <(echo "$$before") <(echo "$$after") || true; git diff --stat; exit 1; \
		fi

.PHONY: ci-openapi
ci-openapi: ## Fail when api/openapi/openapi.json is stale.
	@echo "==> ci-openapi"
	$(GO) run ./internal/apiserver/cmd/genopenapi -check

.PHONY: ci-parity
ci-parity: ## Fail when docs/parity/matrix.md is stale or a status file is invalid.
	@echo "==> ci-parity"
	$(GO) run ./hack/parity -check

.PHONY: ci-rules
ci-rules: ## Fail when hack/ci/must-run.txt or allowed-skips.txt names a package that no longer exists.
	@echo "==> ci-rules"
	$(GO) run ./hack/ci/testreport -check-rules

.PHONY: ci-classify
ci-classify: ## Regression tests of the tier selection (hack/ci/classify-changes.sh).
	@echo "==> ci-classify"
	hack/ci/test-classify.sh

.PHONY: ci-shell
ci-shell: ## shellcheck -x, bash -n and dash -n over every tracked shell script.
	@echo "==> ci-shell"
	hack/ci/lint-shell.sh

# The shellcheck actionlint runs reads no rc file from outside the tree (SHELLCHECK_OPTS=--norc).
.PHONY: ci-actionlint
ci-actionlint: ## Lint the GitHub workflows (actionlint runs shellcheck on their run: blocks).
	@echo "==> ci-actionlint"
	@command -v $(ACTIONLINT) >/dev/null 2>&1 || { echo "ci-actionlint: $(ACTIONLINT) not found (hack/ci/install-tools.sh actionlint)" >&2; exit 1; }
	SHELLCHECK_OPTS=--norc $(ACTIONLINT)

# Two scans. History: every commit reachable from the checked-out commit (the job fetches all of it;
# on a pull request that is the merge commit, so a secret that the pull request adds and later removes
# is still found), not the other branches of the repository: a fixture and its allowlist on one branch
# must not turn the scan of every other branch red. Tree: the files git does not ignore, copied to a
# temporary directory, because gitleaks does not honor .gitignore and would report ignored files such
# as .cache/kind/*.kubeconfig on a developer machine.
.PHONY: ci-gitleaks
ci-gitleaks: ## Secret scan of the history and of the files git does not ignore with .gitleaks.toml.
	@echo "==> ci-gitleaks"
	@command -v $(GITLEAKS) >/dev/null 2>&1 || { echo "ci-gitleaks: $(GITLEAKS) not found (hack/ci/install-tools.sh gitleaks)" >&2; exit 1; }
	$(GITLEAKS) git . --log-opts="--full-history --diff-filter=tuxdb HEAD" --config .gitleaks.toml --no-banner --redact
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		git ls-files -co --exclude-standard -z | tar --null -T - -cf - | tar -xf - -C "$$tmp"; \
		cd "$$tmp" && $(GITLEAKS) dir . --config $(CURDIR)/.gitleaks.toml --no-banner --redact

# Test output reaches the job log through the report below, and the runner acts on workflow commands
# anywhere in a line (`##[error]...`, `##[add-mask]...`) as well as at the start (`::error::`). While the
# report is printed the commands are therefore stopped with a token generated after the tests ran
# (a test cannot know it); the token also stops commands in the go test stderr tail.
# `go test -json` goes to a file (read by the report tool and kept as the job's artifact) and a
# progress line per package to the log. The exit status of go test, of the wall-clock bound and of the
# report are combined: any one of them fails the target.
define ci_go_suite
	@mkdir -p $(CI_OUT)/$(1)-logs && rm -f $(CI_OUT)/$(1)-summary.md
	@set -o pipefail; rc=0; \
		$(CI_GO_ENV) $(2) $(call ci_wall,$(3)) $(GO) test $(CI_GO_TEST_FLAGS) -timeout $(4) $(5) 2>$(CI_OUT)/$(1).stderr \
		| tee $(CI_OUT)/$(1).json | $(ci_progress) || rc=$$?; \
		echo "go test exit status: $$rc"; \
		if [ $$rc -eq 124 ]; then echo "::error::go test was stopped after $(3); the report below covers what finished"; fi; \
		tok=; if [ "$${GITHUB_ACTIONS:-}" = true ]; then tok=$$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); fi; \
		if [ -n "$$tok" ]; then echo "::stop-commands::$$tok"; fi; \
		$(GO) run ./hack/ci/testreport -title "$(6)" -summary $(CI_OUT)/$(1)-summary.md -json-out $(CI_OUT)/$(1)-report.json -stderr $(CI_OUT)/$(1).stderr $(CI_OUT)/$(1).json || rc=$$?; \
		if [ -n "$${GITHUB_STEP_SUMMARY:-}" ] && [ -f $(CI_OUT)/$(1)-summary.md ]; then cat $(CI_OUT)/$(1)-summary.md >>"$$GITHUB_STEP_SUMMARY"; fi; \
		if [ -s $(CI_OUT)/$(1).stderr ]; then echo "--- go test stderr (tail)"; tail -n 40 $(CI_OUT)/$(1).stderr; fi; \
		if [ -n "$$tok" ]; then echo "::$$tok::"; fi; \
		exit $$rc
endef

.PHONY: ci-unit
ci-unit: $(SETUP_ENVTEST) $(KUSTOMIZE) ## go test (-race unless CI_RACE=0) over all packages except test/integration and test/deploy, plus the skip report.
	@echo "==> ci-unit"
	$(call ci_go_suite,unit,,$(CI_UNIT_WALL),$(CI_UNIT_TIMEOUT),$(CI_UNIT_PKGS),Unit tests)

.PHONY: ci-scripts
ci-scripts: $(SETUP_ENVTEST) ## Offline tests of the kind scripts (envtest API servers, no Docker).
	@echo "==> ci-scripts"
	test/e2e/fixtures/test-envtest.sh
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" test/e2e/fixtures/test-scripts.sh
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" test/e2e/fixtures/validate.sh

# The integration suite waits more than it computes (about 11 minutes under -race on 8 CPUs), and
# needs -timeout well above Go's 10 minute default.
.PHONY: ci-integration
ci-integration: $(SETUP_ENVTEST) $(KUSTOMIZE) ## test/integration with PATROL_INTEGRATION=1 under -race.
	@echo "==> ci-integration"
	$(call ci_go_suite,integration,PATROL_INTEGRATION=1 PATROL_INTEGRATION_LOG_DIR=$(CI_OUT)/integration-logs,$(CI_INTEGRATION_WALL),$(CI_INTEGRATION_TIMEOUT),./test/integration/...,Integration suite)

.PHONY: ci-deploy
ci-deploy: $(SETUP_ENVTEST) $(KUSTOMIZE) ## test/deploy: the chart and the static manifests against envtest API servers.
	@echo "==> ci-deploy"
	$(call ci_go_suite,deploy,PATROL_DEPLOY_LOG_DIR=$(CI_OUT)/deploy-logs,$(CI_DEPLOY_WALL),$(CI_DEPLOY_TIMEOUT),./test/deploy/...,Deploy suite)

.PHONY: ci-ui
ci-ui: ## pnpm install --frozen-lockfile, gen:api:check, typecheck, lint, vitest and build of the web UI.
	@echo "==> ci-ui"
	pnpm -C $(UI_DIR) install --frozen-lockfile
	pnpm -C $(UI_DIR) gen:api:check
	pnpm -C $(UI_DIR) typecheck
	pnpm -C $(UI_DIR) lint
	# CI=true here as on GitHub: vitest then fails a focused test (.only) instead of running it alone.
	CI=true pnpm -C $(UI_DIR) test
	pnpm -C $(UI_DIR) build

# Offline pin checks first (cheap), then the image itself. umask 022: rootfs-check asserts file
# modes that depend on it.
.PHONY: ci-image
ci-image: ## Pin checks, BuildKit checks, image build, layout and smoke checks, rootfs check (needs a Docker daemon).
	@echo "==> ci-image"
	$(MAKE) --no-print-directory image-verify-pins VERIFY_PINS_FLAGS=--offline
	$(MAKE) --no-print-directory image-check
	$(MAKE) --no-print-directory image
	$(MAKE) --no-print-directory image-layout-check
	$(MAKE) --no-print-directory image-smoke
	umask 022 && $(MAKE) --no-print-directory image-rootfs-check

# The flake hunt of .github/workflows/scheduled.yml (docs/guides/ci.md, #110). The same suites as the
# unit and integration jobs, with the same reports and rules, but every test runs several times in a
# shuffled order: the timing and order bugs that otherwise reach main one at a time. go test's -timeout
# bounds a whole test binary, so it grows with the count; so does the wall-clock bound. The shuffle seed
# of each package is in the -json stream (`-test.shuffle N`); ci-flake-seeds lists it.
CI_FLAKE_COUNT ?= 3
CI_FLAKE_INTEGRATION_COUNT ?= 2
# Simply expanded, so that the seed printed and the seed used are the same.
ifeq ($(origin CI_FLAKE_UI_SEED),undefined)
CI_FLAKE_UI_SEED := $(shell date +%s)
endif

.PHONY: ci-flake-unit
ci-flake-unit: CI_GO_TEST_FLAGS = -race -count=$(CI_FLAKE_COUNT) -shuffle=on -p 1 -json
ci-flake-unit: CI_UNIT_TIMEOUT = $(shell expr 30 \* $(CI_FLAKE_COUNT))m
ci-flake-unit: CI_UNIT_WALL = $(shell expr 35 \* $(CI_FLAKE_COUNT) + 20)m
ci-flake-unit: ci-unit ## ci-unit under -race, each test CI_FLAKE_COUNT (3) times in a shuffled order.

.PHONY: ci-flake-integration
ci-flake-integration: CI_GO_TEST_FLAGS = -race -count=$(CI_FLAKE_INTEGRATION_COUNT) -shuffle=on -p 1 -json
ci-flake-integration: CI_INTEGRATION_TIMEOUT = $(shell expr 60 \* $(CI_FLAKE_INTEGRATION_COUNT))m
ci-flake-integration: CI_INTEGRATION_WALL = $(shell expr 75 \* $(CI_FLAKE_INTEGRATION_COUNT))m
ci-flake-integration: ci-integration ## ci-integration under -race, CI_FLAKE_INTEGRATION_COUNT (2) times, shuffled.

# vitest's shuffle is known to fail on pre-existing order dependencies
# (docs/verification/runs/2026-10-16-ui-flake.md); the seed is printed so that a failure reproduces with
# `pnpm -C ui exec vitest run --sequence.shuffle --sequence.seed=SEED`.
.PHONY: ci-flake-ui
ci-flake-ui: ## vitest with --sequence.shuffle (seed CI_FLAKE_UI_SEED, default the current time), CI=true.
	@echo "==> ci-flake-ui (vitest seed $(CI_FLAKE_UI_SEED))"
	@mkdir -p $(CI_OUT) && echo "vitest --sequence.seed=$(CI_FLAKE_UI_SEED)" >$(CI_OUT)/ui-shuffle-seed.txt
	pnpm -C $(UI_DIR) install --frozen-lockfile
	CI=true pnpm -C $(UI_DIR) exec vitest run --sequence.shuffle --sequence.seed=$(CI_FLAKE_UI_SEED)

.PHONY: ci-flake-seeds
ci-flake-seeds: ## Write $(CI_OUT)/seeds.md (shuffle and rapid seeds) and copy new rapid fail files to $(CI_OUT)/rapid.
	bash hack/ci/flake-seeds.sh $(CI_OUT)
