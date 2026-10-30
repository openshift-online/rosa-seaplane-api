# Client Pathbinding

**Last Updated**: 2026-09-02

## Summary

`pathbind` is a reflection-based mapping layer that translates between flat consumer structs and the deeply-nested SDK types (`HostedClusterSpecPassthrough`, `NodePoolSpecPassthrough`). It eliminates per-field manual struct construction in consumers and provides a generation pipeline so new SDK fields propagate to CLI flags and Terraform schema with minimal manual effort.

---

## Problem

Both `rosa` CLI and `terraform-provider-rhcs` hand-write the translation between their own representation (cobra flags, Terraform state attributes) and the SDK's deeply-nested types. Adding a new field requires finding every construction and population site in every consumer, writing assignment code, and updating tests — independently, for each consumer.

---

## Architecture

```text
field_metadata.json  (from marker-scanner + CRD type annotations)
      │
      ▼
pathbind-draft.yaml  (generated, clientset/pathbind/ — SDK repo)
      │                leaf paths resolved via OpenAPI walking
      │
      │   +  pathbind-overrides.yaml  (consumer-maintained UX decisions)
      │
      ▼
pathbind-gen --mode=cobra
      │
      ▼
<consumer>/pkg/seaplane/pathbind/          ← local package in consumer repo
      ├── cluster_create_gen.go    ← ClusterCreateInput, RegisterClusterCreateFlags,
      │                               ClusterCreateHandler, RunCreateCluster, GeneratedClusterCreatePrompt
      ├── cluster_update_gen.go    ← ClusterUpdateInput, ClusterUpdateHandler, RunUpdateCluster
      ├── nodepool_create_gen.go   ← same pattern for NodePool create
      └── nodepool_update_gen.go   ← NodePoolUpdateInput, NodePoolUpdateHandler, RunUpdateNodePool

pathbind.go  (clientset/pathbind/ — SDK repo)
      ├── Expand(ctx, src, dst)  — consumer struct → SDK struct
      └── Flatten(ctx, src, dst) — SDK struct → consumer struct
```

---

## pathbind.go — Core Engine

Located at `clientset/pathbind/pathbind.go`.

### `Expand(ctx, src, dst)`

Populates `dst` (a non-nil pointer to an SDK struct) from the `hfsdk:`-tagged fields of `src` (a consumer struct or pointer to struct).

For each field in `src` with an `hfsdk:"<dotted.json.path>"` tag:

- Splits the path by `.`, traverses `dst` by JSON tag name
- Allocates nil intermediate pointer-to-struct fields automatically
- Sets the leaf value via `toFieldValue` (see type conversion below)

**Fields skipped (not forwarded)**:

- Tagged `hfsdk:"-"`
- Nil pointer
- Empty string (`""`)
- Zero integer (`0`) — distinguishes "flag not passed" from "intentional zero"
- Empty slice or nil map

> **Why zero integers are skipped**: cobra flag registration for `*int32` fields always calls `new(int32)`, leaving the pointer non-nil even when the flag is never passed. Treating zero as "not set" prevents `port: 0` and similar zero values from being silently forwarded to the SDK.

`bool` is the sole exception: `false` IS forwarded (it is a meaningful value, not a zero sentinel).

### `Flatten(ctx, src, dst)`

Reads `hfsdk:`-tagged fields of `dst` (consumer struct pointer) from `src` (SDK struct).

- Traverses the path in `src`; nil intermediate pointers are skipped (not an error)
- Converts leaf values back to consumer types (named string types → `string`, `metav1.Time` → RFC3339 string, slices/maps → JSON string)

### Type conversion — `toFieldValue`

Handles conversion between consumer field types and SDK leaf types:

| Source → Target                                    | Mechanism                        |
| -------------------------------------------------- | -------------------------------- |
| `string` → named string type (e.g. `PlatformType`) | `ConvertibleTo`                  |
| `int64` → `int32`, etc.                            | `ConvertibleTo`                  |
| RFC3339 `string` → `metav1.Time`                   | explicit parse                   |
| `string` → `[]T` / `map[K]V` / struct              | JSON unmarshal                   |
| `[]T` / `map[K]V` / struct → `string`              | JSON marshal                     |
| any → `*T`                                         | recurse on elem, wrap in pointer |

The JSON round-trip fallback enables consumer string flags to hold JSON-encoded complex types (e.g. `--resource-tags '[{"key":"env","value":"prod"}]'`). Unmarshal errors surface as field-level errors in `Expand`.

### Consumer struct tagging

```go
type MyInput struct {
    Name         string `hfsdk:"metadata.name"`
    Version      string `hfsdk:"spec.hostedCluster.release.image"`
    VPC          string `hfsdk:"spec.hostedCluster.platform.aws.cloudProviderConfig.vpc"`
    Replicas     *int32 `hfsdk:"spec.nodePool.replicas"`         // nil = not set
    DeleteProt   *bool  `hfsdk:"spec.deleteProtection"`          // false is meaningful
    ResourceTags string `hfsdk:"spec.hostedCluster.platform.aws.resourceTags"` // JSON array
    Derived      string `hfsdk:"-"` // skipped; set manually after Expand
}
```

---

## Configuration — Two-file model

Alias configuration is split across two files with distinct ownership:

### `clientset/pathbind/pathbind-draft.yaml` — SDK-generated

**Location**: SDK repo (`rosa-seaplane-api`), committed alongside `pathbind.go`.  
**Owned by**: the generation pipeline — never hand-edited.  
**Generated by**: `pathbind-gen --mode=init` reading `field_metadata.json` + `openapi.yaml`.  
**Content**: **leaf paths** resolved by walking the OpenAPI schema from each FieldRegistry entry down to scalar leaves, plus the operations and resolved Go type for each leaf.

```yaml
# clientset/pathbind/pathbind-draft.yaml — GENERATED. DO NOT EDIT.
resources:
  cluster:
    sdkType: v1alpha1.Cluster
    fields:
      - path: spec.properties
        goType: map
        operations: [create, update]
      - path: spec.deleteProtection
        goType: boolean
        operations: [create, update]
      - path: spec.hostedCluster.release.image
        goType: string
        operations: [create, update]
      - path: spec.hostedCluster.platform.aws.region
        goType: string
        operations: [create, update]
      - path: spec.hostedCluster.platform.aws.resourceTags
        goType: array(string)
        operations: [create, update]
      - path: spec.hostedCluster.networking.apiServer.port
        goType: integer(int32)
        operations: [create, update]
      # service-set and hidden fields are omitted
```

Every draft path automatically generates a struct field and cobra flag. Consumer overrides add UX metadata (flag name, description, required) on top; they do not select which sub-fields to expose.

Regenerated automatically whenever `make generate-pathbind-draft` runs. New fields appear here as soon as they are annotated with `+seaplane:write-mode` in the CRD types and the OpenAPI spec is regenerated.

---

### `pathbind-overrides.yaml` — consumer-maintained

**Location**: each consumer repo (e.g. `ocm/rosa/pkg/seaplane/`).  
**Owned by**: the consumer team — hand-maintained UX decisions.  
**Content**: flag metadata matched to draft leaf paths by `path`. Entries are optional — all draft paths appear in the generated struct automatically with auto-derived flag names; overrides only refine the defaults.

```yaml
# pkg/seaplane/pathbind-overrides.yaml
resources:
  cluster:
    aliases:
      - path: metadata.name
        alias: name
        flag: cluster-name
        description: "Unique DNS-label name of the cluster. Maximum length 63 characters."
        required: true
      - path: spec.hostedCluster.release.image
        alias: version
        flag: version
        description: "OpenShift release image for the cluster."
        required: true
      - path: spec.properties
        flag: properties
        description: "Customer-defined Cluster properties."
      - path: spec.deleteProtection
        flag: delete-protection
        description: "Enable delete protection."
       - path: spec.hostedCluster.platform.aws.cloudProviderConfig.subnet.id
         alias: subnetID
        flag: subnet-id
        description: "Subnet ID for the cluster."
         required: true
       # Terraform-only grouping of leaf fields into a nested object:
       - path: spec.hostedCluster.networking.machineNetwork
         bundle: network
       - path: spec.hostedCluster.networking.serviceNetwork
         bundle: network
      # Consumer-only field not in the draft (hfsdk:"-" in generated struct):
      - alias: operatorRolesPrefix
        type: string
        flag: operator-roles-prefix
        description: "Prefix for all IAM operator role names."
        required: true
        operations: [create]
```

**Key principle**: the generator includes **all non-service-set draft paths** in the generated struct automatically. Override entries add cobra flag metadata; derived fields (`vpc`, `zone`) need no override — `PreRequest` sets them directly on the input struct.

#### Override schema

| Field         | Required                           | Default when absent                                      |
| ------------- | ---------------------------------- | -------------------------------------------------------- |
| `path`        | yes (except consumer-only entries) | —                                                        |
| `alias`       | no                                 | Minimum unique suffix of `path` in camelCase             |
| `type`        | no                                 | Derived from draft `goType` (see below)                  |
| `goName`      | no                                 | PascalCase of `alias`                                    |
| `flag`        | no                                 | `toKebab(alias)`                                         |
| `description` | no                                 | `""`                                                     |
| `required`    | no                                 | `false`                                                  |
| `operations`  | no                                 | Inherited from draft; required for consumer-only entries |
| `bundle`      | no                                 | Leaf remains at the resource root                        |

`bundle` is a consumer layout decision. In `--mode=tf`, aliases with the same
bundle name are emitted as fields of one Terraform `SingleNestedAttribute`
object and as fields of a nested native Go struct. Their original `hfsdk`
paths are unchanged, so `Expand` and `Flatten` still map each leaf to the SDK.
For example:

```yaml
resources:
  cluster:
    aliases:
      - path: spec.hostedCluster.networking.machineNetwork
        bundle: network
      - path: spec.hostedCluster.networking.serviceNetwork
        bundle: network
```

This produces a `network` Terraform object containing `machine_network` and
`service_network`. Bundling is ignored by the Cobra layout, while nested
consumer structs are supported by the shared pathbind engine.

#### `goType` → consumer type defaults

| Draft `goType`   | Consumer default                                                   |
| ---------------- | ------------------------------------------------------------------ |
| `string`         | `string`                                                           |
| `boolean`        | `*bool`                                                            |
| `integer(int32)` | `*int32`                                                           |
| `integer(int64)` | `*int64`                                                           |
| `array(string)`  | `string[]` for supported consumers; Cobra rejects collection flags |
| `array(object)`  | Unsupported automatically; add an explicit consumer representation |
| `map`            | `map` for Terraform; Cobra rejects map fields                      |

**Consumer-only entries** (no `path`): generator emits the field with `hfsdk:"-"` — Expand skips it; consumer sets it in `PostExpand` or `PreRequest`.

---

## pathbind-gen — Code Generator

Located at `clientset/cmd/pathbind-gen/` (part of the `clientset` module so consumers can `go run` it directly from vendor).

### Modes

**`--mode=init`**: reads `field_metadata.json` + `openapi.yaml`, walks each FieldRegistry path to scalar leaves, emits `pathbind-draft.yaml`. Run by the SDK's CI.

**`--mode=cobra`**: reads draft + overrides, emits per-resource, per-operation into the consumer's local `pathbind` package:

1. **Input struct** — `ClusterCreateInput`, etc. with `hfsdk:` tags (all draft fields included)
2. **`RegisterXxxFlags(cmd, input)`** — cobra flag registration
3. **`XxxHandler` interface** — the dispatch template
4. **`GeneratedXxxPrompt` struct** — default interactive prompting (embeddable)
5. **`RunXxx(ctx, r, cmd, input, handler)`** — the dispatch skeleton
6. **`XxxPlatformAPIFlags []string`** — flag name list for help section marking

### `toKebab` — flag name derivation

camelCase/PascalCase → kebab-case, with acronym-aware rules:

| Input                    | Output                      | Rule                                                               |
| ------------------------ | --------------------------- | ------------------------------------------------------------------ |
| `displayName`            | `display-name`              | lower→upper = dash                                                 |
| `issuerURL`              | `issuer-url`                | upper→upper→end = no dash (acronym)                                |
| `kubeCloudControllerARN` | `kube-cloud-controller-arn` | trailing acronym                                                   |
| `AWSPlatform`            | `aws-platform`              | upper→upper→lower = dash before word start                         |
| `allocateNodeCIDRs`      | `allocate-node-cidrs`       | plural `s` after acronym = no dash                                 |
| `allowedCIDRBlocks`      | `allowed-cidr-blocks`       | `s` followed by uppercase = still no dash; next upper→lower = dash |
| `registryPullQPS`        | `registry-pull-qps`         | trailing all-caps acronym                                          |

**Rule summary**: insert a dash before an uppercase letter only at a lower→upper boundary, OR at an upper→upper boundary when the next character is a lowercase letter that is NOT a plural `s` followed by uppercase or end-of-string.

### Invocation

```makefile
# SDK Makefile — regenerate draft (runs in CI)
generate-pathbind-draft:
    pathbind-gen --mode=init \
        --registry=hack/api-codegen/pkg/registry/field_metadata.json \
        --openapi=api/v1alpha1/public/openapi.yaml \
        --output=clientset/pathbind/pathbind-draft.yaml

# Consumer Makefile (e.g. ocm/rosa)
PATHBIND_GEN_PKG := github.com/openshift-online/rosa-seaplane-api/clientset/cmd/pathbind-gen
PATHBIND_DRAFT   := vendor/github.com/openshift-online/rosa-seaplane-api/clientset/pathbind/pathbind-draft.yaml
PATHBIND_OUT     := pkg/seaplane/pathbind   # local package in the consumer repo

generate-seaplane:
    GOFLAGS="" go run $(PATHBIND_GEN_PKG) \
        --mode=cobra \
        --draft=$(PATHBIND_DRAFT) \
        --overrides=pkg/seaplane/pathbind-overrides.yaml \
        --output-dir=$(PATHBIND_OUT)
```

The generated package is **local to the consumer repo** at `pkg/seaplane/pathbind`, not re-vendored from the SDK. Import it as:

```go
hfpathbind "github.com/openshift/rosa/pkg/seaplane/pathbind"
```

---

## Dispatch Interface Pattern

The generator emits a **four-method interface** per resource+operation.

```go
// ClusterCreateHandler — generated; same four-method contract for all resource/operation pairs.
type ClusterCreateHandler interface {
    // Prompt fills missing inputs interactively when interactive.Enabled() is true.
    Prompt(ctx context.Context, r *rosa.Runtime, cmd *cobra.Command, input *ClusterCreateInput) error

    // PreRequest validates inputs and derives computed fields (e.g. subnet → VPC/zone).
    PreRequest(ctx context.Context, r *rosa.Runtime, input *ClusterCreateInput) error

    // PostExpand sets SDK struct fields pathbind cannot express:
    // constants (Platform.Type) and computed structs (RolesRef).
    PostExpand(ctx context.Context, r *rosa.Runtime, input *ClusterCreateInput, obj *v1alpha1.Cluster) error

    // PostResponse handles the SDK response: output formatting, side effects.
    PostResponse(ctx context.Context, r *rosa.Runtime, cluster *v1alpha1.Cluster) error
}

func RunCreateCluster(ctx context.Context, r *rosa.Runtime, cmd *cobra.Command, input *ClusterCreateInput, h ClusterCreateHandler) error {
    if interactive.Enabled() {
        if err := h.Prompt(ctx, r, cmd, input); err != nil { return err }
    }
    if err := h.PreRequest(ctx, r, input); err != nil { return err }
    obj := &v1alpha1.Cluster{}
    if err := pathbind.Expand(ctx, *input, obj); err != nil { return err }
    if err := h.PostExpand(ctx, r, input, obj); err != nil { return err }
    cluster, err := r.SeaplaneClient.SeaplaneV1alpha1().Clusters().Create(ctx, obj, platform.CreateOptions{})
    if err != nil { return err }
    return h.PostResponse(ctx, r, cluster)
}
```

Generated per operation: `ClusterCreateHandler`, `ClusterUpdateHandler`, `NodePoolCreateHandler`, `NodePoolUpdateHandler`.

---

## Flag Isolation

Platform API flags are isolated from OCM v1 flags via two helpers in `pkg/seaplane/flaggroup.go`:

### `RegisterAndMarkPlatformAPIFlags(cmd, register, platformAPIFlags)`

1. Snapshots all pre-existing flag names on `cmd`
2. Calls `register()` (which invokes `Register*Flags`)
3. For each name in `platformAPIFlags`:
   - **New flag** → tagged `seaplane-api-group` (Platform API only)
   - **Pre-existing flag** → tagged `seaplane-api-shared` (used by both OCM v1 and HF)

```go
seaplane.RegisterAndMarkPlatformAPIFlags(cmd,
    func() { hfpathbind.RegisterClusterCreateFlags(cmd, &hfClusterInput) },
    hfpathbind.ClusterCreatePlatformAPIFlags,
)
```

### `AddPlatformAPIFlagSection(cmd)`

Installs a custom `SetHelpFunc` that renders two additional sections before "Global Flags:":

- **"Shared flags (OCM v1 and Platform API)"** — flags used by both paths
- **"Platform API flags"** — HF-only flags

Call once per command, after `RegisterAndMarkPlatformAPIFlags`.

> **`MarkRegionDeprecated` interaction**: `arguments.MarkRegionDeprecated` in the edit parent command overwrites `SetHelpFunc` on each subcommand, orphaning the Platform API wrapper. Fix: call `AddPlatformAPIFlagSection` again in the edit parent's `init()` **after** `MarkRegionDeprecated` for the affected subcommands (`cluster.Cmd`, `machinepoolCommand`). See `cmd/edit/cmd.go`.

### `hfClusterInput` — flag backing store

`hfClusterInput hfpathbind.ClusterCreateInput` is owned entirely by the seaplane path. OCM v1 continues using its own `args` struct.

`PreRequest` handles derivations (subnet → VPC/zone/region) and any fields not yet fully isolated. `PostExpand` sets values pathbind cannot express (enum constants, computed role refs).

---

## Consumer Implementation (rosa CLI example)

```go
import hfpathbind "github.com/openshift/rosa/pkg/seaplane/pathbind"

type seaplaneClusterCreate struct {
    hfpathbind.GeneratedClusterCreatePrompt
    describeSubnets func(context.Context, aws.Config, string) (*ec2svc.DescribeSubnetsOutput, error)
}

func (h *seaplaneClusterCreate) PreRequest(ctx context.Context, r *rosa.Runtime, input *hfpathbind.ClusterCreateInput) error {
    if len(args.subnetIDs) > 0 { input.SubnetID = args.subnetIDs[0] }
    out, err := h.describeSubnets(ctx, r.AWSConfig, input.SubnetID)
    if err != nil { return err }
    input.VPC    = awssdk.ToString(out.Subnets[0].VpcId)
    input.Zone   = awssdk.ToString(out.Subnets[0].AvailabilityZone)
    input.Region = r.Region
    return nil
}

func (h *seaplaneClusterCreate) PostExpand(_ context.Context, r *rosa.Runtime, input *hfpathbind.ClusterCreateInput, obj *v1alpha1.Cluster) error {
    obj.Spec.HostedCluster.Platform.Type = hypershiftv1beta1.AWSPlatform
    obj.Spec.HostedCluster.Platform.AWS.RolesRef =
        seaplane.ComputeRolesRef(input.OperatorRolesPrefix, r.Creator.AccountID, r.Creator.Partition)
    return nil
}
```

---

## Update Operations

`RunUpdateCluster` and `RunUpdateNodePool` follow the same four-method pattern, adding a `uid` parameter (server-assigned UID for routing) and `namespace` for namespace-scoped resources (NodePool).

The SDK's typed `Update()` (PUT) is used rather than JSON merge patch, so the consumer fetches the current state and applies only the changed fields before calling Update. `PostExpand` for update commands does a Get-then-merge to preserve existing spec fields not covered by the update input.

---

## Draft generation from the FieldRegistry

`pathbind-draft.yaml` is generated in two stages:

1. **`marker-scanner`** reads write-mode annotations from CRD types → `field_metadata.json`
2. **`pathbind-gen --mode=init`** walks the OpenAPI schema from each FieldRegistry entry to its scalar leaves, recording `goType` and `operations` for each leaf path

| FieldRegistry field      | `pathbind-draft.yaml`                       |
| ------------------------ | ------------------------------------------- |
| `fieldPath`              | starting point for OpenAPI walk             |
| `writeMode: mutable`     | `operations: [create, update]`              |
| `writeMode: immutable`   | `operations: [create]`                      |
| `writeMode: service-set` | excluded                                    |
| `hidden: true`           | excluded (unless `writeMode` is `mutable`)  |
| `ownerType`              | resource grouping (Cluster, NodePool, etc.) |

### Workflow when a new CRD field is added

```text
1. Add +seaplane:write-mode=mutable to the new CRD type field
2. make codegen-registry          → field_metadata.json updated
3. make generate-pathbind-draft   → pathbind-draft.yaml updated (leaf paths + goType appear)
   (steps 1-3 are SDK-side, happen automatically in CI)
4. Bump SDK dependency in consumer (copy pathbind-draft.yaml to vendor)
5. make generate-seaplane       → generated consumer code updated automatically
   (new flag appears with auto-derived name; no manual edit required unless UX override needed)
6. Optionally add entry to pathbind-overrides.yaml to customize flag name/description
```

---

## End-to-End Workflow

This section covers the full lifecycle from a CRD change to a client consuming it — SDK authoring, release, dependency bump, and client regeneration.

### Overview

```text
SDK repo (rosa-seaplane-api)            Consumer repo (e.g. ocm/rosa)
──────────────────────────────            ─────────────────────────────
1. Annotate CRD field                     4. Bump SDK in go.mod + go mod vendor
2. make generate                          5. make generate-seaplane
3. Tag release vX.Y.Z                     6. Add override entry (optional)
                                          7. Implement handler changes (if any)
```

---

### Step 1 — Annotate the CRD field (SDK)

Add `+seaplane:write-mode` to the field in the CRD Go type:

```go
// +seaplane:write-mode=mutable      ← appears in create and update
// +seaplane:write-mode=immutable    ← create only
ExampleField string `json:"exampleField,omitempty"`
```

`service-set` fields are excluded from the draft automatically. `hidden` fields are also excluded unless they have `write-mode=mutable` — mutable hidden fields appear in the draft so consumers can set them even though they are not surfaced in the Kubernetes OpenAPI spec (e.g. `nodeLabels`).

If the field belongs to an embedded passthrough type (e.g. `HostedClusterSpecPassthrough`), annotate the field there; the marker-scanner associates it with the correct owner CRD.

---

### Step 2 — Run the full generation pipeline (SDK)

```bash
make generate
```

This runs in order:

1. `codegen-registry` — marker-scanner → `field_metadata.json`
2. `codegen-passthrough` — regenerate HyperShift passthrough types, then refresh
   `field_metadata.json` so downstream generators consume the regenerated markers
3. `codegen-conversion` — public CRD types and conversion functions
4. `generate-deepcopy` — deepcopy methods
5. `manifests` — CRD YAML and feature-set variants
6. `generate-clientset` — typed client SDK and platform wrappers
7. `generate-openapi` — OpenAPI spec from public types
8. `generate-pathbind-draft` — `pathbind-draft.yaml` (leaf paths via OpenAPI walk)

Commit all generated files. CI verifies generation is up to date via `make verify`.

> **OpenAPI scope**: `generate-openapi` runs against `api/v1alpha1/public/` (the reduced public types). Fields from upstream types that are reduced or overridden (e.g. `PlatformSpec`) must be registered in `openapi-merge -schemas` and have their type substitution in `conversion/mirror_types.go`.

---

### Step 3 — Tag a release (SDK)

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

Use semver. Breaking changes to `pathbind.go` (Expand/Flatten signatures, `hfsdk:` tag semantics) warrant a minor or major bump.

---

### Step 4 — Bump the SDK dependency (consumer)

```bash
go get github.com/openshift-online/rosa-seaplane-api@vX.Y.Z
go mod tidy
go mod vendor
```

This updates `go.mod`, `go.sum`, and repopulates `vendor/`. The key vendored files for pathbind are:

| Vendored file                            | Purpose                                                  |
| ---------------------------------------- | -------------------------------------------------------- |
| `clientset/pathbind/pathbind.go`         | Core Expand/Flatten engine                               |
| `clientset/pathbind/pathbind-draft.yaml` | Source of truth for leaf paths                           |
| `clientset/cmd/pathbind-gen/*.go`        | Generator source (run via `go run` in consumer Makefile) |

---

### Step 5 — Regenerate consumer code (consumer)

```bash
make generate-seaplane
```

This runs `pathbind-gen --mode=cobra` from the vendored source and rewrites `pkg/seaplane/pathbind/`:

```text
pkg/seaplane/pathbind/
├── cluster_create_gen.go    ← ClusterCreateInput struct + RegisterClusterCreateFlags
├── cluster_update_gen.go    ← ClusterUpdateInput struct + RegisterClusterUpdateFlags
├── nodepool_create_gen.go
├── nodepool_update_gen.go
├── oidcconfig_create_gen.go
└── helpers_gen.go
```

**Changes automatically** — no manual edit required:

- New draft leaf paths appear as struct fields with auto-derived flag names and cobra registration
- Removed draft paths are removed from the struct and flag list
- `goType` changes update the default consumer field type

**Requires a manual edit**:

- UX customization (custom flag name, description, `required: true`) → add to `pathbind-overrides.yaml`
- Fields the consumer does not want to expose → add an override entry with `flag: ""` to suppress

Commit the updated generated files.

---

### Step 6 — Add override entry (optional, consumer)

If the auto-derived flag name or description is inadequate, add an entry to `pathbind-overrides.yaml` and re-run `make generate-seaplane`:

```yaml
resources:
  cluster:
    aliases:
      - path: spec.hostedCluster.networking.apiServer.port
        flag: api-server-port
        description: "Custom port for the hosted cluster API server."
        required: true # if set, auto-prompts in interactive mode
```

If no override is added the field is fully usable with its auto-derived flag name. Clients that don't need to expose a field simply don't pass its flag — pathbind skips unset fields automatically.

---

### Step 7 — Implement handler changes (consumer, if needed)

Most new fields require no handler change — the flag is registered, `Expand` sets the value, done.

| Scenario                                                              | Where to handle   |
| --------------------------------------------------------------------- | ----------------- |
| Input validation (format, range, cross-field constraints)             | `PreRequest`      |
| Field derived from another input (e.g. subnet → VPC/zone)             | `PreRequest`      |
| Custom interactive prompt for a non-required field                    | override `Prompt` |
| Field is an enum constant set regardless of user input                | `PostExpand`      |
| Field is a computed struct (e.g. `RolesRef` from prefix + account ID) | `PostExpand`      |
| Response field must be displayed or stored                            | `PostResponse`    |

#### `PreRequest` — validation and derivation

`PreRequest` runs after flag parsing and before `Expand` and the SDK call. Return an error to abort with a user-facing message. Cross-field constraints, format checks (CIDR notation, ARN format), and AWS resource lookups (subnet → VPC/zone) all belong here.

#### `Prompt` — interactive mode

`Prompt` is called before `PreRequest` when `interactive.Enabled()` is true. The generated `GeneratedXxxPrompt` struct auto-prompts for every field marked `required: true` in the overrides: if the field is still empty when `Prompt` runs, it shows an interactive prompt and blocks until the user provides a value.

Fields that are **not** marked `required: true` are never auto-prompted. To add interactive prompting for an optional field, embed `GeneratedXxxPrompt` and override `Prompt`:

```go
type seaplaneClusterCreate struct {
    hfpathbind.GeneratedClusterCreatePrompt // auto-prompts required: true fields
}

func (h *seaplaneClusterCreate) Prompt(ctx context.Context, r *rosa.Runtime, cmd *cobra.Command, input *hfpathbind.ClusterCreateInput) error {
    // Auto-prompt all required fields first.
    if err := h.GeneratedClusterCreatePrompt.Prompt(ctx, r, cmd, input); err != nil {
        return err
    }
    // Optional field: only prompt if interactive and not already set.
    if interactive.Enabled() && input.DisplayName == "" {
        var err error
        input.DisplayName, err = interactive.GetString(interactive.Input{
            Question: "Display name for the cluster (optional)",
            Help:     cmd.Flags().Lookup("display-name").Usage,
        })
        if err != nil {
            return err
        }
    }
    return nil
}
```

`Prompt` should only fill fields — all validation happens in `PreRequest`.

---

### Client expectations summary

| What                                             | Automatic                        | Manual                          |
| ------------------------------------------------ | -------------------------------- | ------------------------------- |
| New struct field in `ClusterCreateInput`         | ✓ after `make generate-seaplane` |                                 |
| New cobra flag with auto-derived name            | ✓                                |                                 |
| Interactive prompt for `required: true` fields   | ✓ via `GeneratedXxxPrompt`       |                                 |
| Flag name / description customization            |                                  | ✓ `pathbind-overrides.yaml`     |
| Mark field as required                           |                                  | ✓ `required: true` in overrides |
| Suppress a field from exposure                   |                                  | ✓ `flag: ""` in overrides       |
| Input validation                                 |                                  | ✓ `PreRequest`                  |
| Field derived from another input                 |                                  | ✓ `PreRequest`                  |
| Custom interactive prompting for optional fields |                                  | ✓ override `Prompt`             |
| Enum constant or computed struct                 |                                  | ✓ `PostExpand`                  |
| Help section membership (Platform API vs Shared) | ✓ via annotation                 |                                 |
| Dependency bump                                  |                                  | ✓ `go get` + `go mod vendor`    |
| Consumer code regeneration                       |                                  | ✓ `make generate-seaplane`      |

---

## Extending to Other Consumers

A different consumer (e.g. `terraform-provider-rhcs`) would:

1. Invoke `pathbind-gen --mode=tf` against the same `pathbind-draft.yaml` and its own `pathbind-overrides.yaml`
2. Receive a generated struct with appropriate framework types and schema declarations
3. Implement the generated `XxxHandler` interface against the Terraform provider framework

The `pathbind-draft.yaml` and the `pathbind.Expand`/`Flatten` engine are shared; only the generated consumer layer and the override YAML are consumer-specific.
