# V2 SDK Initiative: Seaplane Platform API Client SDK

**Last Updated Date**: 2026-07-28

## Summary

Build a **v2 Go SDK** (`clientset/`, a new module in this monorepo) that exposes the same HCP cluster lifecycle operations currently served by `ocm-sdk-go` (v1), but backed by the new Seaplane Platform API instead of the OCM API.

The v2 SDK will be validated by running existing FVT/e2e tests against both the v1 and v2 SDK, proving that consumers (rosa CLI, terraform provider, CAPA) can switch backends without functional regression.

Requires:

- Clone/duplication of API tests pointing both to v1 and v2, and covering the same functionality (the test is AWARE of the SDK v1 vs v2 differences).
- Clone/duplication of Client tests, pointing both to ROSA v1 and v2, and covering the same functionality (the test is NOT AWARE of the SDK v1 vs v2 differences).

**Out of scope for this initiative**: CI/prow integration. Manual test execution is sufficient, but all identified tests must pass.

## Motivation

The Seaplane Platform API (see [api-management.md](api-management.md)) introduces a new REST API surface generated from Seaplane CRD types. Today, consumers like `rosa` CLI and `terraform-provider-rhcs` interact with cluster management through `ocm-sdk-go` against the OCM API (`api.openshift.com`). To complete the Seaplane story, these consumers need a client SDK that talks to the new regional Platform API endpoints.

## V1 SDK Architecture (ocm-sdk-go)

Understanding the v1 SDK is necessary to design the v2 interface. The v1 SDK has six layers:

### Layer Model

```text
┌──────────────────────────────────────────────────────────┐
│  Consumer (rosa CLI, terraform, CAPA)                    │
│  connection.ClustersMgmt().V1().Clusters().Add().Send()  │
├──────────────────────────────────────────────────────────┤
│  Connection (connection.go)                              │
│  Builder pattern, auth config, URL routing               │
│  Provides service accessors: .ClustersMgmt(), etc.       │
├──────────────────────────────────────────────────────────┤
│  Generated Resource Clients (clustersmgmt/v1/)           │
│  ClustersClient → .Add(), .List(), .Cluster(id)          │
│  ClusterClient  → .Get(), .Update(), .Delete(),          │
│                    .NodePools(), .Ingresses(), ...       │
├──────────────────────────────────────────────────────────┤
│  Generated Request/Response types                        │
│  ClustersAddRequest → .Body(cluster).SendContext(ctx)    │
│  Marshals to HTTP, sends, unmarshals response            │
├──────────────────────────────────────────────────────────┤
│  Transport Wrapper Stack                                 │
│  Auth → Retry → Metrics → Logging → Base HTTP            │
├──────────────────────────────────────────────────────────┤
│  Types & Builders (aliased from ocm-api-model/clientapi) │
│  cmv1.NewCluster().Name("x").Region(...).Build()         │
└──────────────────────────────────────────────────────────┘
```

### Generation Pipeline (v1)

The entire SDK (except `connection.go` and the transport wrappers) is **generated** from the OCM API model using a proprietary DSL:

```
ocm-api-model/model (proprietary metamodel DSL)
    ↓ ocm-api-metamodel binary
    ↓ `metamodel generate go --generators=builders-alias,clients,...`
    ↓
Generated output: clients, builders, types, JSON serialization, OpenAPI specs
```

Key facts:

- **Model source**: `github.com/openshift-online/ocm-api-model/model` (proprietary DSL, not OpenAPI)
- **Generator**: `github.com/openshift-online/ocm-api-metamodel/cmd/metamodel`
- **Output**: ~1600 generated files in `clustersmgmt/v1/` alone
- **Precedent**: The `arohcp/` package follows the exact same generated pattern at `v1alpha1`

The v2 SDK intentionally drops this proprietary DSL. Instead, it generates directly from the OpenAPI spec that the Seaplane codegen pipeline already produces (see [V2 SDK Design](#v2-sdk-design)).

### Consumer Usage Pattern

```go
// 1. Build connection (configures auth, retries, metrics)
connection, _ := sdk.NewConnectionBuilder().
    Tokens(token).
    URL("https://api.openshift.com").
    Build()

// 2. Navigate to resource via fluent chain
collection := connection.ClustersMgmt().V1().Clusters()

// 3. Build typed payload
cluster, _ := cmv1.NewCluster().
    Name("my-cluster").
    Region(cmv1.NewCloudRegion().ID("us-east-1")).
    AWS(cmv1.NewAWS().AccountID("123456")).
    Build()

// 4. Create and send request
response, _ := collection.Add().Body(cluster).SendContext(ctx)
created := response.Body()  // typed *Cluster
```

### HCP Cluster Lifecycle Operations in V1

The minimum surface the v2 SDK must cover:

| Operation         | V1 SDK call                                                | HTTP                                         |
| ----------------- | ---------------------------------------------------------- | -------------------------------------------- |
| Create cluster    | `Clusters().Add().Body(c)`                                 | `POST /api/clusters_mgmt/v1/clusters`        |
| Get cluster       | `Clusters().Cluster(id).Get()`                             | `GET /api/clusters_mgmt/v1/clusters/{id}`    |
| List clusters     | `Clusters().List()`                                        | `GET /api/clusters_mgmt/v1/clusters`         |
| Update cluster    | `Clusters().Cluster(id).Update().Body(c)`                  | `PATCH /api/clusters_mgmt/v1/clusters/{id}`  |
| Delete cluster    | `Clusters().Cluster(id).Delete()`                          | `DELETE /api/clusters_mgmt/v1/clusters/{id}` |
| Create node pool  | `Cluster(id).NodePools().Add().Body(np)`                   | `POST .../clusters/{id}/node_pools`          |
| Get node pool     | `Cluster(id).NodePools().NodePool(npId).Get()`             | `GET .../node_pools/{id}`                    |
| List node pools   | `Cluster(id).NodePools().List()`                           | `GET .../clusters/{id}/node_pools`           |
| Update node pool  | `Cluster(id).NodePools().NodePool(npId).Update().Body(np)` | `PATCH .../node_pools/{id}`                  |
| Delete node pool  | `Cluster(id).NodePools().NodePool(npId).Delete()`          | `DELETE .../node_pools/{id}`                 |
| Delete protection | `Cluster(id).DeleteProtection()`                           | `POST .../delete_protection`                 |
| Hibernate         | `Cluster(id).Hibernate()`                                  | `POST .../hibernate`                         |
| Resume            | `Cluster(id).Resume()`                                     | `POST .../resume`                            |

## Seaplane Platform API Surface

The v1 SDK exposes 12 service areas through the OCM API. The Seaplane Platform API replaces a subset of these with a smaller, focused surface.
The v2 SDK only needs to cover the Seaplane equivalents.

| V1 OCM Service            | V1 Path                    | V2 Seaplane Equivalent                                                             | Phase     |
| ------------------------- | -------------------------- | ---------------------------------------------------------------------------------- | --------- |
| ClustersMgmt (clusters)   | `/api/clusters_mgmt`       | **Cluster** CRD                                                                    | Immediate |
| ClustersMgmt (node pools) | `/api/clusters_mgmt`       | **NodePool** CRD                                                                   | Immediate |
| AccountsMgmt              | `/api/accounts_mgmt`       | Authz: accounts, policies, attachments (see [authz-design.md](../authz-design.md)) | Future    |
| Authorizations            | `/api/authorizations`      | Authz: check endpoint (see [authz-design.md](../authz-design.md))                  | Future    |
| AccessTransparency        | `/api/access_transparency` | TBD — likely needed - SRE flows                                                    | Future    |
| ServiceLogs               | `/api/service_logs`        | Per-cluster logs — mechanism TBD                                                   | Future    |

The v2 SDK architecture must accommodate adding new resource types (authz, logs, etc.) as the Platform API grows, but the initial implementation covers only Cluster and NodePool.

## V2 SDK Design

### Approach: Kubernetes-Native Generated SDK

The v1 SDK is generated from a proprietary metamodel DSL (`ocm-api-model`). The v2 SDK drops this DSL entirely and generates directly from the CRD type definitions using standard Kubernetes tooling.

The v2 SDK exposes its generated interface directly — there is no v1-compatibility adapter. Consumers migrate to the new interface (see [Interface Decision](#interface-decision)).

**Generated core**: Auto-generated from the Seaplane CRD types (`api/v1alpha1/`) using `client-gen` (from `k8s.io/code-generator`), the same tool that generates typed clientsets for any Kubernetes operator. This produces typed verb clients (`Create`, `Get`, `List`, `Update`, `Delete`) that match the CRD types exactly. A custom `bridge-gen` tool extends the generated clients with platform-specific behavior (watch suppression, `WaitUntil` polling).

### SDK Release Cadence and Strategy

In v1, the SDK was released separately from the backend. The v2 api will support backward compatibility after the GA release.

```
┌────────────────────────────────────────────┐
│  Consumer (rosa CLI, terraform)            │
├────────────────────────────────────────────┤
│  Generated Core (from CRD types)           │
│  Typed clients, models (client-gen)        │
│  Platform wrappers (bridge-gen)              │
├────────────────────────────────────────────┤
│  Connection / Auth / Transport             │
│  AWS SigV4 auth, retry, logging            │
└────────────────────────────────────────────┘
```

### Generation from CRD Types

The v2 SDK generation pipeline runs from CRD type definitions directly:

```text
Go types (api/v1alpha1/*.go)
    ↓ client-gen (k8s.io/code-generator)
    ↓ Typed clientset: Create/Get/List/Update/Delete per resource
    ↓
    ↓ bridge-gen --mode=bridge   → wire field name → metadata field name mappings
    ↓ bridge-gen --mode=platform   → Watch override (ErrWatchNotSupported)
                                   WaitUntil polling helper
```

Markers in the CRD type comments drive `bridge-gen` output:

- `+bridge:field=<wire>,meta=<meta>` — field name mapping (transport layer)
- `+bridge:watch=disabled` — suppress Watch; generate an override returning `ErrWatchNotSupported`
- `+bridge:wait` — generate `WaitUntil(ctx, id, condition func(*T) bool, interval, timeout)`

The entire pipeline runs as `make generate-clientset`.

### Dynamic Generation

Goal: the v2 SDK generation is fully automated as part of the `make generate-clientset` target. When a developer adds or modifies a field in the Go types, runs `make generate-clientset`, and gets an updated clientset and wrappers in one pass. This is achievable because the entire chain derives from CRD-type annotations — no manual model definitions to maintain.

### Interface Decision

The v2 SDK adopts a **Kubernetes-style interface**, modeled on `client-go`. Consumers (rosa CLI, terraform provider) migrate to it directly; a migration guide + helper functions assist in switching. This is more work upfront than a drop-in v1-compatible layer, but cleaner long-term.

This shape is a natural fit: the Seaplane resources are already CRD-backed types (`ObjectMeta` / `Spec` / `Status`), so the SDK simply exposes them the same way you'd work with them in Kubernetes. There are no fluent builders to generate or maintain — callers construct a typed struct literal and pass it to a verb method on a typed client.

**Resource construction** — a plain struct literal, not a builder chain:

```go
cluster := &v1alpha1.Cluster{
    ObjectMeta: metav1.ObjectMeta{
        Name:   "my-cluster",
        Labels: map[string]string{"env": "dev"},
    },
    Spec: v1alpha1.ClusterSpec{
        Region:  "us-east-1",
        Version: "4.19.0",
        AWS:     v1alpha1.AWSSpec{AccountID: "123456789012"},
    },
}
```

**Client verbs** — resource-scoped accessors exposing `Create` / `Get` / `List` / `Update` / `Patch` / `Delete`, each taking a `context.Context` and typed options, exactly like a `client-go` clientset:

```go
created, err := client.Clusters().Create(ctx, cluster, metav1.CreateOptions{})

got, err := client.Clusters().Get(ctx, "my-cluster", metav1.GetOptions{})

list, err := client.Clusters().List(ctx, metav1.ListOptions{})

np := &v1alpha1.NodePool{ /* ObjectMeta + Spec */ }
_, err = client.Clusters().NodePools("my-cluster").Create(ctx, np, metav1.CreateOptions{})
```

Contrast with the v1 fluent chain (`cmv1.NewCluster().Name("x").Region(...).Build()` then `Clusters().Add().Body(c).SendContext(ctx)`): v2 replaces the builder + send pattern with typed struct literals and verb methods.

Because the generated core already produces the typed `Spec`/`Status` models from OpenAPI, this interface is a thin, mechanical layer over them rather than a hand-maintained adapter.

## Scope

### Target Clients

| Client                                         | Priority | Rationale                                                              |
| ---------------------------------------------- | -------- | ---------------------------------------------------------------------- |
| **rosa CLI** (`rosa-seaplane-cli` / `rosactl`) | P0       | Primary user-facing tool, exercises full lifecycle                     |
| **terraform-provider-rhcs**                    | P1       | Key IaC consumer, many enterprise customers depend on it               |
| **CAPA** (Cluster API Provider AWS)            | P2       | Lower priority for initial initiative; evaluate after rosa + terraform |

### In Scope

1. V2 SDK generated from OpenAPI with authentication (AWS SigV4) and basic HCP cluster + node pool lifecycle
2. Identify and catalog specific FVT/e2e tests that exercise cluster lifecycle through v1 SDK
3. Design the v2 SDK interface
4. Hook v2 SDK into rosa CLI and terraform provider
5. Run identified tests against v2 SDK and verify they pass
6. Evaluate and prototype dynamic generation from OpenAPI (integrated into `make generate`)

### Out of Scope

- **Tenancy and authorization** — account linking, policy CRUD, attachment CRUD, and the authorization check surface (see [authz-design.md](../authz-design.md)) are deferred to a future iteration. This first iteration covers authentication (AWS SigV4) only, not authz.
- CI/prow integration (manual testing is sufficient)
- Non-HCP cluster types (classic ROSA)
- Full OCM API surface coverage (only cluster + node pool lifecycle)
- Add-ons, ingresses, identity providers, and other sub-resources beyond cluster/nodepool (future iterations)
- Access transparency, service logs, service management (future phases)

## Test Identification

### Rosa CLI E2E Tests (rosa repo)

These tests in `rosa/tests/e2e/` exercise HCP cluster lifecycle through the CLI, which uses `ocm-sdk-go` under the hood:

| Test file                   | What it exercises                                       | Labels                            |
| --------------------------- | ------------------------------------------------------- | --------------------------------- |
| `hcp_cluster_test.go`       | HCP cluster create/describe/edit/delete, log forwarders | `Feature.Cluster`, `Runtime.Day2` |
| `hcp_machine_pool_test.go`  | HCP node pool (machine pool) CRUD                       | `Feature.MachinePool`             |
| `test_rosacli_idp.go`       | External auth configuration                             |                                   |
| `hcp_tuning_config_test.go` | Tuning configs on HCP clusters                          | `Feature.TuningConfig`            |
| `e2e_setup_test.go`         | Cluster provisioning (precondition)                     | setup                             |
| `e2e_tear_down_test.go`     | Cluster deletion (cleanup)                              | cleanup                           |

### Seaplane CLI E2E Tests (this repo)

These tests in `test/e2e-cli/` exercise the full lifecycle through `rosactl`:

| Test area      | Labels                                                      | What it exercises            |
| -------------- | ----------------------------------------------------------- | ---------------------------- |
| VPC setup      | `vpc-create`, `vpc-list`                                    | AWS VPC creation for cluster |
| IAM setup      | `iam-create`, `iam-list`                                    | IAM roles and OIDC           |
| Cluster create | `hcp-create`                                                | `rosactl cluster create`     |
| Cluster status | `cluster-status`                                            | Poll until cluster is ready  |
| Node pools     | `nodepools-wait`                                            | Node pool readiness          |
| Cluster update | `hcp-patch`                                                 | `rosactl cluster patch`      |
| Cleanup        | `bundles-delete`, `oidc-delete`, `iam-delete`, `vpc-delete` | Full teardown                |

### Seaplane Platform API E2E Tests (this repo)

The `test/e2e-api/` tests exercise the Platform API directly:

| Test file     | What it exercises      |
| ------------- | ---------------------- |
| `e2e_test.go` | Basic API connectivity |

### Acceptance Criteria for V2 SDK

Each test suite below must pass against **both** the v1 SDK (ocm-sdk-go, baseline) and the v2 SDK (`clientset/`). The v1 run establishes the expected baseline; the v2 run proves parity. The initiative is not complete until both columns are green and behavioral parity is validated.

| #   | Test suite                                                                                                                                                                         | v1 SDK (baseline)                                                           | v2 SDK                                                                            |
| --- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| 1   | **Rosa CLI HCP tests** (`hcp_cluster_test.go`, `hcp_machine_pool_test.go`): create HCP cluster, CRUD node pools, delete cluster                                                    | Must pass against ocm-sdk-go to establish baseline behavior                 | Must pass against `clientset/` with identical observable outcomes                 |
| 2   | **Seaplane CLI lifecycle** (`test/e2e-cli/cluster_test.go` labels: `setup`, `create`, `monitor`, `cleanup`): full VPC → IAM → cluster → node pool → teardown cycle through rosactl | Must pass against ocm-sdk-go to establish baseline behavior                 | Must pass against `clientset/` with identical observable outcomes                 |
| 3   | **Terraform basic lifecycle**: `terraform apply` + `terraform destroy` of an HCP cluster with node pools                                                                           | Must pass with provider backed by ocm-sdk-go to establish baseline behavior | Must pass with provider backed by `clientset/` with identical observable outcomes |

**Behavioral-parity validation**: For each test suite, the v1 and v2 runs must produce the same observable outcomes — identical resource states, API response codes, and CLI/Terraform output (excluding endpoint URLs and auth-mechanism differences). Any behavioral divergence must be documented and justified before the initiative is declared complete.

## Work Breakdown

### Epic: V2 SDK — Seaplane Platform API Client

#### Story 1: V2 SDK Skeleton and Authentication

Set up the `clientset/` module with:

- Connection builder with AWS SigV4 authentication (the Seaplane API uses IAM auth, not OCM SSO tokens)
- Regional Platform API endpoint (required; each region has its own endpoint, e.g. `https://seaplane.us-east-1.api.example.com`)
- AWS signing region (required; must match the region of the Platform API endpoint, e.g. `us-east-1`)
- AWS signing service set to `execute-api` (the Platform API is fronted by API Gateway)
- `X-Amz-Account-Id` header (required on every request; the AWS account ID of the calling principal)
- `X-Amz-Caller-Arn` header (optional; the ARN of the IAM role or user making the request, passed when available for audit/authorization)
- Transport wrapper stack (SigV4 signing, retry, logging)
- Basic HTTP round-tripper that talks to the Seaplane Platform API

**Acceptance**:

1. SDK requires a regional Platform API endpoint and rejects initialization when it is missing
2. SDK requires a signing region and rejects initialization when it is missing
3. SigV4 signatures use `execute-api` as the signing service name
4. Every signed request includes the `X-Amz-Account-Id` header with the caller's AWS account ID
5. When a caller ARN is configured, signed requests include the `X-Amz-Caller-Arn` header
6. Acceptance tests capture the raw HTTP request and verify that the `Authorization` header contains `execute-api` as the service, and that `X-Amz-Account-Id` (and optionally `X-Amz-Caller-Arn`) appear as signed headers — not merely that AWS credentials produce a valid signature

#### Story 2: Generated Core from CRD Types

Set up the generation pipeline:

- Use `client-gen` to generate typed clientsets from `api/v1alpha1/` CRD types
- Use `bridge-gen --mode=bridge` to generate wire↔metadata field name mappings from `+bridge:field` markers
- Use `bridge-gen --mode=platform` to generate Watch overrides and `WaitUntil` polling helpers from `+bridge:watch=disabled` / `+bridge:wait` markers
- Wire the generated client into the SDK's transport layer (`clientset/transport`)
- Expose the wrapped clientset through `clientset/seaplane.go`

**Acceptance**: Generated types exist for Cluster and NodePool resources. Can create a cluster via the generated client. Watch returns `ErrWatchNotSupported`. `WaitUntil` polls until a caller-supplied condition is met or the timeout elapses (condition receives `nil` on 404 — caller decides whether absence is terminal).

#### Story 3: Integrate V2 SDK into Rosa CLI

The rosa CLI supports **both** SDKs side by side — v1 (`ocm-sdk-go`) remains the default; the v2 SDK is selected per-invocation via a flag (e.g. `--seaplane` or `--v2`; exact name TBD).

- Fork or branch the rosa CLI (or rosactl)
- Add the v2 SDK as an additional backend for cluster/node pool operations, alongside the existing `ocm-sdk-go` path (do not remove v1)
- Route to v1 or v2 based on the backend-selection flag
- Preserve all CLI flags and output formats identically across both backends

**Acceptance**: `rosa create cluster --hosted-cp --seaplane` (or `rosactl cluster create --seaplane`) works against the Seaplane Platform API via the v2 SDK, while the same command without the flag continues to use the v1 SDK unchanged.

#### Story 4: Integrate V2 SDK into Terraform Provider

- Fork or branch `terraform-provider-rhcs`
- Replace cluster/node pool resource implementations to use v2 SDK
- Preserve terraform state compatibility

**Acceptance**: `terraform apply` of an HCP cluster + node pool plan works against Seaplane Platform API.

#### Story 5: FVT Test Execution and Validation

- Run identified rosa CLI HCP e2e tests against v2 SDK-backed CLI
- Run Seaplane CLI lifecycle tests
- Run terraform lifecycle test
- Document results and any behavior differences

**Acceptance**: All tests in the "Acceptance Criteria" section pass.

#### Story 6: Evaluate Dynamic SDK Generation Pipeline

- Prototype end-to-end flow: Go types → markers → OpenAPI spec → SDK client code
- Evaluate whether `make generate` can produce the v2 SDK alongside CRDs and OpenAPI
- Document gaps and requirements for full automation

**Acceptance**: Document with feasibility assessment, prototype code, and recommendation for full automation.

## Decisions Made

1. **Generation approach**: CRD-types-first. Drop the proprietary OCM metamodel DSL. Generate the v2 SDK directly from the CRD type definitions using `client-gen` (standard Kubernetes tooling) plus a custom `bridge-gen` for platform-specific extensions (watch suppression, `WaitUntil` polling). This avoids the OpenAPI intermediary step and keeps generation aligned with the operator's type definitions as the single source of truth.
2. **Auth model**: AWS SigV4 (IAM auth), not OCM SSO tokens. The Seaplane API authenticates all requests via AWS IAM credentials.
3. **Initial surface**: Cluster + NodePool only. Tenancy and authz (account linking, policies, attachments, authorization check) are deferred to a future iteration, along with access transparency, service logs, etc.
4. **Interface style**: Kubernetes-style, modeled on `client-go` — typed resource structs (`ObjectMeta`/`Spec`/`Status`) constructed as struct literals, and a typed client exposing `Create`/`Get`/`List`/`Update`/`Patch`/`Delete` verbs. No fluent builders.

## Typed vs. Dynamic Client

This initiative deliberately builds a **typed** SDK (generated Go structs, verb
methods — see [Interface Decision](#interface-decision)), not a **dynamic** one
(a generic client that fetches the schema at runtime and operates on
`unstructured` / `map[string]any`, the way `kubectl` does).

The rationale: a typed client suits consumers whose _own code names individual
fields_. `rosa` CLI (each flag maps to a field), terraform-provider-rhcs (each
HCL attribute maps to a field), and CAPA (reconcile logic maps field to field)
are all in this category — they hand-write field names in source, so compile-time
type safety is a direct benefit and the "new field ⇒ regenerate" cost is
negligible (they must add a flag/attribute/mapping to use the field anyway).

A dynamic client only pays off for pass-through consumers that forward a whole
user-supplied resource without naming its fields (e.g. a future
`rosactl apply -f cluster.yaml`), where new fields flow through with no client
release. If/when `rosactl` grows such a surface, a dynamic client may be picked
for it (TBD). It is out of scope here.

(Note: distinct from the build-time [Dynamic Generation](#dynamic-generation)
above, which is about automating codegen in `make generate` — not a runtime
dynamic client.)

## Open Questions

1. ~~**Module location**~~: Resolved — the SDK lives in this monorepo as `clientset/` with its own `go.mod`, following the same pattern as `seaplane-api/`, `seaplane-operator/`, and `seaplane-db/`.
2. **CAPA timing**: Should CAPA integration be part of this initiative or deferred?
3. **Type compatibility**: How much do the Seaplane CRD types (Cluster, NodePool) diverge from the OCM API model types? The migration effort for consumers depends on this.
4. **Service logs**: Logs appear to be per-cluster in Seaplane. How will the SDK expose them — as a sub-resource of Cluster, or as a separate top-level surface?
