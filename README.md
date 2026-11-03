# Patrol CD

Patrol CD is a Kubernetes-native continuous delivery platform that continuously reconciles
declaratively specified application state from version-controlled sources into Kubernetes
clusters. It is an independent implementation (its own control plane, reconciliation engine,
API, CLI and web UI) with functional parity as the goal against a frozen
[Argo CD v3.5.5](https://github.com/argoproj/argo-cd/tree/v3.5.5) baseline
(commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`). It does not embed, wrap or proxy Argo CD.

> **Status: work in progress, not released.** Phases 0–3 (research, architecture, libraries,
> controllers, API, CLI, web UI, server wiring) are implemented and tested locally. Phase 4
> packaging (Dockerfile, Helm chart, static install manifests, first-run bootstrap) is written,
> validated against envtest API servers, and installed and checked on two local kind clusters
> (`make image kind-up kind-fixtures kind-install kind-verify`: Applications of seven source
> types sync through the API to the second cluster). No image has been published and nothing has run on a
> cloud cluster, with more than one replica, on another CNI than kindnet, or on `linux/amd64`.
> The full end-to-end scenario suite, security and performance verification, CI and most
> documentation are **not done yet**. Nothing here has been verified against a live GitHub. The
> API group `cd.patrol.example` is a development placeholder (see ADR-0002).

## What exists

| Area | Where | State |
|------|-------|-------|
| Parity inventory of Argo CD v3.5.5 (1,457 features) | [`docs/parity`](docs/parity/baseline.md) | complete; per-feature implementation status in [`matrix.md`](docs/parity/matrix.md) |
| Architecture, ADRs, threat model | [`docs/architecture`](docs/architecture/overview.md) | written; living documents |
| CRDs (`Application`, `Operation`, `AppProject`, `ApplicationSet`, `Repository`, `RepositoryCredential`, `Cluster`, `SCMProvider`) | [`api/v1alpha1`](api/v1alpha1), [`deploy/crds`](deploy/crds) | implemented, admission-tested with envtest |
| Control plane (controller, repo-server, ApplicationSet controller, notifications, hydrator, commit-server, CMP server) | [`internal/`](internal) | implemented; unit + envtest tested |
| Public API (REST/SSE, OpenAPI 3.1) | [`internal/apiserver`](internal/apiserver), [`api/openapi/openapi.json`](api/openapi/openapi.json) | 125 documented operations (incl. the two webhook receivers) |
| SCM provider contract + GitHub (App auth, PRs, webhooks, statuses/checks) + 7 other providers | [`pkg/scm`](pkg/scm) | contract-tested against fakes only |
| CLI (`patrol`, including `--core` mode and `admin` tools) | [`internal/cli`](internal/cli), [`cmd/patrol`](cmd/patrol) | implemented |
| Multi-call control-plane binary (`patrolcd`) | [`cmd/patrolcd`](cmd/patrolcd), [`internal/server`](internal/server) | implemented |
| Web UI (React + TypeScript) | [`ui/`](ui) | implemented; tested against mocked APIs, Playwright specs not yet run |
| Integration harness (real API servers via envtest, real git/helm/kustomize) | [`test/integration`](test/integration) | lifecycle + recovery scenarios passing |
| Examples | [`examples/`](examples) | validated against the CRDs and real renderers |
| Container image (Alpine, non-root, pinned helm/kustomize/git-lfs) | [`Dockerfile`](Dockerfile), [`hack/images`](hack/images), [`docs/guides/images.md`](docs/guides/images.md) | built for linux/arm64 and checked (layout, runtime smoke test under the chart's restricted settings); ran the five components of the chart's default profile on a local kind cluster; amd64, SBOM and scans are not done |
| Helm chart | [`deploy/helm/patrol-cd`](deploy/helm/patrol-cd), [`docs/guides/installation.md`](docs/guides/installation.md) | value profiles lint and render deterministically; install/upgrade/rollback/uninstall and RBAC sufficiency run against envtest API servers; default profile installed, upgraded, rolled back, uninstalled and re-installed on kind with a kubelet (`make kind-verify VERIFY_FLAGS=--lifecycle`, 58 checks); backed up and restored after the loss of its namespace or cluster (`make kind-restore-drill`, [`docs/guides/backup-restore.md`](docs/guides/backup-restore.md)) |
| Static install manifests (`install`, `namespace-install`, `core-install`, hydrator variants, `crds`) | [`deploy/manifests`](deploy/manifests), [`docs/guides/manifests.md`](docs/guides/manifests.md) | generated from the chart, verified equal to it and server-side applied to envtest; `cluster-install` also applied once by hand to a kind cluster (61 objects, five pods Running, login works; no committed check); the other variants were not |
| First-run bootstrap (`patrolcd bootstrap`: internal token, session key, initial admin password, default SSH known hosts) | [`internal/server`](internal/server), [`bootstrap.md`](docs/architecture/components/bootstrap.md) | unit + envtest tested (including a new initial admin password after uninstall and re-install); run as an init container by the chart and manifests, and on kind by a kubelet (default profile, checked by `hack/kind/verify-install.sh`) |

Verification evidence (commands, environments, results, known gaps) is recorded in
[`docs/verification/log.md`](docs/verification/log.md). Anything that used a fake external
system is labelled simulated there.

## Build and test

Requirements: Go (see `go.mod`), Node + pnpm, git, Helm 4, and the pinned tools installed by
`make tools` (controller-gen, setup-envtest, kustomize).

```bash
make tools            # install pinned tools into ./bin
make generate         # deepcopy, CRDs, the chart's generated files, static install manifests
make verify-generated # fails if generated files are stale
make build            # Go binaries into ./bin
make test             # Go tests incl. envtest-backed ones, test/integration and test/deploy (slow)
pnpm -C ui install && pnpm -C ui test && pnpm -C ui build

# integrated lifecycle/recovery suite (real kube-apiservers via envtest)
PATROL_INTEGRATION=1 go test -race -timeout 60m ./test/integration/...

# packaging checks that need no container runtime
make helm-lint helm-template verify-manifests image-rootfs-check
# deployment validation of the chart and manifests against envtest API servers
KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest)" PATROL_REQUIRE_ENVTEST=1 \
  go test -race -timeout 40m ./test/deploy/

# the CI jobs, runnable locally (docs/guides/ci.md)
make ci-static ci-unit ci-ui ci-image   # see `make help` for the others
```

See [`CLAUDE.md`](CLAUDE.md) for conventions. Tasks, known defects and limitations are tracked as
[GitHub issues](https://github.com/juliosaraiva/patrol-cd/issues); [`TASKS.md`](TASKS.md) lists the scope rules in force.

## Known limitations (high level)

- The container image has been built for linux/arm64 only and not published. Patrol CD has run
  on two local kind clusters (one CNI, kindnet; arm64; one replica per component; the chart's
  default profile; no SSO, ingress, ApplicationSets, hydrator, CMP plugins or notifications
  delivery exercised, no browser run), not on a cloud cluster. A GitHub Actions workflow
  (`docs/guides/ci.md`) has passed its full tier, including the kind lifecycle job, on one
  pull request (`docs/verification/log.md` V-0035); other tiers and pushes to `main` have not run.
- Most unit and integration tests still run against envtest (no kubelet), where pod health is
  simulated; the kind checks (`hack/kind/verify-install.sh`) cover the install, security
  contexts, part of the NetworkPolicy enforcement, metrics endpoints, sync and drift on real
  nodes, and with `--lifecycle` helm rollback, upgrade, uninstall and re-install. Pod logs and
  the web terminal have not been exercised against a kubelet; port-forward was used only by
  hand, by CLI core mode, to reach the control cluster's repo-server and controller (core mode's
  `app manifests`, `app diff` and `app history` worked against the live cluster; see
  `docs/verification/runs/2026-10-19-kind-validation.md`).
- GitHub integration is verified against contract tests and a local fake of the GitHub API,
  not against live GitHub.
- Argo CD parity is tracked feature by feature; many rows are still `in-progress` or
  `not-started` in the matrix.

## Security

Report vulnerabilities privately, as [`SECURITY.md`](SECURITY.md) describes; never in a public issue.

## License

Apache License 2.0 (see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE)). Some Lua health-check and
resource-action data files are derived from Argo CD v3.5.5 and keep their Apache-2.0 origin.
