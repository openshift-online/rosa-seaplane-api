# Feature Gating: Current Context and Forward Design

**Status**: 📝 Draft — ready for peer review
**Jira**: [ROSAENG-61391](https://redhat.atlassian.net/browse/ROSAENG-61391)
**Author**: Chris Doan
**Date**: 2026-10-15
**Origin**: [api-management.md](api-management.md) (the original marker-system design)
**Supersedes**: [feature-gated-write-mode-design.md](feature-gated-write-mode-design.md)
(ROSAENG-61570 — design retained, implementation status corrected by this doc) and
`feature-gate-version-awareness-design.md` (folded into this doc)
**Target packages**: `api/v1alpha1/`, `hack/api-codegen/pkg/{markers,featuregate,validation,registry}/`, `platform-api/internal/codegen/featuregate/`, `platform-api/pkg/validation/`, `platform-api/pkg/handlers/`

---

## Summary

HyperFleet's API management design ([api-management.md](api-management.md)) established three
field markers — **visibility** (`+k8s:openapi-gen=false`), **write mode**
(`+hyperfleet:write-mode=X`), and **feature gate** (`+openshift:enable:FeatureGate=X`) — plus a
generated field-metadata registry and runtime enforcement in the platform API. The
feature-gate-aware write-mode extension (ROSAENG-61570) designed a fourth capability:
per-gate write-mode overrides via
`+hyperfleet:validation:FeatureGateAwareWriteMode:featureGate="X",writeMode="Y"`.

This document:

1. **Describes the current context** — what is actually built, wired, and adopted as of today
   (verified against the working tree), including the gaps that must close before any
   extension is useful.
2. **Studies how openshift/api handles the same problem** — FeatureGateAware markers,
   per-feature-set CRD variants, and a version-aware feature-gate registry — verified against
   the `github.com/openshift/api` module we already depend on.
3. **Proposes the forward design**: an OCP-style, version-aware gate registry feeding the
   existing marker and validation machinery, so that gates — and therefore field access and
   write modes — can be enabled based on the hosted cluster's **OCP version** in addition to
   the customer's feature set.

**TL;DR**: Follow the openshift/api model of separating *gate enablement conditions*
(version × feature set, maintained as data in a gate registry) from *field-level marker
content* (which stays gate-keyed and unchanged) from *enforcement* (our field-metadata
registry and platform API validators, already built). Unlike openshift/api — whose API server
serves exactly one OCP version per deployment, so version is handled by release branching —
our platform API serves customers whose clusters run **many OCP versions concurrently**, so
version must be a **runtime dimension of gate resolution**. We extend the gate registry with
per-version graduation data and thread the cluster's OCP version into validation through a
`ValidationContext`. No field markers change.

---

## Part 1: Current context

### 1.1 What the original design specified (api-management.md)

- Three markers on a single set of Go types (`api/v1alpha1/`), which are the source of truth
  for CRD schemas and the customer-facing OpenAPI spec.
- Passthrough generation from upstream HyperShift types with safe defaults for new fields
  (hidden, service-set), idempotent re-curation from the field-metadata registry.
- A single gate registry (`HyperFleetFeatureGates`) mapping gate → stage
  (GA / TechPreview / DevPreview), grouped into feature sets
  (Default / TechPreviewNoUpgrade / DevPreviewNoUpgrade).
- Gate resolution from two sources: the account's feature-set assignment, **and per-account
  entitlements** (`customerHasGate` checks stage first, then an entitlements service).
- Codegen: CRD YAML, per-feature-set CRD variants, OpenAPI schema (visible fields only),
  field-metadata registry, REST types with generated projection.
- Runtime enforcement: the platform API walks every write request against the registry.

### 1.2 What is built and wired today (verified in the working tree)

| Capability | State | Evidence |
| --- | --- | --- |
| Visibility + write-mode markers | ✅ Widely used | Throughout `api/v1alpha1/` (incl. generated passthrough types) |
| Feature-gate marker (`+openshift:enable:FeatureGate`) | ✅ Used on 8 fields | `api/v1alpha1/configuration.go` (kubelet-advanced ×3, machine-config ×1), `api/v1alpha1/public/{kubeletconfig,machineconfigspec}_types.go` (×4) |
| Gate registry | ⚠️ Exists, duplicated | `hack/api-codegen/pkg/featuregate/registry.go` and hand-mirrored `platform-api/internal/codegen/featuregate/registry.go`; 6 gates defined |
| Field-metadata registry (Go + JSON) | ✅ Generated and imported | `hack/api-codegen/pkg/registry/field_metadata.{go,json}`; platform-api imports it |
| Platform API write validation | ✅ Wired for Cluster/NodePool | `platform-api/pkg/validation/field_validator.go`; handlers call `ValidateCreate`/`ValidateUpdate` |
| Feature-set resolution at runtime | ❌ **Not wired** | All 4 call sites hardcode `featuregate.Default`: `platform-api/pkg/handlers/cluster.go:163,392`, `nodepool.go:212,333` |
| Per-account entitlements (`customerHasGate`) | ❌ Not implemented | No entitlement lookup exists in platform-api |
| Per-feature-set CRD variants | ⚠️ Tool exists, **not in pipeline** | `hack/api-codegen/cmd/crd-variants` variant mode exists, but `make manifests` only invokes `--strip-passthrough-cel`; no variant YAMLs are committed; operator installs base CRDs only |
| Gate-aware write-mode markers | ⚠️ Mechanism merged, **zero adoption** | Scanner/validator/codegen + tests exist (ROSAENG-61570); no field in `api/` uses `FeatureGateAwareWriteMode` |
| Version awareness | ❌ Absent | `IsGateEnabled(gate, featureSet)` is a pure function of entitlement stage; no OCP-version input anywhere |
| Reference validator in hack | ⚠️ Dead code, diverging | `hack/api-codegen/pkg/validation/validator.go` (explicit `EnabledGates` input model) is imported by nothing outside its module |

**Correction to the record**: `feature-gated-write-mode-design.md` is headed
"✅ Implemented… Complete and ready for use". The accurate status is: the *mechanism* (marker
parsing, registry generation, validator support, tests) is merged, but **no field adopts it**
and the runtime gate resolution it depends on (feature sets, entitlements) is not wired. The
doc's status header has been updated to point here.

### 1.3 Gaps that block the feature-gating story end-to-end

1. **Entitlement dimension is inert.** `featuregate.Default` is hardcoded; TechPreview /
   DevPreview gates can never be enabled for a real customer through the platform API today.
2. **Gate registry is maintained twice** (hack + platform-api mirror) with no generation
   keeping them in sync.
3. **Two validator implementations** with different input models (explicit gate list vs
   stage-based feature set) — drift risk; the hack one is unused.
4. **CRD variant generation is not part of the pipeline**, so even the schema-level story
   (gated fields visible/invisible per feature set) is not exercised.
5. **No version dimension**, which is the subject of this document: for ROSA HCP, whether a
   customer can use a gated capability depends on the **OCP version of their hosted cluster**
   (`spec.hostedCluster.release.version` on create, `status.version` when running), because
   the capability is implemented in the hosted cluster's payload.

---

## Part 2: How openshift/api handles this (the model we follow)

Verified against `github.com/openshift/api@v0.0.0-20260715165912` (already a dependency of
every module in this repo).

### 2.1 Gate enablement is data: version × cluster profile × feature set

openshift/api keeps a **feature-gate registry** in its `features` package. Each gate declares
a list of *enablement conditions*, each a conjunction of a version set, a cluster-profile
set, and feature sets, via a fluent builder:

```go
// openshift/api features/features.go (abridged)
FeatureGateAWSEuropeanSovereignCloudInstall = newFeatureGate("AWSEuropeanSovereignCloudInstall").
    reportProblemsToJiraComponent("...").contactPerson("...").
    enhancementPR("...").
    enable(inVersion(4, greaterThanOrEqual), inClusterProfile(Hypershift),
           inTechPreviewNoUpgrade(), inDevPreviewNoUpgrade()).
    mustRegister()
```

Resolution is a three-way predicate (`features/util.go`):

```go
func (s *featureGateStatus) isEnabled(version uint64, clusterProfile ClusterProfileName,
                                      featureSet configv1.FeatureSet) bool {
    matchesVersion := len(s.version) == 0 || s.version.Has(version)        // empty = all
    matchesProfile := len(s.clusterProfile) == 0 || s.clusterProfile.Has(clusterProfile)
    return matchesVersion && matchesProfile && s.featureSets.Has(featureSet)
}
```

Note what this buys them:

- **Version predicates are first-class** (`inVersion(4, greaterThanOrEqual)`) — a gate is
  TechPreview from 4.17 and GA (added to `inDefault()`) from 4.19 by *editing registry data*,
  not code.
- Gates carry **governance metadata** (owning Jira component, responsible person,
  enhancement PR) and fail registration without it.
- The registry generates **machine-readable payload manifests per version-group × profile ×
  feature set** — consecutive versions with identical gate content collapse into one file,
  e.g. `featureGate-4-10-Hypershift-Default.yaml` — consumed by the cluster-config-operator,
  plus a human-readable matrix (`features.md`) rendered for review.

### 2.2 Field-level content is gate-keyed markers, materialized into per-feature-set CRD variants

Fields use `FeatureGateAware` markers with an explicit **default arm** (`featureGate=""`)
plus gated arms — the same pattern our ROSAENG-61570 design adopted:

```go
// openshift/api config/v1/types_dns.go (abridged)
// +openshift:validation:FeatureGateAwareXValidation:featureGate="",rule=`...aws|aws-cn|aws-us-gov...`,message="..."
// +openshift:validation:FeatureGateAwareXValidation:featureGate=AWSEuropeanSovereignCloudInstall,rule=`...aws|aws-cn|aws-us-gov|aws-eusc...`,message="..."

// openshift/api config/v1/types_image_policy.go (abridged)
// +openshift:validation:FeatureGateAwareEnum:featureGate="",enum=PublicKey;FulcioCAWithRekor
// +openshift:validation:FeatureGateAwareEnum:featureGate=SigstoreImageVerificationPKI,enum=PublicKey;FulcioCAWithRekor;PKI
```

The codegen emits gated content **only into the CRD variant for that feature set**. Verified
by diffing generated manifests for the DNS CRD:

- `0000_10_config-operator_01_dnses-Default.crd.yaml` carries the default-arm CEL rule.
- `0000_10_config-operator_01_dnses-TechPreviewNoUpgrade.crd.yaml` (annotated
  `release.openshift.io/feature-set: TechPreviewNoUpgrade`) carries the gate-specific rule.

### 2.3 Enforcement is structural

OCP's kube-apiserver serves the CRD variant matching the cluster's active feature set (the
CVO/cluster-config-operator swaps CRDs when the `FeatureGate` CR's `spec.featureSet`
changes). Gate enforcement happens at the **served-schema level** — gated rules and fields
literally do not exist for clusters without the gate. There is no application-level gate
check.

### 2.4 Where version lives — and why we differ

| Concern | openshift/api | HyperFleet platform API |
| --- | --- | --- |
| One served OCP version per deployment | Yes — repo is branched per release; per-version schema differences ride release branches | **No** — one platform API serves accounts whose clusters run many OCP versions concurrently |
| Gate enablement per version | Registry data (version predicates) → payload manifests per version-group | Must be **runtime** resolution: (account feature set, cluster OCP version) → enabled gates |
| Field content per gate | Markers → per-feature-set CRD variants | Markers → field-metadata registry → platform API validation (REST gateway, not served CRDs) |
| Enforcement | Served CRD variant (structural) | `FieldValidator` on every write (already built) |

This is the central architectural difference: **we cannot branch per OCP version, so version
must join feature set as a runtime dimension of gate resolution.** Everything else about the
OCP model — registry-as-data, gate-keyed markers with default arms, generated audit
artifacts — transfers directly.

### 2.5 Should Hypershift own the version gating? (peer-review discussion)

A fair challenge raised in review: the `HostedCluster` spec is OCP-version-agnostic and the
HCP's control-plane operator silently ignores fields it does not understand, so version
constraints arguably belong *on the Hypershift passthrough fields* ("this only works with
HCPs at OCP 5.1+") rather than duplicated in Hyperfleet — one source of truth instead of
two, and it matches Hypershift owning the pass-through part of the API.

Our position (and the trade-off we accept):

1. **Hypershift markers alone do not protect the customer.** Because the CPO silently
   drops unknown fields, a marker on a Hypershift type is documentation until something
   *enforces* it. The only component that sees the customer request, the account
   entitlements, and the cluster's OCP version together is the Hyperfleet platform API —
   so enforcement stays here regardless of where the data lives.
2. **The entitlement dimension has no Hypershift representation.** Account feature sets
   and per-customer enablement (e.g. a Karpenter-style limited-availability rollout) exist
   only in our platform.
3. **Target state: Hypershift markers as the input to our registry.** When Hypershift
   annotates passthrough fields with version constraints, we should *generate* our
   enablement conditions from those markers instead of hand-curating them — collapsing
   the two sources of truth rather than maintaining parallel ones. Until Hypershift adopts
   such markers, the registry is Hyperfleet-maintained data; drift risk is mitigated by the
   reviewable gate matrix (3.3) and by aligning graduation changes with the Hypershift
   team.

---

## Part 3: Forward design

### 3.1 Principles

1. **Gate enablement conditions are data in one registry** — version × feature set (cluster
   profile reserved as a future dimension), OCP fluent-builder style.
2. **Field markers stay gate-keyed and unchanged** — `+openshift:enable:FeatureGate`,
   `FeatureGateAwareWriteMode`. Version knowledge never spreads into field comments.
3. **Enforcement stays where it already is** — the generated field-metadata registry and the
   platform API validators. No new enforcement path.
4. **Generated audit artifacts** — a per-version-group × feature-set enabled-gates table
   (OCP's `features.md` equivalent) is committed and reviewed in PRs, so graduation is a
   visible, reviewable event.

### 3.2 Version-aware gate registry (the core change)

Replace the single-stage gate entries with OCP-style enablement conditions. Recommended
shape, deliberately close to `openshift/api/features` so the pattern is familiar to OCP
reviewers:

```go
// pkg/featuregate (single consolidated package — see 3.6)
type EnableCondition struct {
    Versions       []VersionRange // e.g. {Min: "4.17"} — empty = all versions
    FeatureSets    []FeatureSet   // e.g. TechPreviewNoUpgrade, DevPreviewNoUpgrade
}

type VersionRange struct {
    Min string // inclusive, e.g. "4.17" (whole minor) or "4.19.4" (z-stream); "" = unbounded
    Max string // inclusive, same forms; "" = unbounded
}

type FeatureGateInfo struct {
    Description         string
    OwningJiraComponent string  // governance, per OCP model
    EnhancementPR       string
    DocsURL             string  // customer-facing docs/KCS for the feature (see 3.10)
    RequestAccessURL    string  // how to request access if not entitled (see 3.10)
    Enable              []EnableCondition // OR of conditions; each condition is AND of its dimensions
}
```

Registry example — a gate that is TechPreview from 4.17 and GA from 4.19:

```go
"HyperFleetEtcdConfig": {
    Description:         "Allows customers to configure etcd settings",
    OwningJiraComponent: "HyperFleet",
    Enable: []EnableCondition{
        {Versions: []VersionRange{{Min: "4.17", Max: "4.18"}}, FeatureSets: []FeatureSet{TechPreviewNoUpgrade, DevPreviewNoUpgrade}},
        {Versions: []VersionRange{{Min: "4.19"}},           FeatureSets: []FeatureSet{Default, TechPreviewNoUpgrade, DevPreviewNoUpgrade}},
    },
},
```

Resolution:

```go
// IsGateEnabled reports whether gate is enabled for this account and cluster version.
// ocpVersion is a full version string (e.g. "4.19.0-ec.3") or "" for unknown.
// Unknown gates fail closed (disabled). An unknown version fails closed *only for
// version-constrained conditions*: it matches exclusively conditions whose Versions
// is nil, so version-unconstrained gates keep today's behavior.
func IsGateEnabled(gate string, featureSet FeatureSet, ocpVersion string) bool {
    info, ok := HyperFleetFeatureGates[gate]
    if !ok {
        return false
    }
    for _, cond := range info.Enable {
        if cond.matchesVersion(ocpVersion) && cond.matchesFeatureSet(featureSet) {
            return true
        }
    }
    return false
}
```

Semantics:

- A gate is enabled iff **any** enablement condition matches (conditions are OR'ed; within a
  condition, version range and feature set are AND'ed) — same shape as OCP's
  `isEnabled`.
- Before the earliest `Min` of every condition → the gate does not exist in that payload →
  disabled for everyone, regardless of feature set.
- Graduation is a registry edit (add a condition with `Default` from version N), followed by
  regeneration — no code, no marker churn. This is exactly how OCP promotes gates.
- A `Max` bound models features removed from the payload.
- Gates with a single condition `{{Versions: nil, FeatureSets: [Default, ...]}}` behave as
  today's non-versioned gates — **fully backward compatible**.
- **Unknown/empty version at runtime**: an empty `ocpVersion` matches *only* conditions
  whose `Versions` is nil. Consequences:
  - Version-unconstrained gates (`Versions: nil` on every condition) behave exactly as
    today — enabled per feature set even when the version is unknown. This preserves the
    backward-compatibility claim above.
  - Version-constrained gates (any condition with a non-nil `Versions`) fail closed when
    the version is unknown: the platform must not enable a capability the cluster payload
    may not have.
- **Version granularity**: bounds may carry an optional z-stream component. A bound like
  `"4.19"` matches the whole 4.19 minor (OCP-style, the common case); a bound like
  `"4.19.4"` is compared at patch granularity, for features that graduate mid-z-stream
  (e.g. a fix backported into 4.19.4). Mixing is allowed: `{Min: "4.19.4"}` requires
  4.19.4 or later; `{Min: "4.17", Max: "4.18"}` covers all of 4.17 and 4.18.

Worked example for `HyperFleetEtcdConfig` above:

| Cluster version | FeatureSet | Enabled? |
| --- | --- | --- |
| 4.16.x | any | No — before first condition |
| 4.17.x / 4.18.x | Default | No |
| 4.17.x / 4.18.x | TechPreviewNoUpgrade | Yes |
| 4.19.x | Default | Yes |
| "" (unknown) | any | No — fail closed (version-constrained gate) |

A tiny semver helper is needed (`NormalizeMajorMinor("4.19.0-ec.3") → "4.19"`, range compare
on major.minor — and on patch when a bound specifies one; prerelease suffixes ignored).
None exists in `api/` or `platform-api/` today; it lives next to the gate registry.

### 3.3 Generated audit artifacts (OCP's `features.md` equivalent)

Codegen walks the registry and commits:

- `docs/api/generated/feature-gate-matrix.md` — a table of gate × version-group × feature
  set → enabled/disabled, mirroring OCP's `features.md`; consecutive versions with identical
  content collapse into version-groups (e.g. "4.17–4.18").
- JSON export of the same data (consumable by CLI/docs tooling to render
  "requires OCP 4.19+" next to gated fields).

Graduation then shows up as a reviewable diff in the PR that promotes the gate — the OCP
review affordance we most want to copy.

### 3.4 Runtime plumbing: `ValidationContext`

The platform API validators currently take a positional `fs featuregate.FeatureSet` that
call sites hardcode to `Default`. Replace with a request-scoped context carrying both
dimensions:

```go
// platform-api/pkg/validation
type ValidationContext struct {
    FeatureSet featuregate.FeatureSet // account entitlement tier
    OCPVersion string                 // hosted cluster OCP version; "" = unknown → fail closed
}

func (v *FieldValidator) ValidateCreate(spec any, ctx ValidationContext) ValidationErrors
func (v *FieldValidator) ValidateUpdate(newSpec, existingSpec any, ctx ValidationContext) ValidationErrors
```

Version sources:

- **Create**: `spec.hostedCluster.release.version` (required for HCP clusters, so always
  known).
- **Update**: the **running** version (`status.version`), not a pending upgrade target —
  conservative: a capability becomes usable only after the upgrade completes. Matters when
  `spec.controlPlaneUpgradePolicy` is mid-flight (open question 2).

Effective write-mode resolution (already implemented for ROSAENG-61570) is unchanged except
the call becomes `IsGateEnabled(gate, ctx.FeatureSet, ctx.OCPVersion)`: specific-gate match →
default (`featureGate=""`) arm → base `+hyperfleet:write-mode`.

Error messages should state the blocking dimension without leaking gate names, e.g.
`"field requires an OpenShift 4.19 or later cluster"` or
`"field is immutable for clusters below version 4.19"`.

**Worked customer-facing example — OCPSTRAT-3600 (ClusterAutoscaler parity)**: several
ClusterAutoscaler `.spec` fields exposed on `HostedCluster.spec.autoscaling` land in a
future OCP payload. A customer on OCP 4.19 who sets one of them must **not** have the
field silently dropped by the HCP's cluster-autoscaler (the CPO ignores unknown fields —
no error, no effect, which is the worst UX). With this design, the platform API rejects
the write up front:

```
HTTP/1.1 400 Bad Request
{
  "reason": "FeatureGateVersionNotMet",
  "message": "spec.autoscaling.balanceSimilarNodeGroups requires an OpenShift 5.1 or
              later cluster; this cluster runs 4.19.0-ec.3",
  "min_version": "5.1"
}
```

The alternative — accepting and silently ignoring — is explicitly rejected by this design.
The customer can also ask *before* writing, via the discovery API (3.10).

### 3.5 Wiring the entitlement dimension (required, independent follow-up)

Version-aware gating is only meaningful once the other dimension is real. Concretely:

1. Resolve the account's feature set (from the account/entitlement service — the
   `customerHasGate` design in api-management.md) in platform API middleware.
2. Replace the four hardcoded `featuregate.Default` call sites with the resolved context.
3. Per-account gate overrides (entitlements) can then be expressed as an optional
   `EnabledGates []string` field on `ValidationContext` — which also gives the unused hack
   reference validator's input model a home. **Overrides must not bypass version
   support**: `IsGateEnabled` remains the sole authority on the version dimension. An
   override is applied only *after* the registry confirms version compatibility, and may
   only widen *account/feature-set eligibility* — concretely, the resolver first finds
   conditions matching the cluster's OCP version (fail-closed when the version is unknown
   and the gate is version-constrained), then treats the override as satisfying the
   feature-set component of those conditions. An override can never enable a gate on a
   version whose payload does not support it.

### 3.6 Consolidation (prerequisite)

- **One gate registry and one featuregate package.** `platform-api/internal/codegen/featuregate`
  is deleted; platform-api imports the consolidated package (it already imports
  `hack/api-codegen/pkg/registry` across modules via replace directives). Whether the
  package should move out of `hack/` is open question 5.
- **One validator.** `hack/api-codegen/pkg/validation` (dead, diverging input model) is
  removed or reduced to thin tests of the shared resolver.

### 3.7 Conflict rules become codegen errors

The never-formalized "first marker match wins" runtime convention (original design's Open
Question #2) must not survive contact with a second dimension. The marker scanner should
fail `make codegen` on ambiguous configurations — e.g. two `FeatureGateAwareWriteMode`
overrides whose (gate, version-range) conditions can both be true with different modes, or a
default arm duplicating the base write-mode. At runtime, if multiple distinct enabled gates
still match, first-marker-match is retained but treated as a codegen gap to fix.

### 3.8 CRD variants (operator side) — optional, deferred

OCP enforces structurally via served CRD variants. Our enforcement point is the platform
API, and the operator consumes CRs *after* platform validation, so variants are not required
for correctness. To be explicit about *which CRDs*: these would be **HyperFleet's own
CRDs** — the `Cluster` and `NodePool` CRs generated from `api/v1alpha1/` and served in the
regional management clusters (consumed by the hyperfleet-operator). The upstream
Hypershift `HostedCluster` CRD is never variant-ed by us; it is owned by Hypershift.
Wiring `crd-variants` generation into `make manifests` (it currently only
runs in `--strip-passthrough-cel` mode) would give defense-in-depth and OCP parity, at the
cost of per-feature-set CRD distribution to management clusters. Deferred until the runtime
story is complete (open question 6).

### 3.9 Multi-region deployment considerations

Architecture fact: one regional cluster per AWS region, each running platform-api and the
hyperfleet-operator; Cluster/NodePool state lives in the regional cluster; platform-api is
stateless. Implication: **everything baked into the platform-api binary — the gate registry,
the field-metadata registry, and the generated feature-gate matrix — is versioned per
deployment, not globally.** During staggered regional rollouts, regions disagree about gate
enablement.

What is *not* a problem: state is region-local, so skew cannot corrupt data — it only
changes answers. Mixed OCP versions *within* a region are already handled by per-request
resolution (3.2). The concerns are consistency, truth sources, and rollout ordering:

1. **Regional skew is asymmetric — enforce monotonicity.** Adding enablements in newer
   platform-api versions leaves older regions fail-closed (safe; features appear
   region-by-region). *Tightening or removing* enablements in newer versions leaves older
   regions fail-open (a customer can set a field in one region that another region's newer
   API rejects). Rule: gate data changes across platform-api versions must be **monotonic**
   (only add enablements); removals require a deprecation window longer than the maximum
   regional rollout skew.
2. **The generated matrix is a per-release audit artifact, not fleet truth.** Each
   platform-api version has its own matrix; "which matrix is authoritative" is undefined
   during rollout. Follow the OCP split: a **runtime, per-region discovery endpoint** is
   authoritative (effective gates for the caller's account and a given cluster version —
   the analog of `oc get featuregate`), while `feature-gate-matrix.md` stays a repo-level
   audit artifact with an "as of platform-api vX" header. rosa CLI and docs should consume
   the discovery endpoint, not embedded matrix data. Product announcements of GA should
   wait until all regions carry the graduation entry.
3. **Version-lock platform-api and operator per regional cluster.** The field validator
   skips fields absent from its baked-in registry (`if !exists { continue }` — fail-open on
   unknown fields). If the operator's served CRD schema and platform-api's registry drift
   apart (different deployment cadences), new fields bypass validation silently. Deploy the
   two as a version-locked pair per region, and add a startup or CI check that the
   field-metadata registry matches the served CRDs.
4. **Graduation is double-gated — ship data early.** A feature needs the cluster on ≥ N
   *and* the region's platform-api to contain the graduation entry for N. Since the registry
   is data, conditions for an upcoming OCP version can be committed before that version is
   GA, hiding the rollout lag.

Decision required (open question 9): keep gate data **deployment-versioned** (recommended —
OCP-like, each regional platform is its own gate authority, monotonic updates, skew is
fail-closed) vs **externalized runtime data** synchronized across regions (global
consistency, but reintroduces the runtime-configuration approach the original design
rejected, and splits truth between markers in code and conditions in data). Note the
account feature-set dimension is already planned as central runtime data, so a hybrid —
registry in code, activation overlay at runtime — is also viable.

**Review outcome**: deployment-versioned is confirmed (peer +1); the discovery endpoint
moves into scope — see 3.10.

### 3.10 Customer-facing discovery API

Raised in review (Karpenter example): customers need to self-serve "which features are
available to me, on my cluster's version — and if not, how do I get access?" Silently
ignoring a gated field is not acceptable (3.4), and an opaque 400 is only half the answer.

Add a read-only endpoint to platform-api, authoritative per region (per 3.9.2), resolving
against the caller's actual account entitlements:

```
GET /api/feature_gates?cluster_version=4.19
```

```json
{
  "cluster_version": "4.19",
  "feature_gates": [
    {
      "id": "HyperFleetKarpenter",
      "state": "enabled",
      "description": "Karpenter-based node management",
      "docs_url": "https://access.redhat.com/articles/karpenter-hcp",
      "request_access_url": null
    },
    {
      "id": "HyperFleetClusterAutoscalerBalanceSimilarNodeGroups",
      "state": "disabled",
      "reason": "requires_openshift_version",
      "min_version": "5.1",
      "docs_url": "https://access.redhat.com/articles/...",
      "request_access_url": null
    },
    {
      "id": "HyperFleetTechPreviewEtcdConfig",
      "state": "disabled",
      "reason": "not_entitled",
      "request_access_url": "https://red.ht/request-tech-preview"
    }
  ]
}
```

Semantics:

- `state` is computed exactly as validation would compute it (`IsGateEnabled` with the
  caller's account feature set and the given cluster version), so the API can never claim
  a capability validation would then reject.
- `reason` distinguishes the blocking dimension (`requires_openshift_version`,
  `not_entitled`, `not_available_in_deployment`) without leaking internal gate names.
- `docs_url` / `request_access_url` come from the new `FeatureGateInfo` metadata fields
  (3.2), so each gate can point at a KCS, runbook, or T&C as suggested in review.
- `cluster_version` is optional: when omitted, the response lists gates and account
  entitlement only (version-dependent reasons report `requires_openshift_version` with
  `min_version` so CLI/docs can still render guidance).
- rosa CLI and the docs tooling consume this endpoint rather than embedded matrix data
  (per 3.9.2); the committed matrix stays a repo-level audit artifact.

Scope: the endpoint ships with Phase 2 (it is the natural consumer of the same registry
that feeds validation), not deferred to Phase 4.

---

## Part 4: Migration plan

| Phase | Content | Depends on |
| --- | --- | --- |
| 0 | Consolidate featuregate packages; delete the diverging hack validator; single effective-mode resolver | — |
| 1 | `ValidationContext` (FeatureSet + OCPVersion); wire account feature-set resolution; replace hardcoded `Default` | 0 |
| 2 | Version-aware gate registry (`EnableCondition`); semver helper (minor + optional z-stream); generated feature-gate matrix (per-release artifact, "as of platform-api vX" header); `IsGateEnabled(gate, fs, version)` with fail-closed semantics; customer-facing discovery endpoint (see 3.10) | 1 |
| 3 | First real adoption of `FeatureGateAwareWriteMode` on a production field (e.g. the etcd TechPreview→GA example from ROSAENG-61570), using a version-graduated gate | 2 |
| 4 (optional) | CRD variant generation in `make manifests`; per-account entitlement overrides on `ValidationContext` (version-compatible only, see 3.5) | 2 |

No existing field markers change in any phase. Gates adopt version conditions incrementally,
exactly as fields adopt gated write-modes incrementally.

## Part 5: Testing strategy

- **Graduation matrix tests**: table-driven gate × version × feature set → expected enabled
  (the worked-example table as code).
- **Boundary tests**: `4.19.0-ec.3` vs `4.19` vs `4.19.1`; version exactly at a range bound;
  `Max` bounds.
- **Fail-closed tests**: unknown gate; version set but feature set not entitled, and vice versa; unknown/empty version on a version-constrained gate (disabled) vs a version-unconstrained gate (unchanged behavior); account override attempting to enable a version-incompatible gate (rejected).
- **Update-path tests**: version taken from `status.version` while
  `controlPlaneUpgradePolicy` targets a newer version (running version wins).
- **Codegen conflict tests**: ambiguous marker combinations fail generation.
- **Consolidation regression tests**: shared resolver output identical to today's for the
  existing (non-versioned) gate corpus.
- **Matrix artifact test**: committed `feature-gate-matrix.md` is up to date with the
  registry (`make verify`).
- **Monotonicity test** (see 3.9): gate data in platform-api version N+1 does not remove or
  tighten enablements relative to version N without a flagged deprecation — checked against
  the previously released registry during codegen verification.
- **Registry/CRD consistency check**: the field-metadata registry baked into platform-api
  matches the CRDs served by the version-locked operator (startup or CI check), closing the
  fail-open path for unknown fields.

## Part 6: Open questions for peer review

1. **Entitlement wiring**: is account feature-set resolution a blocking prerequisite for the
   version work (Phase 1 before Phase 2), or can they proceed in parallel given both feed
   the same `ValidationContext`?
2. **Update-time version source** — *closed in review*: running version
   (`status.version`, conservative) wins; "configure for the version being upgraded to"
   is a separate concern and out of scope here (peer +1).
3. **Version granularity** — *amended in review*: major.minor by default, with optional
   z-stream bounds (`"4.19.4"`) for features that genuinely graduate mid-z-stream. See 3.2.
4. **Registry shape**: fluent builder verbatim (OCP familiarity) vs. declarative struct
   literals as sketched (simpler to diff and generate from). Recommendation: declarative,
   since we also generate artifacts from it.
5. **Package location** for the consolidated featuregate package: stay in
   `hack/api-codegen/pkg/` (imported cross-module like `pkg/registry` today) vs. move to
   `api/` or a new shared module.
6. **CRD variants**: wire per-feature-set CRD variant generation into the pipeline for
   defense-in-depth/OCP parity, or rely on platform API validation alone?
7. **Cluster-profile dimension**: reserve `EnableCondition` extension for a profile
   dimension (OCP: Hypershift vs SelfManagedHA), e.g. if HyperFleet ever serves non-HCP
   variants, or omit?
8. **Per-account gate overrides**: keep the entitlements escape hatch
   (`ValidationContext.EnabledGates`) from api-management.md — but, per review, overrides
   may only widen account/feature-set eligibility after registry-derived version
   compatibility is confirmed (see 3.5); they can never bypass version conditions.
9. **Gate data locality** (see 3.9) — *first half closed in review*: gate data stays
   **deployment-versioned** per regional platform-api (OCP-like, monotonic, fail-closed
   skew). The per-region discovery endpoint ("effective gates for my account and cluster
   version") is in scope of this work — specified in 3.10, shipping with Phase 2 — so CLI
   and docs never depend on baked-in matrix data.

## References

- Peer-review comments and resolutions: [feature-gating-review-responses.md](feature-gating-review-responses.md)
- Original marker-system design: [api-management.md](api-management.md)
- Gate-aware write-mode design (status corrected): [feature-gated-write-mode-design.md](feature-gated-write-mode-design.md)
- openshift/api (verified in module cache `v0.0.0-20260715165912-72066cc9718b`):
  - `features/features.go`, `features/util.go` — version × profile × feature-set gate
    registry, fluent builder, `isEnabled`
  - `payload-command/cmd/write-available-featuresets`, `payload-command/render/write_featureset.go`
    — per-version-group payload manifests and the `features.md` matrix
  - `config/v1/types_dns.go`, `config/v1/types_image_policy.go` — `FeatureGateAwareXValidation`
    / `FeatureGateAwareEnum` markers with `featureGate=""` default arms
  - `config/v1/zz_generated.crd-manifests/*-{Profile}-{FeatureSet}.crd.yaml` — per-variant
    CRD manifests (Default vs TechPreviewNoUpgrade diff verified)
- Implementation under review: `hack/api-codegen/pkg/{markers,featuregate,validation,registry}/`,
  `platform-api/internal/codegen/featuregate/`, `platform-api/pkg/validation/field_validator.go`,
  `platform-api/pkg/handlers/{cluster,nodepool}.go`
