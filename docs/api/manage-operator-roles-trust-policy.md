# Managing Operator Role Definitions and Trust Policies in ROSA Seaplane V2

**JIRA**: [ROSAENG-66827](https://redhat.atlassian.net/browse/ROSAENG-66827)
**Status**: Draft — for peer review
**Last Updated Date**: 2026-10-29

## Summary

ROSA Seaplane V2 clusters need a set of customer-account IAM **operator roles** (one per
control-plane operator, plus a worker instance role) with OIDC **trust policies** and
permission policies. Two clients create these roles today — the `rosa` CLI and the
`terraform-provider-rhcs` (TF) provider — and the role definitions are being duplicated
between them. This document compares the options for defining, packaging, and updating a
single source of truth for the V2 operator role specs and trust policy templates,
recommends an approach, and analyzes migration impact for HCP clusters moving from V1
(OCM) to V2 (Seaplane Platform API).

## Background

### What is in an "operator role definition"?

For each operator role, a client needs:

| Field                           | Example                                                                                                                 |
| ------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Role name / suffix              | `<prefix>-ingress`, `<prefix>-cloud-controller-manager`                                                                 |
| Service account (trust subject) | `system:serviceaccount:openshift-ingress-operator:ingress-operator`                                                     |
| Permission policy               | `arn:aws:iam::aws:policy/service-role/ROSAIngressOperatorPolicy` (AWS-managed) or a customer-managed inline policy (V1) |
| Trust policy template           | Web-identity trust on the OIDC provider, conditioned on `sub` and `aud`; EC2 service trust for the worker role          |
| Tags                            | `red-hat-managed`, `hypershift-policies`, role prefix, managed-policies marker                                          |
| Version applicability           | Which OpenShift/HCP versions require the role or policy revision                                                        |

### How V1 (classic + HCP via OCM) handles it

In V1, **the OCM API is the single source of truth**:

- `GET /api/clusters_mgmt/v1/aws_inquiries/sts_policies?policy_type=OperatorRole` returns
  the policy documents (JSON) keyed by e.g. `openshift_hcp_<role>_policy`.
- `GET /api/clusters_mgmt/v1/aws_inquiries/sts_operator_roles` (credential requests)
  returns the operator set: name, namespace, service accounts, min/max versions.
- Both `rosa` CLI (`OCMClient.GetPolicies`, `GetCredRequests`) and the TF provider
  (`rhcs_rosa_hcp_operator_roles` and `rhcs_rosa_hcp_policies` data sources) fetch these
  at runtime and build roles/trust policies from the response.
- Policies attached to operator roles are **customer-managed** by default (inline JSON
  served by OCM), versioned per OpenShift minor, with a "default policy version" concept
  and an `upgrade roles` flow that patches roles as new versions are released.

Pros of the V1 model: single source of truth, updates ship without client releases,
non-Go consumers can consume the REST API. Cons: policy content must be maintained in the
OCM backend; the inquiry endpoints are OCM-global, not per-region; API availability is a
runtime dependency of `create operator-roles`.

### Current state in V2

**rosa CLI** (`rosa create operator-roles` in seaplane mode,
`cmd/create/operatorroles/by_prefix_seaplane.go`):

- A hardcoded `getHCPOperatorRoles()` map: suffix → service account, AWS-managed
  policy ARNs, description; trust policy built with inline `fmt.Sprintf` templates.
- Role suffixes in `pkg/seaplane/roles.go` (`OperatorRoleSuffixes`, `ComputeRolesRef`).
- No versioning and no update flow; V2 relies on **AWS-managed service-role policies**
  (`arn:aws:iam::aws:policy/service-role/ROSA*`) instead of OCM-served inline policies.

**terraform-provider-rhcs**:

- V1 HCP operator roles use the OCM data sources above.
- For V2 (seaplane resources), there is **no operator-roles resource yet**. The
  `rhcs_seaplane_cluster` / `oidcconfig` resources only expose `oidc_issuer` and
  `operator_roles_prefix`; users are expected to hand-write `aws_iam_role` resources with
  trust policies they copy from docs or the rosa CLI's manual-mode output — i.e. the
  definition would be duplicated a third time in every user TF module.

**Seaplane Platform API** (`rosa-seaplane-api`): no IAM/policy resource or inquiry
endpoint exists. The repo owns the CRD types (`Cluster.Spec.rolesRef`,
`OIDCConfig.Spec.InstallerRoleArn`) and the generated `clientset` SDK consumed by both
clients.

## Problem statement

1. The V2 operator role definition (suffixes, service accounts, policy ARNs, trust
   templates, tags) is duplicated — today between the CLI's hardcoded map and (soon) TF
   user modules, with no owner of record.
2. There is no update story: when HCP adds/removes an operator, changes a service
   account, or AWS ships a new managed policy, every client must be re-cut in lockstep
   or clusters drift.
3. We must decide — per the JIRA — whether the definitions live **in the Platform API**
   (V1-style) or **in an SDK library module** (V2-style), and how dynamic data is
   distributed so workflows can react to changes.

## Requirements

- **R1 Single source of truth** for V2 operator role specs and trust policy templates.
- **R2 Multi-client**: rosa CLI (Go), TF provider (Go), and future clients (CAPA,
  SRE tooling) must all consume it. Non-Go consumption is desirable but not blocking.
- **R3 Update path**: definitions can change (new operators, new service accounts, new
  managed policies) without forcing simultaneous client releases; there must be a
  controlled way to update existing roles (`rosa upgrade operator-roles` equivalent).
- **R4 Offline/deterministic behavior**: clients should work even if the dynamic source
  is unavailable, falling back to a pinned/bundled revision.
- **R5 Migration safety**: V1 HCP clusters migrating to V2 must not break; see
  [Migration analysis](#migration-analysis-v1-hcp--v2).
- **R6 Auditability/versioning**: every definition revision is identifiable
  (semver or content hash) and surfaced in created roles (tags) for drift detection.

## Options considered

### Option A — V1-style: serve definitions from the Platform API

Add a read-only "inquiry" surface to the Seaplane Platform API (e.g.
`GET /api/seaplane/v1/aws/inquiries/operator-roles`), backed by either a ConfigMap/CR
synced by the operator, or static content embedded in the API deployment. Clients fetch
it at runtime, exactly like V1's OCM endpoints.

**Pros**

- Direct parity with the proven V1 model; clients already know this pattern.
- Works for non-Go consumers (REST).
- Updates ship region-by-region from the API without client releases.
- Cedar/authz already exists for SigV4-authenticated API callers.

**Cons**

- The Seaplane API is currently CRD/OpenAPI-generated from Kubernetes types; a
  free-form "policy document" endpoint does not map cleanly onto the CRD→OpenAPI
  codegen pipeline (would need a new Seaplane-native CRD or a hand-written route —
  an exception to the passthrough architecture).
- Adds a hard runtime dependency on API availability for `create operator-roles`,
  which today (V2/`WithAWSOnly`) deliberately avoids the platform API in the AWS-only
  path.
- Multi-region: each regional API must be kept in sync; definitions are global in
  nature.
- Heavier implementation cost (API surface, authz policy, testing) than a library.

### Option B — Shared Go library module + versioned dynamic data (recommended)

Create a small standalone Go module inside the `rosa-seaplane-api` monorepo (e.g.
`iam/` — sibling of `clientset/`), owned by the Seaplane team, that contains:

1. **Canonical, versioned definitions** (Go structs + embedded defaults):
   role suffixes, service accounts, permission policy references (AWS-managed ARNs
   per partition), tags, min/max OpenShift versions, and a `DefinitionVersion`
   (semver). This becomes the single source of truth, consumed by the rosa CLI and
   the TF provider via a `replace` directive / go.mod require just like `clientset`.
2. **Trust policy builder functions**: parameterized templates
   (`BuildWebIdentityTrustPolicy(partition, accountID, issuerURL, serviceAccount)`,
   `BuildEC2TrustPolicy()`) — no client ever hand-writes JSON again.
3. **Dynamic data overlay**: the definitions above are the _pinned floor_; a remote,
   versioned document (S3 bucket / gist-like store, served over CDN with ETag
   caching) can carry newer revisions. The library exposes
   `FetchLatest(ctx, FetchOptions)` and `Resolve(local, remote)` so clients can
   react to changes without a release, while still working offline against the
   embedded copy (R4). Signing (or at minimum a strong hash + version monotonicity
   check) guards the remote document.

**Pros**

- Matches the JIRA direction: "shared workflows within the SDK as a library-like
  module… dynamic data in S3/gist so the workflow can react to changes".
- Zero new API surface; both existing clients already depend on this monorepo via
  `clientset`/`api`, so wiring is trivial.
- Compile-time type safety and unit tests for both definitions and trust templates.
- Works in the CLI's AWS-only mode (no platform API dependency).
- TF provider gets a first-class `rhcs_seaplane_operator_roles` resource/datasource
  built on the library instead of user-copied JSON.
- Deterministic: the embedded revision is the contract; the remote overlay is
  strictly optional and forward-only.

**Cons**

- Go-only: a non-Go consumer (e.g. a Python tool or raw CFN) must go through the
  rosa CLI / TF, or we additionally publish generated artifacts (JSON/CFN) — see
  deliverable D4 below, which mitigates this.
- Two-channel distribution (module + remote overlay) needs a clear precedence and
  signing story; under-specified fetching could become a security question.
- Changes to definitions still require a module release for the "floor" to move;
  only the overlay is dynamic.

### Option C — Keep per-client definitions (status quo)

Each client maintains its own hardcoded list; document the canonical values in docs.

**Pros** — no coordination work today.
**Cons** — guaranteed drift (three copies counting TF user modules); no update story;
fails R1/R3; migration tooling cannot trust any single list. Rejected.

### Option D — Publish CloudFormation/JSON templates only

V2 already uses AWS-managed policies; publish a CFN template (as V1 does for account
roles) that creates the whole role set, and have clients shell out or render it.

**Pros** — battle-tested UX for AWS-first customers; the Platform API IAM CFN template
is already the naming convention source.
**Cons** — doesn't give the CLI/TF programmatic definitions; CFN drift vs. library;
still needs a source of truth to generate the template. Useful **as an artifact
generated from Option B**, not as the primary mechanism.

## Recommendation

**Option B**, with D4 of the deliverables generating the CFN/JSON artifacts (Option D as
an output, not an alternative), and an explicit **re-evaluation trigger** for Option A:
if a non-Go consumer or a per-region override need materializes, add a thin read-only
serving endpoint **generated from the same canonical definitions** so the library
remains the source of truth either way.

### Proposed module sketch

```
rosa-seaplane-api/
  iam/                          # new standalone Go module
    go.mod                      # module github.com/openshift-online/rosa-seaplane-api/iam
    types.go                    # OperatorRoleSpec, TrustPolicyInput, DefinitionSet
    definitions.go              # embedded canonical definition set (version pinned)
    trustpolicy.go              # BuildWebIdentityTrustPolicy / BuildEC2TrustPolicy
    roles.go                    # RoleNames(prefix), ComputeRolesRef(prefix, acct, partition)
    remote.go                   # FetchLatest(ctx, opts) + Resolve(local, remote)
    remote_schema.go            # JSON schema for the overlay document
    cfn/                        # generated CFN template + JSON export (CI job)
    iam_test.go
```

Notes:

- `pkg/seaplane/roles.go` in the rosa CLI and the TF `utils.go` prefix helpers are
  migrated to call this module; the CLI keeps thin wrappers for backwards
  compatibility of internal APIs.
- The overlay document is a JSON serialization of the same `DefinitionSet` type, with
  `version` > embedded version required for the overlay to win. Precedence is strictly
  field-level: the overlay can add roles or narrow versions but cannot remove a role
  that embedded definitions still require (protects R4 determinism).
- The S3 location is published under a stable, partition-aware URL with
  `Cache-Control` + ETag; clients fetch once per command invocation at most.

### Update flow (R3)

1. Seaplane team PRs a change to `iam/definitions.go` (source of truth) and the
   overlay document; CI regenerates CFN/JSON and validates every role's trust policy
   against the current HCP operator set (cross-check with HyperShift source).
2. Clients pick up new floors via routine dependency bumps; the overlay lets
   already-shipped clients react to urgent changes (e.g. AWS policy rename) without a
   release.
3. `rosa upgrade operator-roles --prefix …` (seaplane mode) diffs the account's
   current roles/tags/policies against the resolved definition set and reconciles:
   attach new policies, update trust policies, tag roles with the applied
   `definition_version`. TF reconciles on refresh of a `rhcs_seaplane_operator_roles`
   resource.

## Migration analysis (V1 HCP → V2)

Key differences that matter for migration:

| Aspect         | V1 HCP (OCM)                                                                        | V2 (Seaplane)                                                       |
| -------------- | ----------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| Role naming    | `<prefix>-<namespace>-<name>` (e.g. `<prefix>-kube-system-capa-controller-manager`) | `<prefix>-<suffix>` (e.g. `<prefix>-node-pool-management`)          |
| Permissions    | Customer-managed inline policies served by OCM, versioned per OpenShift minor       | AWS-managed service-role policies (`ROSA*Policy`)                   |
| Trust policy   | Same web-identity shape; issuer is the cluster's / unmanaged OIDC config            | Same shape; issuer from Platform API `OIDCConfig`                   |
| Discovery tags | `red-hat-managed`, operator namespace tags                                          | `red-hat-managed`, `hypershift-policies`, `managed_policies` marker |
| Update path    | `rosa upgrade ...` via OCM policy versions                                          | Proposed `definition_version` reconciliation (above)                |

Implications and risks:

1. **Role names are not 1:1.** A V1 HCP cluster's operator roles cannot be reused
   verbatim by V2's `ComputeRolesRef` naming. If in-place migration (same customer
   account, same cluster moved to V2 management) reuses the existing role set, either:
   (a) V2 must accept the V1 naming (rolesRef is supplied by the client, so the
   Platform API does not care — the _clients_ must map old names), or (b) migration
   creates a new V2 prefix and the old V1 roles are retired after cutover. The library
   should therefore expose a `FromV1RoleSet` adapter that recognizes V1-named roles
   (they already carry `red-hat-managed` + namespace tags) and builds a valid
   `rolesRef` without creating duplicate roles.
2. **Policy type change (customer-managed → AWS-managed).** Migrating a cluster whose
   roles still have inline OCM-served policies to a V2 client flow that attaches
   AWS-managed policies is safe _additively_ (attach new policy) but destructive if we
   also detach old ones during the same reconciliation. The upgrade flow must attach
   first, verify, then detach, and must never detach a policy still referenced by a
   running V1-managed cluster sharing the prefix.
3. **OIDC provider continuity.** V1 unmanaged OIDC configs are being imported into the
   Platform API. Trust policies reference the issuer, not the management plane, so
   existing trust policies remain valid post-migration as long as the issuer URL is
   unchanged. No trust-policy rewrite is required for migration — only for issuer
   rotation.
4. **Version gating.** V1 definitions carry min/max OpenShift versions per operator;
   the V2 definition set must keep those fields so a migrated cluster on an older
   minor still resolves the correct (possibly smaller) operator set. Without this, a
   migrated cluster would show drift for roles that did not exist in its version.
5. **No OCM dependency in the V2 path.** `rosa create operator-roles` in seaplane mode
   is AWS-only; migration tooling that needs V1 definitions (e.g. to recognize old
   roles) must not call OCM. The library approach keeps everything offline-capable —
   an advantage over Option A here.
6. **Shared prefixes.** Customers may have V1 and V2 clusters sharing an operator-role
   prefix. Any reconciliation must scope changes by role name pattern _and_ the
   `hypershift-policies`/`managed_policies` tags, and refuse to touch roles that don't
   match either naming scheme.

## Implementation plan

| #   | Deliverable                                                                                                        | Repo                      |
| --- | ------------------------------------------------------------------------------------------------------------------ | ------------------------- |
| D1  | `iam/` module: types, embedded definitions, trust-policy builders, tests                                           | rosa-seaplane-api         |
| D2  | rosa CLI: replace `getHCPOperatorRoles()` + `pkg/seaplane/roles.go` internals with `iam/`; keep behavior identical | rosa                      |
| D3  | TF provider: new `rhcs_seaplane_operator_roles` resource + datasource built on `iam/`                              | terraform-provider-rhcs   |
| D4  | CI generation of CFN template + JSON definition export from `iam/definitions.go`                                   | rosa-seaplane-api         |
| D5  | Remote overlay: schema, S3 publishing pipeline, signing/version checks, client `FetchLatest`                       | rosa-seaplane-api + infra |
| D6  | `rosa upgrade operator-roles` (seaplane mode) reconciliation using definition versions                             | rosa                      |
| D7  | V1→V2 migration adapter (`FromV1RoleSet`) + e2e coverage of shared-prefix scenarios                                | rosa-seaplane-api / rosa  |

Suggested order: D1 → D2 (unblocks everything, removes duplication) → D3 → D4 →
D5/D6 → D7. D1+D2 is a pure refactor with no behavioral change and can ship first.

## Open questions for reviewers

1. Where should the overlay document live (which S3 bucket/CDN, who owns rotation),
   and do we require cryptographic signing or is version monotonicity + TLS enough?
2. Should the Platform API eventually serve the definition set (Option A) for non-Go
   consumers, and if so is generating that endpoint from `iam/` acceptable?
3. For V1→V2 in-place migration, do we prefer reusing V1-named roles via the adapter,
   or mandating a new V2 prefix (cleaner but creates a second role set in the
   customer account)?
4. Do we need per-partition AWS-managed policy ARN variation beyond the partition
   prefix (e.g. China/GovCloud policy availability)?
5. Should `definition_version` applied to a cluster be recorded in the Cluster CR
   (envelope field) so the operator can detect drift?
