# Continuous integration

GitHub Actions runs the checks of this repository from `.github/workflows/ci.yml`. The workflow
installs pinned tools and calls `make ci-*` targets (`hack/ci.mk`) or, for the kind job, the
`make kind-*` targets of the [local development guide](local-development.md). Every check a job
runs is a `make` target or a script you can run locally. Only runner housekeeping is CI-only:
freeing disk (`hack/ci/free-disk.sh`), making sure sshd can run (openssh-server, `/run/sshd`), handing
the image to the kind job through the Actions cache, and the tool-location variables the setup action exports.

> **Status.** The workflow has run on GitHub on pull request #4: the full tier with the kind job
> passed on head `44b420e` (run 4 of four; the first three failed for reasons that were fixed), see
> [`docs/verification/log.md`](../verification/log.md) V-0035 and
> [`runs/2026-10-22-github-ci-runs.md`](../verification/runs/2026-10-22-github-ci-runs.md). After the
> merge, the first push to `main` (full tier, all jobs), a docs-only and a code-only pull request each ran the jobs
> of their tier and skipped the others (V-0036), and a documentation-only push to `main` ran the docs-only tier
> (V-0037). Not yet observed on GitHub: the kind-only tier, a pull request
> with a label, `CI_HEAVY_ON_PR`, manual runs, the code and full tiers on caches restored from `main`, and arm64.

## Jobs

| Job | What it runs | Make target | Runs on |
|-----|--------------|-------------|---------|
| `changes` | decides which tiers run (`hack/ci/classify-changes.sh`) | | always |
| `static` | secret scan (history and files git does not ignore), gofmt, `go mod tidy -diff`, build, vet, golangci-lint, generated files (`verify-generated`, `verify-manifests`, `helm-lint`), OpenAPI document, parity matrix, test report rules (`must-run.txt` and `allowed-skips.txt` name existing packages), tier selection tests (`hack/ci/test-classify.sh`), SDLC push gate tests (`hack/sdlc/test-sdlc.sh`), shell scripts, workflow lint; on pull requests, conventional commit subjects ([releasing.md](releasing.md)) | `ci-gitleaks`, `ci-gofmt`, `ci-gomod`, `ci-lint-go`, `ci-generated`, `ci-openapi`, `ci-parity`, `ci-rules`, `ci-classify`, `ci-sdlc`, `ci-shell`, `ci-actionlint`, `release-lint-commits` | always |
| `unit` | `go test -json` over every package except the two suites below, with the skip report, then the kind scripts' offline tests | `ci-unit`, `ci-scripts` | code changes |
| `ui` | `pnpm install --frozen-lockfile`, API types check, typecheck, lint, vitest (with `CI=true` locally too, so a focused `.only` test fails), build | `ci-ui` | code changes |
| `image` | offline pin checks, BuildKit checks, image build (linux/amd64), layout, smoke and rootfs checks | `ci-image` | code changes |
| `integration` | `test/integration` (`PATROL_INTEGRATION=1`) | `ci-integration` | full tier |
| `deploy` | `test/deploy`: the chart and the static manifests against envtest API servers, then a summary of what the Pod Security test covered (`hack/ci/podsecurity-evidence.sh`) | `ci-deploy` | full tier |
| `kind` | three kind nodes and a registry: `kind-up`, `kind-fixtures`, `kind-smoke`, `kind-install`, `kind-verify VERIFY_FLAGS=--lifecycle`, `kind-restore-drill` | the `kind-*` targets | kind tier |
| `ci` | one status for the whole workflow (`hack/ci/gate.sh`) | | always |

Playwright (`ui/e2e`) is not part of CI: the specs have never been run, and they need a running
server. `ci` is not a required check yet: no ruleset or branch protection names it (#25); it reports, it
does not block.

## Tiers and triggers

The workflow runs on pushes to `main`, on pull requests (opened, updated, reopened) and on manual
dispatch. Draft pull requests are tested like any other, and marking one ready for review does not
start a second run on the same commit. What runs depends on the change
(`hack/ci/classify-changes.sh`), because the Go suites are slow on a 2 vCPU runner and every full run
adds about 4 GB to the 10 GB Actions cache. (The tiers were designed when the repository was private
and had 2,000 Actions minutes a month; standard runners cost nothing for a public repository.)

| Tier | Jobs | When |
|------|------|------|
| docs only | `static` | every changed file is documentation: `docs/`, `*.md` at the repository root, `.gitignore` (pushes and pull requests alike; a README in a subdirectory counts as a change to that directory; `LICENSE` and `NOTICE` do not count, the image build copies them, and neither does `CHANGELOG.md`: a push that changes only it is a release commit, which the release workflow publishes only after the full tier passed) |
| code | `static`, `unit` (without `-race`), `ui`, `image` | any other pull request, push or manual run (a manual run with `full` false runs only this tier) |
| kind | adds the image hand-over and `kind` | push to `main`, manual run with `full` true (the default), label `ci:kind` or `ci:full`, or a pull request that touches `hack/kind/`, `hack/images/`, `test/e2e/`, `deploy/`, the Dockerfile, `.dockerignore`, the Makefiles, or the CI files the job runs (`hack/ci/{install-tools.sh,tools.lock,preflight.sh,free-disk.sh,sample-resources.sh,kind-diagnostics.sh,classify-changes.sh}`; the workflow, the setup action and the other helpers reach it through the full tier or not at all) |
| full | adds `-race` to `unit`, and `integration`, `deploy` (and the kind tier) | push to `main`, manual run with `full` true, label `ci:full`, or a pull request that touches `deploy/`, `test/deploy/`, `test/integration/`, `go.mod`, `go.sum`, `tools/`, `hack/ci/tools.lock`, the Makefiles, `.github/workflows/ci.yml`, `.github/actions/` (the setup action every Go job uses), `hack/ci/testreport/` or the two lists `hack/ci/must-run.txt` and `hack/ci/allowed-skips.txt` |

Documentation-only changes never reach the kind and full tiers, whatever labels the pull request
has. Set the repository variable `CI_HEAVY_ON_PR` to `true` to run the full tier on every pull
request.

Labels take effect on the next push to the pull request (a label change alone does not start a
run). Create them once with `gh label create ci:kind -d "CI: run the kind job"` and
`gh label create ci:full -d "CI: run the full tier"`. `workflow_dispatch` can only be used once
`ci.yml` is on the default branch (it is, since the merge of #4; no manual run has been made).

A new push to a pull request cancels the run in progress (minutes already used are billed). Every
other run, a push to `main` or a manual run, has a concurrency group of its own, so it is never
cancelled or replaced.

**Minutes.** The job timeouts bound the worst case, they are not an estimate: docs 5 + 60 + 5, code
5 + 60 + 120 + 30 + 60 + 5 = 280, kind tier 280 + 255 = 535, full tier 280 + 100 + 75 + 255 = 710 (it
always includes kind). Measured on GitHub, full tier with the kind job, warm caches: about 66 job
minutes a run (static 3, unit 24, web UI 4, image 6, integration 11, deploy 8, kind 10; each job is
billed rounded up, about 72, when the repository was private); the first run, with cold caches, took about 104, and the first push to `main` (no cache of its own yet) 107
(unit 37 minutes instead of 24). The other tiers, on cold pull-request caches: docs only about 11 job minutes
(`static` alone took 10.7), code only about 44 (static 10, unit without `-race` 17, web UI 4, image 12). On the
caches restored from `main`, docs only takes about 3 (a pull request 3.2, a push to `main` 3.1); the code tier's
warm-cache figure is unmeasured. A Dependabot bump of a workflow
action changes `ci.yml`, so it costs a full-tier run. These are job durations; the minutes GitHub billed are not
known. Since the repository is public, standard GitHub-hosted runners are not billed.

**Cache space.** A full run saves about 4 GB of caches per ref (the Go build caches of the static, unit,
integration and deploy jobs are 0.6 to 1 GB each) and the repository limit is 10 GB; at 17:07Z on the day of the
first push to `main` the repository held 12.4 GB, so GitHub evicts the least recently used caches and a run can
start cold again (after the probe pull requests' caches were deleted it was 9.4 GB, V-0037). Delete the caches of a closed pull request's ref (`gh cache delete --all --ref
refs/pull/<n>/merge`) if space becomes the problem.

## Tools and their pins

The runner image ships Helm 3, another Kustomize, kubectl 1.37 and ShellCheck 0.9, none of which
is what the repository was validated with, and it changes weekly. Jobs therefore install their
own, and fail in the preflight step when a tool is missing or wrong:

- **Go** comes from `go.mod` (`actions/setup-go`), with `GOTOOLCHAIN=local`.
- **Pinned downloads** (`hack/ci/tools.lock`: helm 4.3.1, kind 0.33.0, kubectl 1.36.1,
  golangci-lint 2.15.0, gitleaks 8.30.1, shellcheck 0.11.0, actionlint 1.7.12), installed by
  `hack/ci/install-tools.sh`. Every line carries the SHA-256 from the publisher's checksum file
  (for ShellCheck, which publishes none, the digest GitHub reports for the release asset) and the
  installer verifies it before extracting. To bump a tool, edit all its lines in
  `tools.lock` and run `hack/ci/install-tools.sh --check`, which downloads each asset for both
  platforms again and compares it with the publisher's file.
- **`make tools`** builds controller-gen, setup-envtest and Kustomize 5.8.1 from `tools/go.mod`;
  the three binaries are cached by the hash of `tools/go.mod` and `tools/go.sum` (`bin/envtest`
  separately, below). The Go build and module caches are keyed by the Go version and `go.sum`.
- **Node 22.23.3 and pnpm 10.33.0**: `hack/ci/setup-pnpm.sh` installs the pnpm of the Dockerfile
  (`PNPM_VERSION`, `PNPM_SHA512`) through corepack, which refuses a different hash.
- **Envtest binaries** (kube-apiserver and etcd of `ENVTEST_K8S_VERSION`) come from setup-envtest with
  the release index pinned to a controller-tools commit (`ENVTEST_INDEX` in the `Makefile`); the
  index carries the SHA-512 of each archive. `bin/envtest` is cached by the hash of the `Makefile`.
- **Actions** are pinned by full commit SHA with a version comment, and Dependabot proposes
  updates monthly (`.github/dependabot.yml`). Third-party actions are avoided: tools are plain
  downloads.

`hack/ci/preflight.sh PROFILE` prints the environment and checks it (`static`, `unit`,
`integration`, `deploy`, `ui`, `image`, `kind`); run it locally to see what a job needs.

## Skips are reported, not hidden

Several Go tests skip when a tool is missing (git, git-lfs, gpg, sshd, helm, kustomize). A green
run that skipped them would prove less than it seems, so the Go jobs run `go test -json` and
`hack/ci/testreport` reads the stream:

- a skip that is not in `hack/ci/allowed-skips.txt` fails the job (the report prints the line to
  add if the skip is legitimate);
- a test in `hack/ci/must-run.txt` (the ones that need sshd, gpg, git-lfs, helm, envtest ...)
  that did not pass fails the job;
- a test, package or build that failed or did not finish also fails the report (a backstop for a
  `TestMain` that drops the status of `m.Run`), and the headline says which kind of problem it is
  (`FAILED: 4 failed packages ...; skip policy OK`, `FAILED: the tests passed, but 2 forbidden
  skipped tests`);
- the step summary lists failures, skips with their reasons, the tail of `go test`'s stderr, and
  the slowest packages and tests.

A skip's reason is the last output block the test wrote before `t.Skip`. A test that streams more
than 8 KiB without a newline before skipping loses its message and the skip is forbidden, whatever
an earlier log line said. `make ci-rules` (in `ci-static`) fails when a package named in either rule
file no longer exists, because a rule for an absent package would silently stop applying.

Never allow-list a skip that exists because a tool or the envtest binaries are missing: fix the
environment instead.

## The kind job

It needs the image from the `image` job (`docker save` into the Actions cache, restored by
commit SHA), then runs the validated sequence one step per target, each with its own timeout.
`PATROL_WAIT_SCALE=3` stretches the scripts' waits for the 2 vCPU runner. A step's timeout covers
the waits its script can run one after the other (soft ones such as kind's `--wait` included), so that
the script, not the step timeout, reports a slow failure: `kind-up` (two waits of 9 minutes),
`kind-fixtures` (the seed Job, 15 minutes, after the image builds) and `kind-install` (helm
`--timeout` 18 minutes) have 25 minutes, `kind-smoke` 15. The job
- frees disk first (`hack/ci/free-disk.sh`) and checks the host (`hack/ci/preflight.sh kind`);
- records CPU, memory and disk every 30 s (`hack/ci/sample-resources.sh`);
- uploads `verify-install.log` and the resource samples (30 days);
- on failure collects diagnostics (`hack/ci/kind-diagnostics.sh`, 7 days) and always removes
  the clusters.

Diagnostics are allow-listed: host and Docker state, `kind export logs`, and read-only
`kubectl get/describe/logs` of both clusters. They never include Secrets, `helm get` output or
kubeconfigs. The bundle is collected outside the upload path. The script stops collecting after
`DIAG_BUDGET_SECONDS` (600) and still scrubs; the 15 minutes of the step are the outer `timeout`.
`INDEX.txt` is written first with `scrub: PENDING`. The scrub replaces every collected file that
contains a known secret value (the admin password, the fixture credentials, the cluster tokens,
kubeconfig keys; plain, base64 or as a PEM line), replaces `*.gz` files (it cannot read inside
them) and removes symlinks. The bundle is moved to the upload path only when `INDEX.txt` says
`scrub: ok`. Anything else uploads only `INDEX.txt`: a cluster whose Secrets could not be read
makes the status `INCOMPLETE`; a cluster whose API does not even list its Secret names is skipped
before its items are collected (so a wedged API costs about two minutes, not ten); a signal or the
outer timeout removes everything but `INDEX.txt` and marks it `INCOMPLETE (interrupted ...)`; a
`SIGKILL` leaves `PENDING`, which the workflow also rejects. With no `patrol-*` cluster and no
kubeconfig there is nothing to scrub and the host state is uploaded (`ok (nothing to scrub ...)`).
`--scrub-only DIR` applies the scrub to an existing directory and exits 3 unless it is `ok`.

## Reproduce a job locally

```bash
hack/ci/preflight.sh static unit  # FAIL lines are what differs from CI (use the job's profile)
make tools                        # controller-gen, setup-envtest, kustomize
# Linux only: install the pinned tools and put them first on PATH. On macOS use your own
# (Helm 4, golangci-lint 2.15.0, shellcheck 0.11.0 ...); the preflight tells you what is off.
hack/ci/install-tools.sh --dest /tmp/ci-bin all && export PATH=/tmp/ci-bin/bin:$PATH
make ci-static                    # the secret scan reads the history of HEAD, then a copy of the files git does not ignore
make ci-unit CI_RACE=0            # or CI_RACE=1; reports under dist/ci
make ci-scripts ci-integration ci-deploy ci-ui ci-image
make image kind-up kind-fixtures kind-smoke kind-install kind-verify VERIFY_FLAGS=--lifecycle kind-restore-drill
```

`make ci-unit` and the suites write `dist/ci/*.json` (the `go test -json` stream), a Markdown
summary and a JSON report; `PATROL_WAIT_SCALE=3` gives the kind scripts the CI timings.

## When a job fails

- **`helm`/`kustomize` version errors**: the runner's own tools came first on `PATH`. The
  preflight step names the offender; the setup action exports `HELM`, `PATROL_TEST_HELM`,
  `KUSTOMIZE` and `PATROL_TEST_KUSTOMIZE`.
- **envtest `timeout waiting for process kube-apiserver` or `CRDs to appear`**: the runner was
  too busy for the control plane. The unit job runs one package at a time (`-p 1`) and sets
  `KUBEBUILDER_CONTROLPLANE_START_TIMEOUT=180s` (`hack/ci.mk`); `test/integration` (60 s, doubled when
  `CI` is set) and `test/deploy` (2 min) set their own start timeouts; every harness waits 90 s for
  CRDs (`internal/envtestx`, envtest's own default is 10 s). `envtestx.Start` also stops the
  control plane when a start fails: envtest itself leaves etcd and kube-apiserver running, and each
  leaked pair slowed every package after it (seen in a 2 CPU container). A package that still fails
  needs a longer wait in its own harness.
- **a forbidden skip**: see above.
- **the kind job fails at "all Running, ready, 0 restarts"**: a Patrol pod restarted or is not
  ready. Look for the cause in the diagnostics before blaming the runner: `describe` output (Last
  State reason and exit code, e.g. OOMKilled vs Error), the `--previous` logs (`leader election
  lost` after API timeouts is what KV-13 in
  [phase4-findings](../verification/phase4-findings.md) saw once, with the cause not
  established; CPU pressure on the 2 vCPU runner is a plausible but unproven candidate), and
  `resources.tsv` around that time.
- **HTTP 412 `No ED25519 host key is known` for e2e-guestbook-ssh**: the repo-server's mounted
  known-hosts file lags about a minute behind the ConfigMap (KV-12, fixed: `install.sh` waits for
  it). If it still appears, the wait timed out; see the diagnostics.

## Not covered yet

Playwright specs, a multi-architecture or arm64 image build in CI (the release workflow builds
both, [releasing.md](releasing.md)), vulnerability scans (#106), required checks (#25), and a scheduled
run that would catch drift of external dependencies (Docker Hub images, tool downloads).
