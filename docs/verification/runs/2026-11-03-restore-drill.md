# 2026-11-03 — backup and restore drill on kind (#116)

First end-to-end restore of a Patrol CD installation: `patrol admin export` of a populated
installation, loss of the installation, the same chart version installed again with the same
values, `patrol admin import`, and checks of the result. Everything ran locally against the
disposable kind clusters; nothing was pushed to a registry or a remote cluster. The backups held
the test credentials of those clusters; the drill kept them in a `0700` temporary directory it
removed, and they are not recorded.

| Item | Value |
|------|-------|
| Host | macOS 27.2 (arm64), Docker Desktop 29.9.2 |
| kind / kubectl / helm | v0.33.0 / v1.36.1 / v4.3.1 (`v4.3.1+gc0d08d0`) |
| Cluster nodes | `kindest/node:v1.36.4`, arm64; `patrol-control` (control-plane + worker) and `patrol-target` (destination) |
| Patrol image | `patrol-cd/patrol-cd:dev` built by `make image` from `cf7c8b9` (clean tree), image created 2026-11-03T03:56:30Z; its `patrol` CLI ran every export and import |
| Chart | `deploy/helm/patrol-cd` 0.1.0 with `hack/kind/values.yaml`, reinstalled from the values `helm get values` returned |
| Script | `hack/kind/restore-drill.sh` (`make kind-restore-drill`) of branch `test/backup-restore-drill`: steps 3, 5 and 6 ran earlier versions of it; steps 11 and 12 ran the version this change adds (git blob `1b98e8c`), after the agent review's changes |

Sources, as in the [2026-10-19 kind validation](2026-10-19-kind-validation.md): **script** results
come from a committed script that fails when they do not hold; **by hand** results come from
commands typed during the session, which nothing re-runs. Verbatim script logs:
[`2026-11-03-restore-drill-logs.txt`](2026-11-03-restore-drill-logs.txt) ("logs §N").

## What ran

| # | Step | Command | Source | Result |
|---|------|---------|--------|--------|
| 1 | Environment | `make image && make kind-up && make kind-fixtures && make kind-smoke && make kind-install && make kind-verify VERIFY_FLAGS=--lifecycle` | script | exit 0; `verify-install: 58 passed, 0 failed` |
| 2 | Offline checks of the drill's safety helpers | `test/e2e/fixtures/test-scripts.sh` | script, envtest (simulated) | `test-scripts: 96 passed, 0 failed`; the 17 restore-drill checks are in logs §4 |
| 3 | Namespace loss, first run | `make kind-restore-drill` | script; logs §1 | exit 2, 58 passed, 1 failed: every backup, loss, reinstall, import and restore check passed; the last cleanup step, which writes the restored admin password into `patrol-initial-admin-secret` for the other kind scripts, failed with `invalid character '\r' in string literal` (finding R-1) |
| 4 | Repair of the kind installation after step 3 | `kubectl patch secret patrol-secret --type=json` removing `admin.password` and `admin.passwordMtime`; `kubectl rollout restart deployment/patrol-server` | by hand | the bootstrap stored the hash of the password in `patrol-initial-admin-secret` again (both keys back); step 5's precondition logged in with it |
| 5 | Namespace loss | `make kind-restore-drill` | script; logs §2 | exit 0, `restore-drill: 59 passed, 0 failed`, 130 s |
| 6 | Control-plane cluster loss | `RESTORE_DRILL_FLAGS=--new-cluster make kind-restore-drill` | script; logs §3 | exit 0, `restore-drill: 59 passed, 0 failed`, 182 s; the new `patrol-control` node container was created at 04:46:12Z, `patrol-target` (04:31:54Z) was not touched |
| 7 | Scheduled backup | the CronJob of `docs/guides/backup-restore.md` (image `patrol-cd/patrol-cd:dev`), `kubectl create job --from=cronjob/patrol-backup` twice | by hand | first Job `Complete` in 8 s, one file `patrol-20261103T044948Z.yaml`, mode `-rw-------`, 22117 bytes, with the drill's kinds and counts (1 AppProject, 7 Applications, 1 Cluster, 6 ConfigMaps, 5 Repositories, 2 RepositoryCredentials, 6 Secrets); with `applications` removed from the Role, the second Job `Failed` after its two pods printed `level=WARN … cannot list resource "applications"`, and no file was left. Removed afterwards |
| 8 | Wrong namespace | the image's `patrol` with the internal kubeconfig of `patrol-control`: `admin import -n no-such-ns`, `admin import --dry-run -n no-such-ns`, `admin import -n patrol-drill-wrong` (an empty namespace) | by hand | missing namespace: a `level=WARN … namespaces "no-such-ns" not found` line per object, `Import process completed successfully`, exit 0, nothing written; the dry run reports success; the empty namespace received 7 Applications, 1 Cluster and 6 Secrets, exit 0 (namespace deleted afterwards) |
| 9 | Connection flags | `patrol admin export --kube-context no-such-context -n patrol`; the same with `--context` | by hand | `--kube-context` is ignored (exit 0, the current context was exported); `--context` fails with `context "no-such-context" does not exist` |
| 10 | Mutants of the drill's helpers | six edits of `restore-drill.sh`, one at a time, each followed by `test-scripts.sh`: `Rejected` dropped from the final Operation phases, `.metadata.generation` misspelt in `target_state`, `strip_finalizers` ignoring a failed list, ApplicationSets dropped from `patrol_kinds`, the patrol-server pods added to the controller selector, `listing` hiding an error | by hand | each run `95 passed, 1 failed`, the failing check being the one written for that case; the script was restored and compared with `cmp` afterwards |
| 11 | Namespace loss, reviewed script | `make kind-restore-drill` | script; logs §5 | exit 0, `restore-drill: 59 passed, 0 failed`, 131 s; output identical to step 5 apart from the import's timestamp |
| 12 | Control-plane cluster loss, reviewed script | `RESTORE_DRILL_FLAGS=--new-cluster make kind-restore-drill` | script; logs §6 | exit 0, `restore-drill: 59 passed, 0 failed`, 179 s; the new `patrol-control` node container was created at 05:22:58Z, `patrol-target` (04:31:54Z) was not touched; output identical to step 6 apart from the import's timestamp |

## What the drill checks

`hack/kind/restore-drill.sh` stops before it removes anything unless the installation is the one
`make kind-install kind-verify` leaves: the Helm release `patrol`, the Patrol pods ready, the 7
fixture Applications Synced/Healthy, the 7 e2e Deployments available on `patrol-target`, and an
admin login with the password in `patrol-initial-admin-secret`. Then:

1. **Backup.** It adds `resources-finalizer.cd.patrol.example` to `e2e-guestbook`, so a mistake in
   the procedure would delete that Application's workloads; exports with the installed image's
   CLI (`docker run --network kind`, the internal kubeconfig of `patrol-control`); fails on any
   `level=WARN` line; checks the backup holds the Application, the Cluster, `patrol-secret`, the
   cluster and repository credential Secrets and the settings ConfigMaps, and leaves out
   `patrol-initial-admin-secret` and `patrol-internal-token`; saves `helm get values` and the
   chart version.
2. **Loss.** Namespace: it stops the application and ApplicationSet controllers, waits for their
   pods to go, removes the finalizers of Applications, ApplicationSets and AppProjects, checks no
   finalizer and no Operation in progress is left (an Operation is final in the phases
   `OperationPhase.IsTerminal` names, `Rejected` included), deletes the namespace and waits until it
   is gone; the 8 CRDs remain. Every check that wants an empty list fails when the list cannot be
   read. Cluster (`--new-cluster`): it carries the fixture credentials over,
   deletes `patrol-control`, creates it again with `up.sh`, deploys the fixtures and loads the
   image; the new cluster has no Patrol CRDs. Both: the UIDs and generations of the e2e-*
   Namespaces, Deployments, ReplicaSets, Pods and Services on `patrol-target` are unchanged.
3. **Reinstall.** `helm install` of the same chart with the saved values; the same chart version;
   it waits for the `default` AppProject and checks the new installation generated another admin
   password.
4. **Import.** `patrol admin import -n patrol -`; one success line for namespace `patrol`, no
   `level=WARN` line, and every object of the backup exists again (`kubectl get -f`).
5. **Restored.** Admin login with the password from before the drill succeeds and the new
   installation's password is refused; the 7 Applications reach Synced/Healthy within one shared
   deadline, with no Operation created; `e2e-guestbook` has its finalizer again; a GET of the
   private Helm repository through the API returns a `Successful` connection state, while that
   repository refuses anonymous requests; the UIDs and generations on `patrol-target` are still
   those recorded before the loss (39 objects).
6. **Cleanup.** It removes the drill's finalizer and writes the restored admin password into
   `patrol-initial-admin-secret`, which the other kind scripts read.

Secrets stay out of every log and artifact: the backup, the Helm values, the passwords and the
kubeconfig live in a `0700` `mktemp` directory the script removes on exit; passwords go to `curl`
through a pipe.

## Findings

| ID | Finding | Resolution |
|----|---------|------------|
| R-1 | The drill's cleanup built a JSON patch from `base64 | tr -d '\n'`; the `base64` first on this host's PATH (Homebrew) ends lines with `\r\n`, so the patch held a `\r` and the API server refused it | Fixed in the script (`tr -d '\r\n'`); steps 5 and 6 ran with the fix |
| R-2 | `patrol admin import` reports success and exits 0 when it wrote nothing (missing namespace) or wrote a whole backup into a namespace without an installation (step 8) | #287; the guide says to pass `-n`, read the first line and fail on `level=WARN` |
| R-3 | `patrol admin` ignores the global `--kube-context` flag (step 9) | #288; the guide says to use `--context` |
| R-4 | The backup leaves out `patrol-session-revocations`, so a restore accepts sessions that were logged out within the session lifetime | #289; the guide describes the effect and the key replacement that ends it |
| R-5 | Project-role tokens are verified against the project status the backup leaves out, which patrol-server rebuilds only at startup (inferred from code, not run) | #290; the guide says to restart patrol-server |

## Not covered

The static manifests, the namespace-scoped and core variants, HA, Applications and ApplicationSets
in other namespaces, ApplicationSets at all (the fixtures have none), project-role tokens, SSO,
user-provided TLS Secrets, `--prune`, a restore into another namespace, release name or chart
version, and `linux/amd64`. The CI kind job runs the namespace scenario (`make kind-restore-drill`)
on every kind-tier run; the cluster scenario ran only here.

## Changes after the review

The agent review of the change found that the drill and the guide counted a `Rejected` Operation as
still running, and that the checks before the namespace deletion passed when kubectl failed. Both
were fixed before steps 2, 10, 11 and 12 ran.
