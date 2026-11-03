# Releasing

Releases are cut from `main` without hand-editing versions or changelogs. The commit subjects
(conventional commits, as the repository already uses) decide the next version and become
`CHANGELOG.md`; a release pull request carries the change; merging it is the decision to release,
and the release workflow publishes once CI has passed on the merged commit and the owner approved
its publishing jobs.

> **Status.** The tool (`hack/release`) is unit-tested and was run against this repository's
> history and in a scratch clone (prepare, generated-file checks, artifacts); see
> [`docs/verification/log.md`](../verification/log.md) V-0038 and V-0039. On GitHub, the Release PR
> workflow pushes `release/next` but cannot open the pull request until the repository settings
> below are made (#95), and the Release workflow has run its `plan` job only (no release was
> pending). The first release pull request, the approvals, the image push, the chart push and the
> GitHub release are unobserved.

## The flow

```
push to main ──► Release PR workflow ──► release/next branch + pull request "chore(release): vX.Y.Z"
                 (release-pr.yml)          CHANGELOG.md section, Chart.yaml version/appVersion,
                                           chart README and static manifests regenerated
merge the PR ──► CI on main (full tier) ──► Release workflow (release.yml), from the release commit:
                                              artifacts │ image* ──► publish*: chart push, tag vX.Y.Z,
                                                                               GitHub release
                                              * waits for the owner's approval (environment release)
```

1. **Every push to `main`** runs `release-pr.yml`. `hack/release` reads the commits since the last
   `vX.Y.Z` tag. If one of them is releasable it rebuilds the branch `release/next` from `main` with
   one commit, `chore(release): vX.Y.Z`, and opens or updates the pull request. Later pushes to
   `main` rebuild it, so it always describes `main` as it is.
2. **Review and merge the pull request.** Reword `CHANGELOG.md` lines in it if needed, right
   before merging (the next push to `main` would rebuild the branch and drop the edit). Merge it
   with a merge commit or a squash, and keep its title `chore(release): vX.Y.Z`: the release
   workflow accepts only that pull request's merge.
3. **CI runs on the merge** (pushes to `main` run the full tier, and so does a push that changes
   only `CHANGELOG.md`). After every successful CI run of a push to `main`, and on a manual **Run
   workflow** of `release.yml`, `hack/release select` checks whether a release is *pending*:
   `Chart.yaml` names X.Y.Z, `CHANGELOG.md` has its section and the tag `vX.Y.Z` does not exist.
   Both events then choose the same commit, the **release commit**. It is the newest commit since
   the last tag on `main`'s first-parent history that:
   - changed `CHANGELOG.md`, with `Chart.yaml` at X.Y.Z and the same X.Y.Z section as `main`'s tip;
   - is the merge of this repository's `release/next` pull request titled `chore(release): vX.Y.Z`
     (GitHub's pull requests of the commit: merged into `main`, `merge_commit_sha` equal to the
     commit, so a merge commit, a squash or the tip of a rebase).

   Any other commit that changed `CHANGELOG.md` is passed over: a pull request from a fork or from
   another branch, or a direct push. One that leaves the X.Y.Z section alone does not block the
   release; one that changes the section leaves no release commit until the version is prepared
   again (below). The commit the event happened to point at is never used: a later commit would be
   tagged with changes no changelog section lists (and the next release starts from the tag), and
   a documentation-only push runs the static checks only. The release commit is published once the
   newest CI run of its push to `main` passed every job (`Choose tiers`, `Static checks`,
   `Unit tests`, `Web UI`, `Image`, `Integration suite`, `Deploy suite`, `Kind`, `CI`; a skipped
   job does not count). Until then a CI-triggered run waits (a notice); a manual run fails with the
   reason.
4. **Approve the release.** The `plan` job's notice names the tag, the release commit and its pull
   request (`releasing vX.Y.Z from <sha> (pull request #N)`). Check them, then approve the `image`
   job in the run's *Review deployments* dialog; after the image is built, approve the `publish`
   job the same way. A release takes these two approvals; a job waits up to 30 days for its
   approval, and the workflow artifact the `publish` job needs is kept 35 days. Rejecting `image`
   publishes nothing; rejecting `publish` leaves the image `X.Y.Z` in the registry with no chart,
   git tag or GitHub release (a later run pushes the same image tag again). It publishes:

   | Job | What | Where |
   |-----|------|-------|
   | `artifacts` | `make release-artifacts`: `patrol` CLI for linux, darwin, windows × amd64, arm64; `install.yaml`, `namespace-install.yaml`, `core-install.yaml`, `ha-install.yaml`, `ha-namespace-install.yaml`, the `*-with-hydrator.yaml` variants and `crds.yaml`, all pointing at the published image; `patrol-cd-X.Y.Z.tgz`; `SHA256SUMS` | workflow artifact |
   | `image` | `make image-multiarch PUSH=1`: linux/amd64 + linux/arm64, SBOM and provenance attestations, stamped `vX.Y.Z` | `REGISTRY/patrol-cd/patrol-cd:X.Y.Z` |
   | `publish` | checks `SHA256SUMS`, `helm push`, then `gh release create vX.Y.Z --target <tested sha>` with the changelog section, install commands, image digest and the artifacts | `oci://REGISTRY/charts/patrol-cd`, GitHub release |

   The tag is created last: it exists only when everything else was published. A failed run can be
   re-run from the Actions page or with **Run workflow** on `release.yml` (it checks CI on the
   release commit the same way); pushing the same image tag and chart version again replaces them,
   and the re-run jobs wait for approval again.

Nothing new is proposed while a release is pending, so the release pull request does not reappear
between the merge and the tag.

**Release runs one at a time.** Runs of `release.yml` for CI runs of pushes to `main` and manual
runs on `main` share one concurrency group, and a run waiting for approval holds it. Each push to
`main` meanwhile starts a run that waits behind it, and GitHub keeps only the newest of those.
Approving an older run publishes the release commit its `plan` job chose; if that is no longer
the commit you want released (the version was prepared again since), reject it and approve the
newer run. Events the workflow ignores (CI runs of pull requests, forks or other branches, failed
CI runs, manual runs on other branches) get a group of their own, so they never replace a waiting
release run.

**CI failed on the release commit.** The release workflow reports the release as blocked. For a
flaky failure, re-run the failed jobs of that CI run; its success starts the release. For a real
defect, merge the fix, then run the **Release PR** workflow manually with `version` set to the
pending version: it rebuilds that version's changelog section (now listing the fix) and the merge
of that pull request becomes the release commit. A fix whose type the changelog leaves out changes
no section, and that run fails with nothing to commit (#284).

**No commit is the release commit.** The release workflow reports the release as blocked with
`none of the N commits since … is the merge of this repository's release/next pull request`. This
happens when `CHANGELOG.md` gained the X.Y.Z section some other way, when a later commit changed
that section, or when the release pull request's merge was reverted. Prepare the version again:
run the **Release PR** workflow manually with `version` set to the pending version and merge its
pull request. Right after a merge, GitHub can take a moment to link the pull request to its commit;
if the workflow ran in that window, re-run it.

## Versions

One number for everything: the git tag `vX.Y.Z`, the chart's `version` and `appVersion`, the image
tag `X.Y.Z` (the chart's default `image.tag` is its appVersion) and the version stamped into both
binaries (`vX.Y.Z`).

| Commits since the last tag | Next version |
|----------------------------|--------------|
| a breaking change: `type!:` or a `BREAKING CHANGE:` trailer | major; **minor before 1.0.0** |
| `feat` | minor |
| `fix`, `perf`, `revert` | patch |
| only `docs`, `test`, `chore`, `ci`, `build`, `refactor`, `style` | no release |

The first release (no tag yet) is the chart's current version, `0.1.0`. To choose a version
yourself (1.0.0, say), run the **Release PR** workflow manually with `version` set; it must be
newer than the last tag (or the pending version itself, to rebuild its section). Pre-releases (`-rc.1`) are not produced.

## Changelog

`CHANGELOG.md` is generated, newest release first. Each release section lists *Breaking changes*
(with the trailer's text), *Features*, *Bug fixes*, *Performance* and *Reverts*; other types are
left out. Each line is `**scope:** description`, the pull request it arrived with (from the
`Merge pull request #N` merge commit, or a squash merge's `(#N)` suffix) and the commit. Merge
commits themselves are skipped. Commit subjects are untrusted input: `<`, `>` and `&` are escaped
and control characters removed before they reach the changelog, release notes or pull request body.

## Commit subjects are checked

The `static` CI job runs `make release-lint-commits` on pull requests: every non-merge commit
between the base and the head must be `<type>(<scope>): <description>` (scope optional, `!` for a
breaking change) with a type from the table above. Reword offending commits
(`git rebase -i`, then force-push) or squash-merge with a conventional title.

## Repository settings

| Setting | Why |
|---------|-----|
| Environment `release` (Settings → Environments → New environment) | The `image` and `publish` jobs run in it. Add yourself as a *Required reviewer*, leave *Prevent self-review* off (you start the runs you approve, through your merge), and under *Deployment branches and tags* choose *Selected branches and tags* with the branch `main`. **Create it before the first release:** GitHub creates a missing environment on first use with no protection, and those jobs then run without asking. |
| Settings → Actions → General → *Allow GitHub Actions to create and approve pull requests* | `release-pr.yml` opens the pull request with the workflow token. Required unless `RELEASE_TOKEN` is set; without either, the run fails with an error that names this setting. |
| Label `release` | Put on the release pull request (`gh label create release`). When it is missing the pull request is opened without it and the run warns. |
| Secret `RELEASE_TOKEN` (optional) | Pull requests opened with `GITHUB_TOKEN` do not start CI. A fine-grained token or GitHub App token with *contents* and *pull requests* write on this repository makes CI run on the release pull request. Without it, close and reopen the pull request (or push to it) to start CI. |
| Variable `RELEASE_REGISTRY` (optional) | Where the image and chart go, `host[/namespace]`. Default `ghcr.io/<owner>`. |
| Secret `RELEASE_REGISTRY_TOKEN` (optional) | Registry password when `RELEASE_REGISTRY` is not GHCR; for GHCR the workflow token (`packages: write`) is used. |

A package's visibility on GHCR is its own setting: after the first release, check it in the package's
settings and make it public for anonymous pulls (a package kept private needs an image pull secret
in the cluster).

## Locally

```bash
make release-next                      # the version the next release pull request would propose
make release-preview                   # its CHANGELOG.md section
make release-lint-commits              # check origin/main..HEAD
make release-prepare                   # what the workflow commits (needs helm 4 and make tools)
make release-artifacts RELEASE_VERSION=0.1.0 RELEASE_REGISTRY=ghcr.io/juliosaraiva   # dist/release
```

`go run ./hack/release status` prints the release state (`current`, `tag`, `tagged`, `changelog`,
`pending`); `GH_TOKEN=… go run ./hack/release select -repo owner/name` adds the release commit
(`sha`), its pull request (`pull`), its CI result (`ci`, `ci_reason`) and the decision (`release`),
as the workflow sees them. Nothing here pushes, tags or publishes.

## Not covered yet

Signing (cosign) of the image, chart and checksums; GitHub artifact attestations for the binaries;
pre-releases and release branches for patch releases of older minors; a Homebrew tap or other
package managers.
