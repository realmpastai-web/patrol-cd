# Component: `internal/cli/admin` (`patrol admin`)

Status: implemented; every command is exercised by tests (envtest kube-apiserver 1.36 for the
commands that talk to Kubernetes). Not yet wired into `cmd/patrol` (owned by the CLI core
workstream, see [Integration](#integration)).
Decision records: [ADR-0002](../adr/0002-api-group-and-versioning.md) (API group and its
migration), [ADR-0003](../adr/0003-resource-model.md) (Repository/Cluster CRDs, credentials
Secrets), [ADR-0004](../adr/0004-no-redis.md) (no Redis), [ADR-0006](../adr/0006-internal-apis.md)
(repo-server token), [ADR-0012](../adr/0012-notifications.md).
Parity status: [`docs/parity/status/cli-admin.yaml`](../../parity/status/cli-admin.yaml).

`patrol admin` is the counterpart of `argocd admin` (Argo CD v3.5.5): administrative commands
that use the kubeconfig credentials directly instead of the Patrol CD API server. They read and
write the Patrol CD objects (group `cd.patrol.example`), the settings ConfigMaps/Secrets
(`patrol-cm`, `patrol-rbac-cm`, `patrol-cmd-params-cm`, …) and, for the repo-server backed
commands, call the repo-server with the internal token.

## Integration

```go
f := &admin.Factory{IOStreams: admin.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr}, PromptsEnabled: true}
admin.Register(root, f)              // adds "admin" once
err := root.ExecuteContext(ctx)
os.Exit(admin.ExitCode(err))         // 0, 1, or the command's code (argocd-import: 2)
```

`Factory` (in `factory.go`) is deliberately narrow: streams, an optional `*slog.Logger`
(default: text at warn level to `ErrOut`), an optional `RESTConfig` override (tests, embedding),
`PromptsEnabled` and a clock. kubectl connection flags (`--kubeconfig`, `--context`, `-n`, `--server`,
`--token`, `--insecure-skip-tls-verify`, …) are persistent flags of `admin` and shadow root flags of the
same names. Errors that carry an exit code are `*admin.ExitError`.

## Commands

| Command | Notes |
|---------|-------|
| `export` | Settings ConfigMaps, referenced and Patrol-labelled Secrets, AppProjects, SCMProviders, Repositories, RepositoryCredentials, Clusters, ApplicationSets, Applications (control-plane namespace plus `--application-namespaces`/`--applicationset-namespaces` or the cmd-params globs; `--namespace-scoped`). Strips uid, resourceVersion, managedFields, status, last-applied; owner references kept with UID cleared. `--include-secrets` (default true) prints a warning that base64 is not encryption; `-o FILE` is written with mode 0600. `--include-operations` adds Operations (with status, as an audit record). |
| `import SOURCE` | Creates/updates in dependency order; owner references re-pointed to the new UIDs (dangling ones dropped with a warning). Never deletes without `--prune`. Prune safety: settings ConfigMaps, `patrol-secret` and `patrol-notifications-secret` are never pruned; Secrets are pruned only when the backup contains Secrets (a `--include-secrets=false` backup prunes none) and only when managed by Patrol CD (`cd.patrol.example/secret-type` label or `managed-by: patrol-cd` annotation; Secrets that are only referenced are restored but never pruned); Applications' finalizers are removed before a prune delete; objects owned by a pruned ApplicationSet, Repository, RepositoryCredential, Cluster or SCMProvider are released (owner reference removed) before the owner is deleted with a UID precondition, so neither the garbage collector nor the ApplicationSet controller's ordered deletion removes them. `--dry-run`, `--verbose`, `--override-on-conflict`, `--skip-resources-with-label`, `--stop-operation` (sets `spec.cancel` on running Operations), `--ignore-tracking`, `--prompts-enabled`. Objects of another API group are refused (Argo CD's points to `argocd-import`, other Patrol groups require `--migrate-group`). Operations are never re-created. A backup that points a Cluster at another server than the live Cluster of the same name is refused before anything is written, with or without `--dry-run`, because `spec.server` cannot change; every such Cluster is listed. A difference of trailing slashes is the same server and keeps the stored text. `--keep-live-clusters` keeps the live Cluster and the Secret it references, also from `--prune`, and restores the rest; a Cluster skipped by label is not refused and keeps that Secret the same way, whether or not the backup holds that Cluster. Such a Secret is held before the first write, and reported as not restored if it is gone. A backup that would start another Cluster reading a held Secret is refused too, unless the backup uses that Secret for the server of the Cluster it is held for. A Secret the backup's Cluster references but nothing live does is updated rather than created, so a Cluster written by hand can be restored; TLS Secrets stay out of that. An object the backup gives an owner gets that owner even if it otherwise matches, so a Secret orphaned by that procedure belongs to the new Cluster. Taking the backup's server is a manual procedure, in `patrol admin import --help`. |
| `settings validate` | Upstream groups and output (`✅`/`❌`). Flags renamed `--patrol-cm-path`/`--patrol-secret-path` (hidden `--argocd-*` aliases), `--load-cluster-settings`; Patrol addition `--settings-file` (repeatable) for the other settings objects. A file Secret without `server.secretkey` is validated as if present (patrol-server generates it). |
| `settings resource-overrides` | `ignore-differences`, `ignore-resource-updates`, `health`, `list-actions`, `run-action` (alias `action`), using the real diff normalizers and the Lua runtime; upstream messages. |
| `settings rbac can / validate` | Upstream wording and exit codes (`Yes`/`No`, `-q`), policies from `--policy-file` (CSV or ConfigMap YAML) or `--namespace`. |
| `notifications template get/notify`, `trigger get/run` | Cluster configuration or `--config-map`/`--secret` files (`-` = stdin, `:empty`); repo-server flags for `repo.*` helpers. RESOURCE_NAME is a file when it exists, else an Application. `template notify` delivers to `console` recipients and, **by default, only renders other services** (`--dry-run=false` sends, as upstream always does). |
| `cluster stats / shards` | Same flags and sharding algorithms (`internal/controller/sharding`); statistics come from `Cluster.status` written by the controller (no Redis). Redis flags are accepted and reported as ignored. |
| `cluster namespaces`, `enable-namespaced-mode`, `disable-namespaced-mode` | `--dry-run` default true, `--max-namespace-count`, `--cluster-resources`. |
| `cluster kubeconfig CLUSTER_URL [OUTPUT_PATH]` | Kubeconfig equivalent to the controller's REST config. Written with mode 0600 to OUTPUT_PATH/`--output-file`; printed to stdout only with credentials redacted (upstream requires the path). |
| `cluster generate-spec CONTEXT` | Cluster object plus credentials Secret (ADR-0003). `--generate-bearer-token` creates `<sa>-long-lived-token` and waits for the token. Credentials are redacted on stdout; `--output-file` (0600) has them. |
| `app generate-spec`, `get-reconcile-results`, `diff-reconcile-results` | Same flags as `app create`. `--refresh` builds settings, cluster registry, repository resolver, live-state cache and the comparison engine in-process and calls the repo-server (`--repo-server`, `--repo-server-token-file`, TLS by default, `--repo-server-plaintext` explicit). |
| `repo generate-spec REPOURL` | Repository object (named like the API's, `repo-<hash>`) plus `<name>-creds` Secret; validations of `repo add`; password prompt. Redacted on stdout except username and client certificate; `--output-file` (0600). |
| `proj generate-spec`, `update-role-policy`, `generate-allow-list` | `update-role-policy` defaults to dry run and prints a unified diff; unchanged policies are not rewritten. `generate-allow-list` resolves resources through discovery (upstream rules: first apiGroup, create verb). |
| `initial-password` | Upstream output from `patrol-initial-admin-secret`. |
| `redis-initial-password` | No Redis (ADR-0004): prints a not-applicable message, exit 0. |
| `dashboard` | See below. |
| `migrate-group` | Patrol addition (ADR-0002): copies the configuration objects from `--from-group` to `--to-group`, rewriting keys, finalizers, owner references and adopting credentials Secrets; dry run by default; sources are never modified. |
| `argocd-import SOURCE` | Patrol addition: see below. |

### `dashboard`

Upstream starts an in-process API server and UI with authentication disabled. Patrol CD
instead serves a loopback reverse proxy (`--address localhost`, `--port 8080`) to a running
`patrol-server` pod reached through the Kubernetes API (`pods/portforward`); users log in as on
the server. The upstream TLS connection is verified: the certificate in `patrol-secret`
(`tls.crt`) is pinned when present, otherwise `--server-crt` or the system roots are used;
`--insecure` and `--plaintext` are explicit opt-outs. A non-loopback `--address` prints a
warning. The browser's Host header is kept (server-side origin checks); Server-Sent Events are
streamed.

Port forwarding speaks the kubelet WebSocket channel protocol (`v4.channel.k8s.io`) with
`github.com/coder/websocket`: client-go's `tools/portforward` needs `github.com/gorilla/websocket`
and `k8s.io/streaming`, which are not in `go.mod`/`go.sum` (this workstream may not change them).
Messages are at most 32 KiB including the channel byte; received messages are bounded to 1 MiB.

### `argocd-import`

Reads an Argo CD export (`argocd admin export` output or any stream of Argo CD Applications,
AppProjects, ApplicationSets, repository/credential-template/cluster Secrets and `argocd-*`
settings) and converts it:

- settings: `argocd-*` → `patrol-*`; `resource.customizations.*.argoproj.io_{Application,ApplicationSet,AppProject}` keys and the legacy `resource.customizations` YAML move to `cd.patrol.example`; service addresses in cmd-params renamed; the result is validated by `internal/settings` and its findings reported;
- `argocd-secret`: `server.secretkey` and `accounts.*.tokens` are not copied (sessions and API tokens signed by Argo CD are not valid); the admin bcrypt hash is kept;
- repository, repo-creds (and `-write` variants) and cluster Secrets become Repository, RepositoryCredential and Cluster objects with a `<name>-creds` Secret (owner reference added when created); cluster `config` JSON is decoded strictly;
- Argo CD keys (`argocd.argoproj.io/…`, `notifications.argoproj.io/…`, recipients annotations) are mapped; transient keys (refresh, hydrate, tracking-id, notified state) are dropped; ApplicationSet generator selector keys are mapped;
- Applications and ApplicationSet templates: automated sync is disabled (`automated.enabled: false`, settings kept) unless `--keep-auto-sync`; Argo CD finalizers are removed (the Patrol equivalent is named); in-flight `operation` is dropped; project JWT tokens are dropped. ApplicationSet templates are `spec.template` and every generator-level `template` (list, clusters, git, scmProvider, clusterDecisionResource, pullRequest, plugin, matrix, merge, nested children at any depth): generator fields win when merged (`internal/appset/template.MergeTemplate`), so each is converted and reported with its path (`generators[0].matrix.generators[1]….template`);
- `spec.sourceHydrator` (Applications and templates) is a `WARNING`: with `hydrator.enabled`, Patrol CD hydrates an Application without hydration status as soon as it exists (hydrator component, decision "no currentOperation → hydrate"), committing and pushing to the sync (or `hydrateTo`) branch with write credentials whatever the sync policy; the finding names repository and branch and tells to stop Argo CD's hydrator first. `hydrator.enabled: true` in `argocd-cmd-params-cm` is reported too. It is a warning, not blocking: the import itself writes no git data and the hydrator only runs when enabled in Patrol CD;
- the report lists every produced object and findings: `WARNING` (finalizers, resource-tracking transfer procedure, credentials, prune/selfHeal, source hydrator git writes, notification subscriptions and delivery state, namespaces not enabled, settings issues) and `BLOCKING` (unknown fields under strict decoding, unsupported kinds or repository types, invalid Secrets, invalid projects). Blocking findings exit with code 2 and nothing is written.

Default is a dry run. `-o FILE` (0600; Secrets unless `--include-secrets=false`) writes the
converted objects; `--dry-run=false` creates them in the namespace (existing objects are left
unchanged; settings are merged with `--overwrite-settings`). Secret values are never printed.
Ownership of live resources is never taken automatically: the report prints the transfer
procedure for the tracking method in use.

## Security properties

- Secret values are printed only where emitting them is the command's purpose
  (`initial-password`); generate-spec/kubeconfig output redacts credentials unless written to a
  file. No secret is logged (client-go warnings are routed to stderr, deduplicated).
- Every file that may hold credentials (export, generate-spec, kubeconfig, argocd-import
  output) is created or truncated with mode 0600 (an existing file is chmod-ed).
- TLS verification is on by default for the repo-server and patrol-server connections.
- Inputs are bounded: backups 256 MiB, other files 16 MiB, WebSocket messages 1 MiB; URL inputs
  have a 30 s timeout.
- Import and argocd-import refuse foreign API groups and never delete without `--prune`.
- Import checks every Cluster it can list before its first write and never replaces one: deleting
  a Cluster makes ApplicationSets delete its Applications, and its credentials Secret goes with it.
  An account that cannot list Clusters gets the warning from the list and no check.

## Differences from upstream

- No Redis: `redis-initial-password` is a no-op; Redis flags of `cluster stats/shards` and
  `dashboard` are accepted and ignored with a warning.
- `dashboard` proxies to a real `patrol-server` (login required) instead of starting an
  unauthenticated in-process server.
- `template notify` does not contact services other than `console` unless `--dry-run=false`.
- `cluster kubeconfig` accepts one argument (redacted stdout) besides the upstream two.
- generate-spec commands emit Patrol CD objects (Repository/Cluster + credentials Secret)
  instead of Argo CD Secrets; JSON output of several objects is a `v1` List and YAML a
  multi-document stream.
- `export` writes SCMProviders, Repositories, RepositoryCredentials and Clusters and can include
  Operations; import never recreates Operations.
- `export --include-secrets` (default true, upstream always exports Secrets) and the "referenced
  Secret" rule (Secrets named by Patrol object specs or `$secret:key` settings values that start
  a value or YAML scalar) are Patrol additions; `import --prune` keeps upstream's Secret prune
  rule (fixed name, secret-type label, managed-by annotation) and, unlike upstream, never prunes
  Secrets of a Secret-less backup, `patrol-secret` or `patrol-notifications-secret`, and never
  lets a pruned owner cascade to objects it owns (upstream deletes an ApplicationSet with
  background propagation).
- Upstream keeps a cluster's server and credentials in one Secret, so an import moves both and
  never refuses. Patrol keeps them in two objects and `spec.server` is immutable, so `import`
  refuses a backup that moves a live Cluster, and `--keep-live-clusters` is a Patrol addition.
  `--skip-resources-with-label` on a Cluster also keeps the Secret the live Cluster references,
  although that Secret does not carry the label, because upstream's label covers both.
  Two Cluster objects can read one Secret here, which upstream's model cannot express, so `import`
  also refuses a backup that would start a Cluster reading a held Secret at another server.
- Settings flags are named after Patrol objects (`--patrol-cm-path`, hidden `--argocd-cm-path`).
- `--logformat`/`--loglevel` on `admin` are accepted (they only apply to `admin` itself, as
  upstream).

## Not implemented / not verified

- The port-forward tunnel has only been exercised against a protocol-faithful fake server
  (no kubelet in envtest; `admin dashboard` was not run against the kind clusters of
  2026-10-19); the dashboard proxy, TLS pinning and pod lookup are tested.
- `cluster stats/shards` read statistics the controller writes to `Cluster.status`; the tests
  seed that status.
- `initial-password` reads the Secret; creating it at first start belongs to patrol-server
  (OPS-031).
