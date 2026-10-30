# V2 SDK Integration into the rosa CLI

**Last Updated Date**: 2026-07-28

## Summary

This document explores how the `rosa` CLI can support **both** the v1 SDK
(`ocm-sdk-go`, against the OCM API) and the v2 SDK (`clientset/`, against
the Seaplane Platform API) side by side, with v1 remaining the default and v2
selected per-invocation via a flag. It is a companion to
[v2-sdk-initiative.md](v2-sdk-initiative.md), which covers the SDK itself; this
doc covers the **consumer integration** (Story 3 of that initiative).

Two findings from the rosa codebase drive the design:

1. The v1 SDK's generated types (`cmv1.Cluster` and friends) are the CLI's
   de-facto internal domain model, referenced by **~151 non-test files** — the
   SDK cannot simply be swapped underneath the CLI.
2. The HCP lifecycle commands call the OCM API far beyond CRUD — versions,
   regions, machine types, billing accounts, quota, subscriptions. The
   Platform API's _initial_ surface (Cluster + NodePool) backs almost none of
   these, and even as it reaches feature parity over time, those features
   return **re-based on AWS-account tenancy** instead of the OCM org. The v2
   flow is therefore a genuinely different flow — initially smaller, and
   permanently forked wherever identity is consulted — not the same flow with
   a swapped transport.

The recommended approach is **command-level routing**: each HCP lifecycle
command dispatches at the top of its `run` function to either the existing
v1 path (untouched) or a v2-native path that talks `v1alpha1` types directly to
the v2 SDK. Output formatting is kept identical by reusing the existing
`cmv1`-based printers through a small, **display-only `v1alpha1 → cmv1`
mapper**.

## Current rosa CLI Architecture (relevant to integration)

### 1. Clean input seam, leaky output seam

Commands build rosa's own domain struct — `ocm.Spec` (`pkg/ocm/clusters.go:64`),
**not** an SDK type — and hand it to the OCM client:

```go
// cmd/create/cluster/cmd.go:3480
clusterConfig := ocm.Spec{ ... }
// cmd/create/cluster/cmd.go:3672
cluster, err := r.OCMClient.CreateCluster(clusterConfig)   // returns *cmv1.Cluster
```

The `Spec → cmv1 → HTTP` translation is hidden inside `pkg/ocm`
(`createClusterSpec`). On the way out, however, the return type is
`*cmv1.Cluster`, and that type is consumed directly throughout the CLI:

- **~151 non-test files** across `cmd/` and `pkg/` reference
  `github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1` types directly
  (describe, output formatting, validation, machinepool, ingress, idp,
  autoscaler, …).
- `pkg/rosa.Runtime` itself embeds `Cluster *cmv1.Cluster`, and
  `Runtime.FetchCluster()` returns `*cmv1.Cluster` (`pkg/rosa/runtime.go`).

`cmv1.Cluster` is not just an SDK response type — it is the CLI's internal
cluster model.

### 2. Lifecycle commands depend on OCM far beyond CRUD

Measured by distinct `OCMClient` method calls:

| Code path                     | Distinct OCM calls | Examples beyond CRUD                                                                                                                                                                       |
| ----------------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `cmd/create/cluster/cmd.go`   | ~16                | `GetRegionList`, `ValidateVersion`, `GetAvailableMachineTypesInRegion`, `GetBillingAccounts`, `GetCredRequests`, `EnsureNoPendingClusters` (quota), `IsTechnologyPreview`, `GetOidcConfig` |
| `cmd/describe/cluster/cmd.go` | ~11                | `GetSubscriptionBySubscriptionID`, `GetLimitedSupportReasons`, `GetInflightChecks`, `FetchClusterMigrations`, `GetScheduledUpgrade`                                                        |
| `pkg/machinepool`             | ~19                | `GetDefaultClusterFlavors`, `ListKubeletConfigNames`, `GetTuningConfigsName`, `GetClusterAutoscaler`                                                                                       |

The Seaplane Platform API's initial surface is **Cluster + NodePool only**
(see [v2-sdk-initiative.md](v2-sdk-initiative.md)). Today there is nothing to
back versions, regions, machine types, billing, subscriptions, or quota. Any
design premised on "same command code, different backend" hits this wall: those
calls either fail at runtime, require keeping a live OCM connection alongside
Seaplane (defeating the migration), or must be skipped — and "skip this step"
is a change to the command flow, not to the transport.

Most of these features are expected to reach the Platform API eventually —
re-based on AWS-account tenancy rather than the OCM org. That changes the
timeline, not the design conclusion; see
[Does future v2 feature parity change the recommendation?](#does-future-v2-feature-parity-change-the-recommendation)

### 3. The Runtime is the wiring choke point

Commands are wrapped by `rosa.DefaultRunner(RuntimeWithOCM(), runFn)` and reach
the SDK through `r.OCMClient.*` and `r.FetchCluster()` (`pkg/rosa/runner.go`,
`pkg/rosa/runtime.go`). Backend selection and client construction belong here.

### 4. Auth models diverge

|                       | v1 (OCM)                                       | v2 (Seaplane)                                              |
| --------------------- | ---------------------------------------------- | ---------------------------------------------------------- |
| Credential            | OCM SSO tokens (`rosa login`, `config.Load()`) | AWS IAM (SigV4)                                            |
| Endpoint              | Global `api.openshift.com`                     | Per-region Platform API endpoint                           |
| Runtime support today | `WithOCM()`                                    | needs AWS creds (`WithAWS()` exists) + endpoint resolution |

`--seaplane` mode cannot reuse the login config; it needs AWS credentials
(the Runtime already obtains these via `WithAWS()`) plus regional endpoint
resolution. Notably, in v2 mode **no OCM login should be required at all**.

## Recommended Approach: Command-Level Routing + Display-Only Mapper

Dispatch at the top of each HCP lifecycle command, not inside the client:

```bash
rosa create cluster --hosted-cp [--seaplane]
  run()
   ├── default:      existing run path (untouched — all OCM calls intact)
   └── --seaplane: runSeaplane()
                      · flags → v1alpha1.Cluster (struct literal)
                      · v2 SDK client-go-style verbs, natively
                      · no OCM connection; OCM-only steps skipped by design
                      · output via existing printers, fed by a display-only
                        v1alpha1 → cmv1 mapper
```

### Design elements

1. **Persistent `--seaplane` flag** (final name TBD), registered globally.
   Each HCP lifecycle command that supports v2 checks it first thing in `run`
   and dispatches to its v2-native implementation. Commands without a v2 path
   fail fast with a clear error when the flag is set — no silent fallthrough
   to v1.

2. **Same cobra commands, same flag definitions.** Flags are registered once on
   the command, so "preserve all CLI flags" holds structurally. The v2 path
   reads the same flag values. Flags that v2 cannot honor (billing account,
   OCM-specific properties, …) are **rejected with an explicit error**, not
   silently ignored.

3. **v2-native run functions** live alongside the v1 code (e.g.
   `cmd/create/cluster/seaplane.go`, or a shared `pkg/seaplane/`). They
   build `v1alpha1.Cluster` / `v1alpha1.NodePool` struct literals from flags
   and call the v2 SDK's client-go-style verbs directly — exactly the
   consumption model the initiative doc intends for v2 consumers. No
   `ocm.Spec`, no builders.

4. **`Runtime.WithSeaplane()`** constructs the v2 SDK client: AWS SigV4 creds
   via the existing `WithAWS()` machinery, plus regional endpoint resolution
   (see [Endpoint-Resolution Contract](#endpoint-resolution-contract) below).
   It does **not** call `WithOCM()` — v2 mode must work without `rosa login`.

5. **Display-only `v1alpha1 → cmv1` mapper.** The describe/list output code is
   large, `cmv1`-typed rendering logic (`formatClusterHypershift`,
   `clusterInfraConfig`, … in `cmd/describe/cluster/cmd.go`). Rather than
   reimplementing it (output drift risk) or refactoring it (151-file blast
   radius), the v2 path maps `v1alpha1.Cluster → cmv1.Cluster` **only for
   display**, populating only fields the printers read. `cmv1` types are
   constructible via builders (`cmv1.NewCluster().ID(...).Build()`), so this is
   mechanical. Fields with no Seaplane equivalent _yet_ (subscription,
   limited support, billing) render as absent; the mapper grows alongside the
   Platform API surface and the commands wired to it.

### Commands in scope (per acceptance criteria)

| Command                                                   | v2 path                                                                 |
| --------------------------------------------------------- | ----------------------------------------------------------------------- |
| `rosa create cluster --hosted-cp`                         | flags → `v1alpha1.Cluster` → `Clusters().Create()`                      |
| `rosa describe cluster`                                   | `Clusters().Get()` → display mapper → existing printer                  |
| `rosa edit cluster`                                       | `Clusters().Patch()` / `Update()`                                       |
| `rosa delete cluster`                                     | `Clusters().Delete()`                                                   |
| `rosa create/list/describe/edit/delete machinepool` (HCP) | `Clusters().NodePools(...)` verbs                                       |
| `rosa logs`, log forwarders                               | TBD — depends on Platform API logs surface (initiative Open Question 4) |

Everything else (`idp`, `ingress`, `addons`, roles, upgrades, …) has no v2
path and errors fast under `--seaplane`.

## Alternative Considered: Swapped Backend Behind a Narrow Interface

An earlier draft of this document recommended extracting a narrow
`ClusterBackend` interface (cluster + node pool CRUD), selecting the
implementation on the flag, and having the v2 implementation map
`v1alpha1 ↔ cmv1` in both directions so all command code ran unchanged.

**Why it was rejected** — the premise "command code unchanged" is false:

- The lifecycle commands make **dozens of OCM calls beyond CRUD** (see table
  above) that have no Seaplane equivalent. Under `--seaplane` with no OCM
  login these calls cannot succeed. The options are all bad: widen the
  interface to ~40 methods that mostly map to nothing; keep a dual
  OCM+Seaplane connection (defeats the migration and still requires OCM
  SSO); or riddle the shared code with `if seaplane { skip }` branches —
  which is command-level routing, just smeared through the code instead of
  cleanly separated.
- It required a **bidirectional** mapper (`ocm.Spec → v1alpha1` on requests,
  `v1alpha1 → cmv1` on responses) with fidelity high enough for validation
  logic, not just display. The recommended approach keeps only the smaller,
  display-only half.
- Even at feature parity, the flows fork wherever identity is consulted. Most
  v1 features (billing account selection, versions, IdPs, …) are expected to
  return in the Platform API — re-based on **AWS-account tenancy** instead of
  the OCM org (some, like the OCM quota precheck, will not). The _step_
  "select a billing account" exists in both flows, but the data source,
  credentials, and failure modes differ. Shared command code would need
  identity-conditional branches at every such point — which is command-level
  routing, just smeared through shared functions instead of cleanly split.

Two other alternatives were rejected earlier and remain rejected:

- **Refactor the ~151 files onto a neutral domain model** — a big-bang rewrite
  contradicting "v1 remains the default, side by side."
- **OCM-compatible facade on the Platform API** (server-side shim so the v1 SDK
  talks to Seaplane) — explicitly dropped from the initiative.

## Risks and Mitigations

| Risk                                                                                               | Mitigation                                                                                                                                                              |
| -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Output drift between v1 and v2 paths                                                               | Reuse v1 printers via the display mapper; acceptance e2e tests (`hcp_cluster_test.go`, `hcp_machine_pool_test.go`) are the contract                                     |
| Duplicated command-flow logic                                                                      | The duplicated part is precisely what _must_ differ (validation backed by endpoints that don't exist in v2); keep v2 run functions thin and share flag parsing/printing |
| Interactive mode in v2 (`--interactive` prompts source region/version/machine-type lists from OCM) | Phase 1: disable interactive under `--seaplane`; later source lists from AWS APIs or Platform API when available                                                        |
| Flag surface ambiguity (~300 create-cluster flags, many meaningless in v2)                         | Explicit allowlist per v2 command; anything else errors with "not supported with --seaplane"                                                                            |
| Display mapper completeness                                                                        | Populate only fields the printers read for the acceptance-test commands; grow test-by-test                                                                              |

## Relationship to the "migrate directly to client-go" vision

This approach _is_ the direct migration, scoped: the v2 run functions consume
the v2 SDK natively (`v1alpha1` struct literals + verb methods), exactly as the
initiative doc intends. Only the presentation layer borrows v1's printers via
the display mapper — a bounded concession to "identical output formats" that
can be retired if/when the v1 path is removed. The v2-native command logic is
also essentially what `rosactl` needs, so it is a candidate for sharing between
the two CLIs (see Open Question 3).

## Endpoint-Resolution Contract

`Runtime.WithSeaplane()` must resolve the regional Platform API endpoint and
AWS signing region deterministically before constructing the v2 SDK client.
The contract below defines the precedence, region derivation, error behavior,
and mismatch validation that Story 3 implementations and tests must follow.

### Precedence (highest wins)

| Priority | Source                       | What it sets                                                                                                                                  |
| -------- | ---------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| 1        | `--seaplane-url <url>` flag  | Explicit endpoint URL; skips all other endpoint resolution                                                                                    |
| 2        | Built-in region→endpoint map | Endpoint looked up by resolved region (see below)                                                                                             |
| 3        | Discovery (future)           | Reserved for a future service-discovery mechanism; not implemented in the initial version — if precedence 1 and 2 both miss, resolution fails |

### Region derivation (highest wins)

| Priority | Source                                                                 |
| -------- | ---------------------------------------------------------------------- |
| 1        | `--region` flag (explicit)                                             |
| 2        | `AWS_DEFAULT_REGION` environment variable                              |
| 3        | AWS SDK default credential chain region (e.g. `~/.aws/config` profile) |

If none of these sources yield a region, `WithSeaplane()` must return a
clear error: `"region is required for --seaplane mode: set --region, AWS_DEFAULT_REGION, or configure a region in your AWS profile"`.

### Error behavior

| Condition                                                                                        | Behavior                                                                                                    |
| ------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------- |
| No region resolvable                                                                             | Fatal error before SDK client construction (see message above)                                              |
| Region resolved but not present in the built-in region→endpoint map and `--seaplane-url` not set | Fatal error: `"no Platform API endpoint known for region %s; use --seaplane-url to specify one explicitly"` |
| `--seaplane-url` set but no region resolvable                                                    | Fatal error: region is still required for SigV4 signing even when the endpoint is explicit                  |

### Region/endpoint mismatch validation

When `--seaplane-url` is provided **and** a region is resolved, `WithSeaplane()`
must extract the region segment from the endpoint URL (e.g. `us-east-1` from
`https://seaplane.us-east-1.api.example.com`) and compare it to the resolved
region. On mismatch, emit a warning: `"warning: resolved region %s does not match region in --seaplane-url %s; SigV4 will sign with %s"`. The full URL is safe to log — Platform API endpoints are public. This is a
warning, not a fatal error, because custom endpoints (dev, staging) may
intentionally differ.

### Contract summary for tests

Acceptance tests for `WithSeaplane()` must cover:

1. `--seaplane-url` takes precedence over the region→endpoint map
2. Region derived from `--region` takes precedence over `AWS_DEFAULT_REGION`, which takes precedence over the AWS profile
3. Missing region produces the expected fatal error
4. Region present but unmapped (no `--seaplane-url`) produces the expected fatal error
5. Explicit endpoint with no resolvable region produces the expected fatal error
6. Region/endpoint mismatch emits a warning but proceeds
7. The resolved region is used as the SigV4 signing region and the resolved endpoint is used as the SDK base URL

## Open Questions

1. ~~Endpoint resolution in v2 mode~~ — resolved; see [Endpoint-Resolution Contract](#endpoint-resolution-contract) above.
2. ~~Which region?~~ — resolved; see [Region derivation](#region-derivation-highest-wins) above.
3. **rosa vs. rosactl** — retrofit `rosa` (this doc) or treat greenfield
   `rosactl` as the real v2 home, sharing the v2-native command logic? The
   command-level-routing approach keeps this optionality open.
4. **Type divergence** — how far do `v1alpha1.Cluster` / `NodePool` diverge
   from the `cmv1` fields the printers read? This sizes the display mapper
   (see also Open Question 3 in the initiative doc).
5. **Waiting/status UX** — v1 create supports `--watch`-style polling via OCM. The v2 path uses `WaitUntil` (pseudocode: `WaitUntil(ctx, id string, condition func(*ResourceType) bool, interval, timeout time.Duration) error`), available on each resource client; `ResourceType` is the concrete type for that client (`v1alpha1.Cluster`, `v1alpha1.NodePool`, etc.) and the readiness predicate is resource-specific. The condition function receives the live resource on each poll, or `nil` when the resource is absent (404). For create, the condition checks `Status.Phase == ClusterPhaseReady` (or `NodePoolPhaseReady`); for delete, it returns `true` when the resource is `nil`. The Platform API does not support the Kubernetes watch stream protocol — polling via `WaitUntil` is the only synchronization mechanism.

## Suggested First Steps

1. Add the persistent `--seaplane` flag and `Runtime.WithSeaplane()`
   (AWS creds + endpoint config + v2 SDK client). No command wired yet.
2. Wire `rosa create cluster --hosted-cp --seaplane`: minimal flag allowlist
   → `v1alpha1.Cluster` literal → `Clusters().Create()`. Print via a first-cut
   display mapper.
3. Wire `describe` / `delete` cluster, then the machinepool verbs.
4. Run `hcp_cluster_test.go` / `hcp_machine_pool_test.go` against
   `--seaplane`; expand the flag allowlist and display mapper until they
   pass.
