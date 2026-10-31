# Component: `internal/clusters`

Status: implemented (library; not yet wired into the controller, API server or CLI).
Decision records: [ADR-0003](../adr/0003-resource-model.md) (Cluster kind referencing a
Secret), [ADR-0008](../adr/0008-sharding-and-ha.md) (sharding uses the registry),
[ADR-0009](../adr/0009-authn-authz.md).

Destination-cluster access: turning a `Cluster` object and its credentials Secret into a
`*rest.Config` with Argo CD v3.5.5's authentication precedence, in-process EKS/GKE/AKS tokens,
the informer-backed cluster registry with the synthetic in-cluster cluster, connection probing
and status writing, impersonation, and the `patrol-manager` bootstrap used by
`patrol cluster add`. The sub-package `internal/clusters/k8sauth` provides the
`patrolcd k8s-auth aws|gcp|azure` exec credential plugin (upstream `argocd-k8s-auth`).

## Responsibilities

1. Build connection configuration (`RawRestConfig`, `RestConfig`, `ApplyTuning`) with the
   upstream `Cluster.RawRestConfig` precedence and the `SetK8SConfigDefaults` transport tuning.
2. Produce cloud bearer tokens in-process: AWS (presigned `sts:GetCallerIdentity`,
   aws-sdk-go-v2, optional AssumeRole and profile), GCP (application default credentials,
   `golang.org/x/oauth2/google`), Azure (azidentity; workload identity by default).
3. Keep a read-only, informer-backed view of the Cluster objects and credential Secrets of
   the control-plane namespace plus the synthetic `in-cluster` cluster (`Registry`), resolve
   Application destinations with upstream messages, and publish effective changes.
4. Probe connectivity (`Probe`), map live-state cache information to the upstream connection
   state (`ComputeClusterInfo`) and persist `Cluster.status` with retries (`StatusWriter`).
5. Apply impersonation to a configuration (`ImpersonatingConfig`). Which service account to
   impersonate is decided by `internal/project` (`SelectServiceAccount`).
6. Install, rotate and remove the `patrol-manager` identity in a destination cluster
   (`EnsureManagerServiceAccount`, `RotateManagerToken`, `UninstallManager`).

Out of scope: the ClusterService API, `patrol cluster` / `patrol admin cluster` commands and
the UI (REPO-098..102, 106, 108..110 surfaces); the periodic cluster-info updater, sharding and the
live-state cache (`internal/controller`, `internal/cache`); project permission evaluation and
service-account selection (`internal/project`); settings parsing (`internal/settings`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `RawRestConfig(ctx, cluster, secretData, Options) (*rest.Config, error)` | Untuned config (upstream `RawRestConfig`). `secretData` is the data of the Secret named by `spec.config.credentialsRef` (keys `ClusterSecretKey*`). |
| `RestConfig(ctx, cluster, secretData, Options)` | `RawRestConfig` + `ApplyTuning` (upstream `RESTConfig`). |
| `Options` | `Tuning` (nil = `DefaultTuning()`), `UserAgent`, `InClusterConfig` (nil = `rest.InClusterConfig`), `FakeInCluster` (`FakeInClusterFromEnv()` reads `PATROL_FAKE_IN_CLUSTER`), `AWSAuthExec` (upstream exec form instead of in-process tokens), `K8sAuthCommand` (default `patrolcd`), `DisableNativeK8sAuth`, `AWS`/`GCP`/`Azure` provider overrides, `Providers` (token-source cache; nil = process-wide LRU of 1024), `Now`. |
| `Tuning`, `DefaultTuning()`, `TuningFromEnv(logger)`, `TuningFromLookup(lookup, logger)`, `ApplyTuning(cfg, Tuning)` | REPO-105 knobs: `PATROL_K8S_CLIENT_QPS` (50), `PATROL_K8S_CLIENT_BURST` (2×QPS), `PATROL_K8S_CLIENT_MAX_IDLE_CONNECTIONS` (500, also per host), `PATROL_K8S_TCP_TIMEOUT` (30s), `PATROL_K8S_TCP_KEEPALIVE` (30s), `PATROL_K8S_TLS_HANDSHAKE_TIMEOUT` (10s), `PATROL_K8S_TCP_IDLE_TIMEOUT` (5m), `PATROL_K8S_SERVER_SIDE_TIMEOUT` (0), `PATROL_K8SCLIENT_RETRY_MAX` (0 = off), `PATROL_K8SCLIENT_RETRY_BASE_BACKOFF` (100 ms). Out-of-range or unparsable values keep the default (warning). |
| `ParseProxyURL`, `ExecConfig`, `AWSExecArgs` | Proxy scheme check (http, https, socks5); `execProviderConfig` → `clientcmdapi.ExecConfig`; upstream `aws --cluster-name … [--role-arn …] [--profile …]` arguments. |
| `Credentials`, `CredentialsFromSecretData`, `(Credentials).SecretData` | Typed view of the credentials Secret; `String`/`GoString` never print values. |
| `AWSToken`, `AWSTokenWithRetry`, `AWSTokenRequest`, `DecodeAWSToken` | EKS token `k8s-aws-v1.` + base64url(presigned URL), `x-k8s-aws-id` and `X-Amz-Expires: 60` signed, expiry now + 14 min. `Region`, `STSEndpoint`, `HTTPClient`, `LoadOptions` are not part of the Cluster API. |
| `GCPTokenSource(GCPOptions)` | Scopes `cloud-platform` and `userinfo.email`; `GOOGLE_APPLICATION_CREDENTIALS` from the lookup (exec env first) or the ADC search. |
| `AzureTokenSource(AzureOptions)` | Scope `<AAD_SERVER_APPLICATION_ID or 6dae42f8-4368-4678-94ff-3960e28e3630>/.default`; `AAD_LOGIN_METHOD` `workloadidentity` (default), `spn` (secret or certificate), `msi`, `azurecli`, `ropc`; `AAD_ENVIRONMENT_NAME`, `AZURE_AUTHORITY_HOST`. Credentials are created on first use. |
| `Token`, `TokenSource`, `TokenSourceFunc`, `ProviderCache`, `NewProviderCache` | Token plumbing. Tokens are cached until one minute before expiry and dropped after a 401. |
| `ExecCredentialJSON(Token)` | `client.authentication.k8s.io/v1beta1` ExecCredential printed by `k8s-auth`. |
| `NewRegistry(dyn, kube, RegistryOptions)`, `(*Registry).Start(ctx)`, `HasSynced` | Registry over a dynamic client (Clusters) and a clientset (Secrets labeled `cd.patrol.example/secret-type=cluster`, configurable). Options: `Namespace` (required), `InClusterEnabled` (nil = true), `ResyncPeriod` (3 min), `SecretLabelSelector`, `LocalServerVersion` (one probe at Start for the synthetic cluster), `Logger`, `Now`. |
| `ByServer`, `ByName`, `ServersByName`, `All`, `ProjectClusters` | Lookups (deep copies). `GetCluster`, `GetClusterServersByName`, `GetProjectClusters` give the same with the `internal/project.ClusterLookup` signatures (compile-time checked in tests). |
| `ResolveDestination`, `DestinationCluster`, `CountApplications` | Upstream `GetDestinationServer` / `GetDestinationCluster` messages; application count per server. |
| `SecretData`, `(*Registry).RestConfig`, `(*Registry).RawRestConfig` | Credentials lookup and config building for a registry cluster. |
| `SetInClusterEnabled`, `InClusterEnabled(settings)`, `ValidateRegistration` | `cluster.inClusterEnabled` (enabled unless exactly `"false"`); `cannot register cluster: in-cluster has been disabled`. |
| `Subscribe(func(Event)) (cancel)`, `Event{Type, Server, Old, New, Changes}` | Effective-change notifications (Added/Modified/Deleted; `Changes{Identity, Spec, Metadata, Credentials}`). |
| `IsSynthetic`, `IsSkipReconcile`, `RefreshRequestedAt`, `NeedsCacheInvalidation`, `DiffCacheSettings`, `NormalizeNamespaces`, `NormalizeServer`, `AutoLabelKubernetesVersion`, `ObjectNameForServer` | Helpers for REPO-092/096/097/079/095/104. |
| `Probe(ctx, cfg) (version, error)` | `GET /version`, normalized to `v<major>.<minor>.<patch>`. |
| `ComputeClusterInfo(*CacheState, apps, now) StatusUpdate`, `ApplyClusterInfo`, `ProbeUpdate` | REPO-094 mapping; `ApplyClusterInfo` is what `RecordClusterInfo` writes, so the updater can compare before writing. Sync times have whole seconds and condition messages at most 32,768 bytes, as the API stores them. |
| `ApplicationCounts(apps, permitted) map[string]int64`, `CountApplications`, `WantsVersionLabel`, `ClusterInfoIntervalFromLookup` | Inputs of the updater in `internal/controller/clusterinfo`. |
| `NewStatusWriter(dyn, ...)`, `Update`, `RecordProbe`, `RecordClusterInfo`, `RecordVersionLabel` | Status subresource writes (re-read + retry on conflicts and transient errors with `DefaultStatusBackoff`); `Ready`/`ConnectionFailed` conditions with `observedGeneration`; label patch for REPO-095. |
| `ImpersonatingConfig(base, user)`, `ServiceAccountUsername` | Impersonation of a project-selected service account. |
| `EnsureManagerServiceAccount(ctx, kube, namespace, clusterWide, namespaces, *BootstrapOptions) (token, error)`, `ServiceAccountBearerToken`, `RotateManagerToken(ctx, kube, oldToken, persist, opts)`, `UninstallManager`, `ParseServiceAccountToken` | `patrol-manager` SA, `patrol-manager-role(-binding)` (ClusterRole with `*/*/*` + nonResourceURLs `*`, or a Role with `*/*/*` per namespace), `patrol-manager-long-lived-token` Secret polled every 500 ms for 30 s (10 s per request). |
| Errors | `ErrClusterNotFound` (`*NotFoundError`, `cluster "<s>" not found` / `cluster "<s>" is disabled`), `ErrInClusterDisabled`, `ErrAmbiguousName` / `*NameError` (`there are no clusters with this name: <n>`, `there are <k> clusters with the same name: [<s> <s>]`), `ErrSecretNotFound` (`*SecretError`), `ErrInvalidConfig`, `ErrNotSynced`, `ErrNotLegacyToken`, `ErrNoBearerToken`. |
| `k8sauth.NewCommand(k8sauth.Config) *cobra.Command` | `k8s-auth` with `aws` (`--cluster-name`, `--role-arn`, `--profile`; presign retried every 5 s for 1 min), `gcp` and `azure`. To be wired into `patrolcd`. |

### Authentication precedence (REPO-091)

1. In-cluster server (`https://kubernetes.default.svc`, trailing `/` ignored) with
   `FakeInCluster`: local kubeconfig (`KUBECONFIG` list or `~/.kube/config`).
2. In-cluster without username, password or bearer token: the pod configuration.
3. In-cluster with any of them: the pod configuration with those credentials, token file
   cleared. TLS, AWS and exec settings are ignored in-cluster.
4. Otherwise `awsAuthConfig` (in-process tokens, or with `AWSAuthExec` the exec form
   `patrolcd k8s-auth aws --cluster-name …`), then `execProviderConfig`, then bearer token /
   basic auth — each with `insecure`, `serverName` and the Secret's CA, client cert and key.
5. Then `proxyURL`, `disableCompression`, server-side timeout, QPS and burst.

Exec configurations that run the helper — `patrolcd k8s-auth <aws|gcp|azure> …` or upstream's
`argocd-k8s-auth <aws|gcp|azure> …` (migrated clusters) — are served in-process unless
`DisableNativeK8sAuth` is set: `execProviderConfig.env` takes precedence over the process
environment for GCP and Azure. An AWS exec configuration with `env` or unknown flags is
executed as a subprocess (the AWS SDK reads the process environment).

When the credentials Secret holds both `certData` and `keyData`, no in-process cloud token is
produced: `awsAuthConfig` and helper exec configurations keep the (inert) exec form, because
client-go never runs an exec plugin when certificate auth is configured
(`exec.Authenticator.UpdateTransportConfig`). Such clusters authenticate with mutual TLS only,
exactly as upstream, and need no cloud credentials. A certificate without a key does not count.

`ApplyTuning` bakes TLS, auth wrappers, exec and proxy settings into one tuned
`http.Transport` and clears `TLSClientConfig`, `ExecProvider`, `AuthProvider`,
`WrapTransport` and `Impersonate` (baked), keeping bearer/basic fields as upstream does. Apply
`ImpersonatingConfig` after `RestConfig`.

### Registry semantics

- Informers: Clusters through the dynamic client (converted once to `*v1alpha1.Cluster`,
  `spec.server` trailing `/` trimmed, `spec.namespaces` trimmed and empty entries dropped,
  managedFields dropped); Secrets selected by label. Objects that do not convert are skipped
  with a warning (REPO-077).
- Effective cluster per server: the Cluster object with the smallest name (duplicates are a
  misconfiguration; warning logged; same rule as `internal/project.ClusterIndex`). The
  in-cluster server is served by such an object, else by the synthetic cluster
  `{name: in-cluster}` whose status comes from one probe at `Start`; it is hidden entirely
  while `cluster.inClusterEnabled` is false.
- Name lookups only consider objects that serve their server; `in-cluster` resolves to the
  in-cluster server when enabled and no cluster carries that name (even if an object renamed
  it — upstream behavior).
- Events describe the effective server → cluster mapping. Status-only updates publish
  nothing (the StatusWriter does not cause invalidation loops). Secret data changes publish
  `Modified` with `Changes.Credentials` (data fingerprint, not resourceVersion). An object for
  the in-cluster server replaces the synthetic cluster with `Modified` (`Changes.Identity`);
  deleting it restores the synthetic cluster with `Modified`; disabling in-cluster publishes
  `Deleted`. Subscribers get `Added` for the current state first; delivery is ordered and
  asynchronous per subscriber.

## Invariants

- Credentials never appear in logs or error strings: `Credentials` and `Token` print only
  presence/expiry; Secret data, token values and environment values are never logged; the
  provider cache keys are SHA-256 hashes.
- `RawRestConfig` never mutates its inputs, including the configuration returned by
  `Options.InClusterConfig` (copied before proxy, limits and tuning are applied; a nil result
  is `ErrInvalidConfig`); lookups return deep copies; events carry copies.
- The registry publishes, for each server, a sequence that replays to the current effective
  mapping (property-tested against an independent model with random object, Secret and
  setting changes).
- `ImpersonatingConfig` never mutates `base`; impersonation headers are sent exactly once.
- `EnsureManagerServiceAccount` converges: re-running restores tampered rules and subjects,
  replaces bindings whose immutable `roleRef` differs, and reuses the token Secret.
- `RotateManagerToken` deletes the old token Secret only after `persist` succeeded; on any
  earlier failure the new Secret is removed and the old token stays valid.

## Failure modes

| Situation | Result |
|-----------|--------|
| Unsupported proxy scheme | ``unable to create K8s REST config, can`t parse proxy url: failed to parse proxy url, unsupported scheme "<s>", must be http, https, or socks5`` (upstream code wording, including its backquote; the parity inventory writes `can't`) |
| Pod not in a cluster (in-cluster server) | `unable to create K8s REST config: …` wrapping `rest.ErrNotInCluster` |
| AWS config/profile error | `error loading AWS configuration: …`; k8s-auth: `timeout while trying to get signed aws request: last error: …` after 1 min |
| GCP/Azure credential problems | Request errors `get cluster bearer token: …` (config building succeeds, like the exec plugin); unsupported Azure login method / environment fail at construction |
| Missing credentials Secret | `*SecretError` (`ErrSecretNotFound`) naming the required label |
| Unknown server / disabled in-cluster / bad name | `*NotFoundError` / `*NameError` with upstream messages; `DestinationCluster` adds `error getting cluster by server "<s>": …` or `error getting cluster by URL: …` |
| Lookup before `Start` | `ErrNotSynced` |
| Probe | `failed to get server version: …`, `failed to parse server version: …` |
| Status write conflict / transient API error | re-read and retry (5 attempts, 100 ms doubling, cap 5 s); NotFound returned as is; synthetic cluster rejected |
| Token Secret never filled | `failed to get token for serviceaccount "patrol-manager": …`; Secret of another SA: `secret "<n>" is not a token secret of serviceaccount "<sa>"` |
| Rotation of a bound/projected token | `ErrNotLegacyToken`; empty token `ErrNoBearerToken` |

## Security notes

- `execProviderConfig` runs arbitrary commands in the controller with the controller's
  environment plus `env` — as upstream; only cluster administrators may create Cluster
  objects. In-process GCP/Azure providers read files named by `GOOGLE_APPLICATION_CREDENTIALS`
  / `AZURE_CLIENT_CERTIFICATE_PATH` / `AZURE_FEDERATED_TOKEN_FILE` from the same sources.
  GCP credential files are restricted to the known ADC types.
- Unlike upstream's API, Patrol stores `execProviderConfig` in the Cluster spec, readable by
  anyone allowed to read Cluster objects: do not put secrets in its `args` or `env` (the CRD
  field documentation says so); the API layer must still sanitize it (REPO-088).
- `tlsClientConfig.insecure` disables verification; certificates and keys come only from the
  referenced Secret.
- Registry Secrets are label-selected to keep the watch narrow; RBAC still needs `list/watch`
  on Secrets in the control-plane namespace.
- The bootstrap grants full cluster (or namespace) access to `patrol-manager`, as upstream;
  `UninstallManager` removes it. Tokens are long-lived legacy Secrets (upstream behavior);
  rotation replaces them.
- `ParseServiceAccountToken` does not verify signatures; it only locates the Secret to
  rotate. The cluster verifies tokens.

## Parity IDs covered

See `docs/parity/status/clusters.yaml`. Implemented (library): REPO-077, 084, 085, 086, 087,
089, 090, 091, 095, 105, 107, 111, 112, 113. In progress (library part done, surfaces or
loops elsewhere): REPO-078, 079, 080, 081, 088, 092, 093, 094, 096, 097, 103, 104, 106,
SYNC-093.

## Differences and gaps

- Storage is the `Cluster` CRD plus a referenced Secret (ADR-0003), so REPO-080's `"True"`
  quirk, REPO-082's string parsing and REPO-104's data-replacement semantics do not apply to
  the registry; names are required by the CRD (no name = server defaulting in this package).
- Duplicate servers: upstream picks an arbitrary Secret; Patrol picks the smallest object
  name and logs a warning.
- `UninstallManager` removes objects from the namespace used at install and the per-namespace
  Roles (upstream always uses `kube-system` and leaves Roles).
- Native cloud tokens: EKS/GKE/AKS work without a helper binary. The kubelogin login methods
  `devicecode` and `interactive` are not supported (no terminal in the controller); `azd`,
  `azurepipelines` are not implemented.
- Exec environment variables are passed sorted by name (upstream: map order).
- `ComputeClusterInfo` bounds what a destination reports, so that the API cannot refuse a status
  and lose the connection state with it: a server version over 256 bytes and API versions over
  512 KiB as stored are left out, which leaves the stored ones in place (renders read them, and
  `ApplyClusterInfo` keeps what an update does not have), and a connection message is stored as
  valid UTF-8 and cut to 32768 bytes. `WantsVersionLabel` skips a version the API would refuse as a label value.
- `spec.server` is immutable on the CRD as well as through the API, because the status describes
  that destination and `ApplyClusterInfo` keeps its version and API versions: `kubectl` cannot move
  a Cluster, and a new destination needs a new object. `patrol admin import` refuses a backup that
  would move one before it writes anything ([cli-admin](cli-admin.md)). Upstream's Secret has no
  such rule. The rule compares the stored text, and the API normalizes only what it reads, so a
  server stored with a trailing `/` keeps it.
- The implicit in-cluster cluster has no object, so no cluster information (#246).
- The synthetic in-cluster version uses the `v<major>.<minor>.<patch>` form like every other
  cluster (upstream omits the `v` for the synthetic one only).
- `RawRestConfig` with in-process cloud auth carries a `WrapTransport` and cannot be
  serialized into a kubeconfig; use `AWSAuthExec` / `DisableNativeK8sAuth` for
  `admin cluster kubeconfig`-style export (REPO-109).
- Not implemented here: the ClusterService API (REPO-098..103 handlers), CLI (REPO-106,
  108, 109), UI (REPO-110), the cluster-info updater (REPO-094, `internal/controller/clusterinfo`),
  cache invalidation wiring (REPO-096), shard filtering with skip-reconcile
  (REPO-097), project destination rules (REPO-081, `internal/project`).
- Requested API addition (not made; `api/v1alpha1` is owned elsewhere): a
  `KubernetesInClusterName = "in-cluster"` constant next to
  `KubernetesInternalAPIServerAddr`; this package declares `InClusterName` meanwhile.

## Adapting (callers)

- Controller: `reg := clusters.NewRegistry(dyn, kube, {Namespace, InClusterEnabled:
  ptr(clusters.InClusterEnabled(patrolCM)), LocalServerVersion: probe of the pod config})`;
  call `SetInClusterEnabled` on settings changes; subscribe and, per event, invalidate
  `internal/cache` when `DiffCacheSettings(old,new).Any()`, `Changes.Credentials` or
  `Changes.Identity`, or `NeedsCacheInvalidation(new, info.LastCacheSyncTime)`; skip
  Applications whose cluster `IsSkipReconcile`. Build configs with
  `reg.RestConfig(ctx, c, clusters.Options{Tuning: ptr(clusters.TuningFromEnv(log)),
  FakeInCluster: clusters.FakeInClusterFromEnv()})`, then `ImpersonatingConfig` with the user
  from `project.SelectServiceAccount`. `internal/controller/clusterinfo` writes the status every
  cluster-info interval.
- API server: pass the registry as `project.ClusterLookup`; on create/update run
  `ValidateRegistration`, `RestConfig` + `Probe` (`error getting server version: …`), then
  `RecordProbe`.
- CLI `patrol cluster add`: `EnsureManagerServiceAccount(ctx, kube, systemNS, len(ns)==0, ns,
  nil)`; name objects with `ObjectNameForServer`; `rotate-auth` uses `RotateManagerToken`
  with a `persist` that writes the credentials Secret after a `Probe` with the new token.

## Verification

Run on 2026-10-13 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0):

```bash
go build ./...                                    # ok
go vet ./internal/clusters/...                    # ok
golangci-lint run ./internal/clusters/...         # 0 issues
go test -race -count=1 ./internal/clusters/...    # ok; the 5 envtest tests skip
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/clusters/...  # ok; 96 tests/subtests pass, none skipped
go run ./hack/parity                              # docs/parity/status/clusters.yaml parses, no warnings
```

Tests: precedence table, TLS (custom CA, unknown CA, insecure, serverName, mTLS), bearer and
basic headers, compression (tuned and raw), HTTP proxy, exec plugin (the test binary acts as
the plugin and reports args, env and `KUBERNETES_EXEC_INFO`), AWS exec-form arguments, native
recognition table, tuning env parsing (table + rapid bounds), transport fields, retries
(rapid model of `retryTransport`), AWS token format/AssumeRole/retry against a fake STS, GCP
against a fake token endpoint with a generated service-account key, Azure SPN and workload
identity against a mock Entra tenant (TLS), registry lookups/events with fake clientsets, a
rapid model-based test of the registry core, and envtest tests: registry over the real CRD,
status writes, version label, impersonation against real RBAC, bootstrap objects/idempotency/
rotation/uninstall (token controller emulated by the test, as envtest has none), token
timeout. Cloud endpoints are fakes (`internal/clusters/internal/fakecloud`).
