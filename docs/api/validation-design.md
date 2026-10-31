# Validation and ObjectValidation: Current-State Assessment

**Status**: 📝 Draft — ready for peer review
**Jira**: [ROSAENG-75120](https://redhat.atlassian.net/browse/ROSAENG-75120) (child of
[ROSAENG-61569](https://redhat.atlassian.net/browse/ROSAENG-61569) — *Implement CRD
client-side validation using objectvalidation pattern*)
**Author**: Chris Doan
**Date**: 2026-10-19
**Repos assessed**:
- rosa CLI v2 seaplane: [`openshift/rosa`](https://github.com/openshift/rosa) (`cmd/**/seaplane.go`, `pkg/seaplane/`)
- TF v2 seaplane: [`terraform-redhat/terraform-provider-rhcs`](https://github.com/terraform-redhat/terraform-provider-rhcs) (`provider/seaplane/`)
- API + clientset: this repo (`seaplane-api/`, `clientset/`, `hack/api-codegen/`, `seaplane-operator/`)

---

## Summary

This document inventories how validation is performed today across the ROSA HCP v2
("seaplane") platform and client flow — client-side (rosa CLI, Terraform provider) and
server-side (platform API, kube-apiserver CRD schemas, operator) — and assesses the state of
the **objectvalidation pattern** (validating objects against CRD schemas on the client side,
per the [cluster-capi-operator reference](https://github.com/openshift/cluster-capi-operator/tree/main/pkg/controllers/crdcompatibility/objectvalidation))
that ROSAENG-61569 proposes to adopt.

**TL;DR**:

1. **Server-side validation is layered and substantial** — hand-written domain validators +
   the generated field-metadata `FieldValidator` in the platform API, backed by structural
   CRD schemas (including CEL) enforced by the kube-apiserver on the regional cluster.
2. **Client-side validation is thin, hand-written, and duplicative** — each client re-implements
   a small, divergent subset of the server rules (proxy URLs, tags, image types, subnet/CIDR
   math, autoscaler flags) purely for early, better-context error messages.
3. **The objectvalidation pattern is not implemented anywhere** in the three repos. No client
   loads a CRD schema or validates objects against one before submission; the closest
   existing machinery is the **field-metadata registry** (`hack/api-codegen/pkg/registry`)
   already generated from the same markers that produce the CRDs — which the platform API
   uses server-side but clients do not consume.
4. **Recommendation**: implement ROSAENG-61569 as a shared, client-embeddable validator
   built on the existing generated registries (field metadata + CRD YAML) rather than
   introducing a fourth, pattern-only implementation. This aligns with the feature-gating
   design (see [feature-gating-design.md](feature-gating-design.md), PR #593), which already
   plans to consolidate the duplicate validator in `hack/api-codegen/pkg/validation`.

---

## Part 1: The seaplane request flow (where validation can happen)

```
rosa CLI v2 / TF v2 provider          platform API                 regional cluster
┌─────────────────────────┐    HTTPS  ┌────────────────────────┐   CR write  ┌──────────────────┐
│ flag/schema binding      │─────────▶│ handler hand-written    │───────────▶│ kube-apiserver    │
│ (pathbind-gen: CLI flags│          │ domain validators       │            │ structural CRD    │
│  / TF plugin-framework  │          │ + FieldValidator        │            │ schema + CEL      │
│  schema)                │          │ (field-metadata         │            │ (seaplane-operator│
│ hand-written pre-checks │          │  registry)              │            │  CRDs, no         │
│                         │          │                         │            │  admission        │
└─────────────────────────┘          └────────────────────────┘            │  webhooks)        │
                                                                        └──────────────────┘
```

Validation layers, client → server:

| # | Layer | Where | Mechanism | Authoritative? |
| --- | --- | --- | --- | --- |
| 1 | CLI flag binding | `rosa` `pkg/seaplane/pathbind` (generated) | cobra flags → `ClusterCreateInput` et al.; type-safe setters, integer-narrowing overflow checks | No |
| 2 | CLI hand-written pre-checks | `rosa` `cmd/**/seaplane.go`, `pkg/seaplane/` | ad-hoc Go functions (subnets/CIDRs, proxy, network type, autoscaler, machine-pool name, AWS checks) | No |
| 3 | TF schema binding | `provider/seaplane/generated/*_resource_gen.go` (pathbind-gen, TF mode) | plugin-framework attribute types; **no declarative validators emitted** | No |
| 4 | TF hand-written pre-checks | `provider/seaplane/{cluster,nodepool,oidcconfig}_handler.go` | ad-hoc `diag` functions (tags, capabilities overlap, image type) | No |
| 5 | Platform API domain validation | `seaplane-api/pkg/validation/*.go` + handlers | hand-written validators (CIDRs, tags, spot, identity, trust bundle, …) | Yes (intent) |
| 6 | Platform API field-metadata validation | `seaplane-api/pkg/validation/field_validator.go` | generated registry: visibility, write mode, feature gates, immutability, service-set | Yes (intent) |
| 7 | kube-apiserver CRD schema | `seaplane-operator/config/crd/bases/*.yaml` | structural schema: types, required, enums, patterns, min/max, CEL (`x-kubernetes-validations`) | Yes (last line) |

---

## Part 2: Current state — server side

### 2.1 Platform API (`seaplane-api/`)

The platform API is the intended validation authority. Verified in the working tree:

| Concern | Implementation | Notes |
| --- | --- | --- |
| Field visibility / write mode / feature gates / immutability | `pkg/validation/field_validator.go` (`FieldValidator`, `ValidateCreate` / `ValidateUpdate` / `PreserveServiceSet`) | Driven by the **generated field-metadata registry** (`hack/api-codegen/pkg/registry/field_metadata.json`) produced from markers on `api/v1alpha1`. Per-CRD-kind (`Cluster`, `NodePool`). |
| Network/CIDR | `pkg/validation/network_cidr_validator.go` | Machine/service CIDRs, per-region rules |
| AWS tags | `pkg/validation/aws_tags.go` | Key/value regexes, reserved prefixes, shared tag budget across cluster + node pools |
| Spot instances | `pkg/validation/spot.go` | Termination-handler queue URL, cross-cluster uniqueness |
| Identity/namespace | `pkg/validation/identity.go`, `additional_allowed_principals.go` | Account-namespace binding |
| Misc | `additional_trust_bundle.go`, `json_fields.go`, `preserve.go`, `validateNodePoolReplicas` (handlers) | |

Handler wiring (`pkg/handlers/cluster.go`, `nodepool.go`): every create/update calls the
domain validators plus `FieldValidator.ValidateCreate/ValidateUpdate` before writing the CR.
Errors return structured `ValidationErrors` (`{field, reason}`) with API error codes.

**Known gaps (confirmed, and already designed against in PR #593):**

- `featuregate.Default` is **hardcoded** at all 5 call sites (`cluster.go:293,522,532`,
  `nodepool.go:232,402,414`) — entitlement/TechPreview gating is inert at runtime.
- A **second, diverging validator** exists in `hack/api-codegen/pkg/validation/validator.go`
  (explicit `EnabledGates`/`Fields` request model) — unused outside its own module.
- The field validator **skips unknown fields** (`if !exists { continue }`), i.e. fail-open
  for fields absent from its baked-in registry — mitigated operationally by version-locking
  platform-api and the operator (see feature-gating design §3.9.3).

### 2.2 kube-apiserver / CRD schemas (regional cluster)

- The `seaplane-operator` installs CRDs (`seaplane.io_clusters`, `_nodepools`, `_oidcconfigs`,
  …) with full structural schemas: ~370 required/pattern/bound constraints and CEL
  `x-kubernetes-validations` rules in the Cluster CRD alone (e.g. upgrade-schedule CEL rules
  verified in `config/crd/bases/seaplane.io_clusters.yaml`).
- **No admission webhooks** exist in seaplane-operator (no `ValidatingWebhookConfiguration`,
  no objectvalidation package) — CRD structural validation is the only apiserver-level check.
- Passthrough subtrees (fields mirrored from HyperShift types) get their CEL stripped at
  generation time (`make manifests` → `crd-variants --strip-passthrough-cel`) because CEL
  copied from upstream may reference fields that don't exist in our projection — i.e.
  passthrough fields are validated *less* at this layer, by design.

### 2.3 Operator

`seaplane-operator` performs no additional request validation; it consumes CRs already
accepted by the platform API and renders HostedCluster/NodePool objects. Hypershift's CPO
silently ignores fields it doesn't understand — which is exactly why layer 5/6 must not
fail open (see feature-gating design §2.5).

---

## Part 3: Current state — client side

### 3.1 rosa CLI v2 seaplane

| Concern | Where | Duplicates server rule? |
| --- | --- | --- |
| Flag binding & types | `pkg/seaplane/pathbind` (generated from `pathbind-overrides.yaml`, same generator as the clientset) | Type-level only |
| Subnet/CIDR ↔ machine CIDR math | `cmd/create/cluster/seaplane.go: validateSeaplaneSubnets` | Partially (server: `network_cidr_validator.go`) |
| Proxy URLs, trust bundle | `validateSeaplaneClusterProxyFlags` → `ocm.ValidateHTTPProxy/HTTPSProxy/AdditionalTrustBundle` | Reused **legacy v1 validators** shared with OCM flow |
| Network type / network flags | `validateSeaplaneNetworkType`, `validateSeaplaneNetwork` | Partially |
| Autoscaler flags | `clusterautoscaler.ValidateAutoscalerFlagsForHostedCp` + conversion | Yes (HCP-supported-subset rule) |
| Machine pool name, EC2 metadata tokens | `mpValidation.ValidateMachinePoolName`, `ocm.ValidateHttpTokensValue` | Yes (server: nodepool handler) |
| AWS-side checks (instance types, availability) | `rosaaws.ValidateMachinePoolAWS` (live EC2 calls) | Beyond server scope |
| Feature gates / write modes / immutability | **none** | No — relies entirely on server 400s (`seaplane.WithAPIErrorDetails`) |

Notably, the CLI's per-command validation set is **inconsistent** — create-cluster has rich
pre-checks, edit-cluster fewer, machinepool a different mix — and none of it is generated
from the API's source of truth (markers / CRDs / field-metadata registry), so it drifts.

### 3.2 Terraform provider v2 seaplane (`provider/seaplane/`)

| Concern | Where | Duplicates server rule? |
| --- | --- | --- |
| Schema binding | `generated/{cluster,nodepool,oidcconfig}_resource_gen.go` (pathbind-gen `--mode=tf`, plugin-framework) | Type-level only; **no `Validators` are emitted** into the generated schema |
| AWS tags | `cluster_handler.go: validateClusterTags` | Yes (key/value regex, reserved prefix — mirrors `aws_tags.go`; budget intentionally left to server) |
| Capabilities overlap | `cluster_handler.go: validateClusterCapabilities` | Deliberate subset: overlap check client-local, name validation left to server |
| Image type enum | `nodepool_handler.go: validateImageType` | Yes (mirrors server enum via `hypershiftv1beta1.ImageType` constants) |
| Feature gates / write modes / immutability | **none** | No — surfaces server 400s as apply-time diags |

The handler comments show a consciously applied rule: *validate locally only what can never
be valid server-side or what gives better UX at plan time; leave authoritative rules to the
platform API*. That rule is good but applied by hand, per attribute, with no systematic
derivation from the CRD schema.

### 3.3 Clientset (`clientset/`)

The generated clientset performs **no semantic validation** — it is transport + typed
bindings only. `pathbind.go` contains only mechanical conversion checks (e.g. integer
narrowing overflow). This is the natural embedding point for a shared client-side
validator, since both clients already depend on it.

---

## Part 4: ObjectValidation — current state

**The objectvalidation pattern (per the parent task's reference,
[cluster-capi-operator/crdcompatibility/objectvalidation](https://github.com/openshift/cluster-capi-operator/tree/main/pkg/controllers/crdcompatibility/objectvalidation))
is not implemented in any of the three repos.** Verified by search:

| Repo | `objectvalidation` refs | CRD-schema client-side validation |
| --- | --- | --- |
| rosa-seaplane-api (incl. clientset, operator) | 0 | None |
| rosa CLI | 0 | None |
| terraform-provider-rhcs | 0 | None |

What exists instead, and how it maps to the pattern's goals:

| objectvalidation ingredient | Seaplane equivalent today | Gap |
| --- | --- | --- |
| Load CRD schema from data | CRD YAMLs exist (`seaplane-operator/config/crd/bases/`) but are not embedded in any client | Clients have no schema source |
| Set up CRD validation (apiextensions machinery) | Nothing client-side | Missing |
| Validate CR/objects against schema before submission | Hand-written per-command/per-handler checks only | Missing, drift-prone |
| Forward/backward compatibility validation | Nothing | Missing |

The closest existing asset is the **field-metadata registry**
(`hack/api-codegen/pkg/registry/field_metadata.{go,json}`): generated from the same markers
that generate the CRDs, already consumed server-side by `FieldValidator`, and already
imported cross-module. It covers visibility/write-mode/gate/immutability — rules the CRD
schema alone does *not* express — while the CRD YAML covers types/enums/patterns/CEL that
the field-metadata registry does not. The two are complementary halves of "the schema".

---

## Part 5: Gaps and risks

| # | Gap | Risk |
| --- | --- | --- |
| 1 | No client-side schema validation; every rule beyond a hand-picked few surfaces only as a server 400 (CLI) or apply-time diag (TF) | Poor UX for TF (errors at apply, not plan); error text quality depends on server messages |
| 2 | Hand-written client checks duplicate server rules without a shared source | Drift (client accepts what server rejects; client rejects what server accepts) |
| 3 | Hand-written checks are inconsistently distributed across commands/resources | Coverage holes (e.g. edit-cluster vs create-cluster) |
| 4 | Passthrough CEL stripped from CRDs + field-validator skip-unknown-fields | Fail-open paths if registries/CRDs drift from platform-api version (addressed in feature-gating design §3.9.3) |
| 5 | `featuregate.Default` hardcoded server-side; two diverging validators | Gating inert; maintenance burden (designed in PR #593, not yet implemented) |
| 6 | TF generated schema carries no validators; CLI pathbind carries none | Single place (codegen) where a fix would benefit both clients |

---

## Part 6: Recommendation for ROSAENG-61569

Adopt the objectvalidation **pattern**, but implement it on the assets we already generate,
as one shared library embedded in the clientset:

1. **One shared validator package** (e.g. `clientset/validation` or the consolidated
   featuregate package proposed in PR #593 Phase 0) that composes:
   - **field-metadata registry** → visibility, write mode, feature gates, immutability,
     service-set (already implemented server-side in `FieldValidator` — reuse it, don't
     fork it);
   - **CRD structural schema** (embed the generated CRD YAMLs, or a derived JSON-schema
     projection) → types, enums, patterns, bounds, CEL where expressible client-side
     (the objectvalidation core: load schema → `apiextensions-apiserver` validation →
     validate object).
2. **Both clients consume it at their binding layer**, not per-command:
   - rosa CLI: in the pathbind layer (`pkg/seaplane/pathbind`) or a single pre-submit hook
     per resource op, replacing the scattered ad-hoc functions (keep AWS live checks, which
     are not schema-expressible).
   - TF: emit schema validators during pathbind-gen `--mode=tf` from the same registry, so
     validation moves from apply-time diags to **plan-time** errors — the biggest UX win.
3. **Keep the division of labor explicit** (the TF handlers already state it well): the
   platform API stays authoritative; client validation is a projection of the same generated
   truth, allowed to lag (fail-open client-side is acceptable; fail-open server-side is not).
4. **Forward/backward compatibility** (the objectvalidation webhook's second use): expose
   schema-versioned validation so the CLI/TF can validate against the platform-api version
   of a *target* region before/after rollout skew — aligns with the deployment-versioned
   gate-data model and per-region discovery endpoint in feature-gating design §3.9–3.10.
5. **Sequence** after PR #593's Phase 0 (validator consolidation) so there is exactly one
   validator implementation to embed, not three.

### Expected outcome

| Consumer | Today | After |
| --- | --- | --- |
| rosa CLI | ~8 hand-written pre-checks, uneven coverage | Generated full-schema pre-checks + retained AWS live checks |
| TF v2 | 3 hand-written diags at apply time | Generated plan-time schema validation + authoritative server checks |
| platform API | Two validators, gates inert | One shared validator, versioned with clients |
| objectvalidation | Absent | Pattern realized via embedded CRD schemas + field-metadata registry |

---

## References

- Parent task: [ROSAENG-61569](https://redhat.atlassian.net/browse/ROSAENG-61569) — objectvalidation pattern implementation
- Reference implementation: [openshift/cluster-capi-operator — objectvalidation](https://github.com/openshift/cluster-capi-operator/tree/main/pkg/controllers/crdcompatibility/objectvalidation)
- Feature-gating design (peer-reviewed, PR #593): [feature-gating-design.md](feature-gating-design.md) — `ValidationContext`, validator consolidation, fail-closed semantics
- Marker system origin: [api-management.md](api-management.md)
- Evidence (verified in working trees, 2026-10-19):
  - `seaplane-api/pkg/validation/` (field_validator.go + 8 domain validators), `pkg/handlers/{cluster,nodepool}.go`
  - `hack/api-codegen/pkg/validation/validator.go` (unused duplicate), `hack/api-codegen/pkg/registry/field_metadata.json`
  - `seaplane-operator/config/crd/bases/seaplane.io_*.yaml` (structural schema + CEL; no webhooks)
  - `rosa`: `cmd/{create,edit}/cluster/seaplane.go`, `cmd/create/machinepool/seaplane.go`, `pkg/seaplane/`
  - `terraform-provider-rhcs`: `provider/seaplane/{cluster,nodepool,oidcconfig}_handler.go`, `provider/seaplane/generated/`
