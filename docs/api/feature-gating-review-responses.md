# Peer Review Responses: Feature Gating Design

**PR**: [rosa-seaplane-api#593](https://github.com/openshift-online/rosa-seaplane-api/pull/593)
**Reviewers**: robpblake, gdbranco, coderabbitai
**Date**: 2026-10-15

This document summarizes every review comment on the feature-gating design, our assessment
of its validity, the agreed/adopted resolution, and where the resolution landed in
[feature-gating-design.md](feature-gating-design.md).

---

## 1. robpblake — Use a customer-facing example (OCPSTRAT-3600)

> Can we use a customer-facing example: OCPSTRAT-3600 is a good one… There are cluster
> autoscaler features that will only be available in OCP 5.1. How do we communicate to
> customers that try to use those on OCP < 5.1 that they cannot, or it has no impact.
> One option is that we don't and it's just silently ignored. Bad UX, but an option.

**Summary**: The design lacks a concrete customer-facing example. OCPSTRAT-3600 (Cluster
Autoscaler configuration parity for HCP) is a real case: fields that only exist in a future
OCP payload. How does a customer on an older version find out a field is unusable? Silent
ignoring is possible but bad UX.

**Assessment**: Valid and important. Silent ignoring is exactly the failure mode this whole
design exists to prevent — the CPO in the hosted cluster drops unknown fields silently, so
without platform-API rejection the customer's configuration would be *accepted and then
discarded*, which is the worst outcome (no error, no effect).

**Resolution (adopted)**:
- Added OCPSTRAT-3600's ClusterAutoscaler fields as the worked customer-facing example in
  [§3.4](feature-gating-design.md#34-runtime-plumbing-validationcontext): a customer on
  OCP 4.19 setting a field that requires OCP 5.1 gets an HTTP 400 with an actionable
  message ("requires an OpenShift 5.1 or later cluster"), not a silent drop.
- The discovery API (§3.10) gives customers a way to ask *before* attempting a write:
  `GET /api/feature_gates?cluster_version=4.19` returns which capabilities are available
  and why others are not.

**Answer**: Agreed — we reject with a version-specific error rather than silently ignoring,
and the design now documents both the rejection path and the discovery path using
ClusterAutoscaler as the example.

---

## 2. robpblake — Should Hypershift own the version gating?

> Should Hypershift own the version gating rather than it being duplicated in Hyperfleet?
> IIUC, the `HostedCluster` spec is OCP version agnostic and essentially the CPO of the HCP
> will ignore any fields that it doesn't understand… additions to the `HostedCluster` API
> should include "this only works with HCPs at OCP version 5.1+" markers rather than it
> being maintained in Hyperfleet… The common trade-off here between drift in Hyperfleet and
> Hypershift and having two sources of truth, but I think that is more aligned with
> Hypershift owning the pass-through part of the API.

(Second comment on the same line links the internal Slack thread where this was discussed.)

**Summary**: Since the gated capabilities are implemented in the Hypershift/OCP payload and
the HostedCluster spec is version-agnostic, version constraints arguably belong *in
Hypershift* (as markers on the passthrough fields) rather than duplicated in Hyperfleet.
Duplicating creates two sources of truth and drift risk.

**Assessment**: Valid concern — drift is the main risk of our approach. However, moving
gating entirely to Hypershift does not solve the customer problem on its own:

1. **The CPO silently drops unknown fields.** Hypershift-side markers would at best
   annotate the API; without a validation layer, an old HCP still silently ignores a field
   set by a newer client. Someone must *enforce*, and the only component that sees the
   customer's request, the account entitlements, and the cluster's OCP version together is
   the Hyperfleet platform API.
2. **Entitlements are Hyperfleet's dimension.** Account feature-set / per-customer
   enablement (e.g. the Karpenter rollout) has no representation in a Hypershift marker;
   it only exists in our platform.
3. **Timing.** We cannot block the Hyperfleet gating story on upstream Hypershift marker
   adoption.

**Resolution (adopted)**:
- Added [§2.5](feature-gating-design.md#25-should-hypershift-own-version-gating) to the
  design documenting this trade-off explicitly.
- Positioned Hypershift markers as the *preferred long-term input* to our registry: when
  Hypershift annotates passthrough fields with version constraints ("works with HCPs at
  OCP 5.1+"), we generate our enablement conditions from those markers instead of
  hand-curating them, eliminating the second source of truth. Until then the registry is
  Hyperfleet-maintained data, and drift is mitigated by the reviewable gate matrix (§3.3)
  and by aligning with the Hypershift team on graduation changes.

**Answer**: We keep enforcement in Hyperfleet (it is the only place with all three inputs:
request, entitlement, OCP version), but we agree the *version data* should originate in
Hypershift. The design now states that Hypershift version markers on HostedCluster
passthrough fields are the target source for our registry, and we'll raise the marker
addition with the Hypershift team (see the Slack thread referenced in the review).

---

## 3. gdbranco — Z-patch (z-stream) version granularity

> we might need z patch since some features are enabled within z versions

**Summary**: Some features graduate within a z-stream (e.g. backported to 4.19.4), so
major.minor granularity is insufficient.

**Assessment**: Valid — OCP itself occasionally enables features in a z-stream (e.g. fixes
backported into a specific patch release). Restricting to major.minor would force us to
gate a feature on an entire minor it doesn't belong to.

**Resolution (adopted)**:
- `VersionRange.Min`/`Max` now accept an optional patch component (`"4.19.4"`). A bound
  with a patch is compared at patch granularity; a bound without one (`"4.19"`) matches the
  whole minor, preserving the common case and OCP parity. See
  [§3.2](feature-gating-design.md#32-version-aware-gate-registry-the-core-change).
- Open question 3 updated: recommendation is now "minor granularity by default, optional
  z-stream bounds where a feature genuinely graduates mid-z".

**Answer**: Agreed; the registry supports optional z-stream bounds in `VersionRange`, used
only when a feature actually lands in a z-stream.

---

## 4. robpblake — +1 to conservative update-time version source

> +1 to conservative e.g. this feature is only available once the HCP is on the target
> version. I think the "we need to change the configuration of a cluster as part of an
> upgrade" is a separate concern.

**Summary**: Reviewer endorses using the *running* version (`status.version`) at update
time, treating "configure for the version being upgraded to" as a separate feature.

**Assessment**: Valid — this resolves open question 2.

**Resolution (adopted)**: Open question 2 is closed: the running version wins; the escape
hatch ("pre-configure fields that only take effect after the upgrade completes") is
explicitly out of scope and would be a separate design if ever needed.

**Answer**: Agreed and recorded — decision: running version, conservative; upgrade-time
pre-configuration is a separate concern.

---

## 5. robpblake — Which CRD is being variant-ed?

> Can you explain a bit more about this. Which CRD are you talking about in terms of
> variants?

**Summary**: The CRD-variants section (§3.8 / OCP's per-feature-set CRD variants) does not
say which CRDs the variants would be generated for.

**Assessment**: Valid clarification request — ambiguity between the *HostedCluster* CRD
(upstream, Hypershift-owned) and *Hyperfleet's own* CRDs.

**Resolution (adopted)**: §3.8 now states explicitly: the variants are for **Hyperfleet's
own CRDs** — the `Cluster` and `NodePool` CRs (generated from `api/v1alpha1/`) that are
served in the regional management clusters and consumed by the hyperfleet-operator. The
upstream HostedCluster CRD is never variant-ed by us; Hypershift owns it.

**Answer**: The Hyperfleet Cluster/NodePool CRDs served in the regional clusters — not the
HostedCluster CRD. §3.8 was updated to say this.

---

## 6. robpblake — +1 deployment-versioned; utility of a discovery / "do I have access" API

> +1 to deployment-versioned. I also see utility in an API that explains which gates are
> available within a deployment and a "do I have access to this feature" API for
> customers. Using Karpenter as an example: limited to specific customers on specific
> versions; self-serve "Am I able to use this and how do I request access" would be useful;
> allowing the feature gate to point to specific instructions (KCS, T&Cs). Possibly beyond
> the scope of the work here?

**Summary**: Endorses deployment-versioned gate data (open question 9) and asks for a
customer-facing discovery API: which gates exist in this deployment, am I entitled, how do
I request access, and per-gate documentation/T&C links.

**Assessment**: Valid, and directly in scope — §3.9 already sketched a per-region discovery
endpoint; the reviewer's Karpenter example strengthens the case and adds two requirements:
per-gate documentation/access-request links, and entitlement-awareness in the response.

**Resolution (adopted)**:
- Open question 9's first half is closed: gate data stays **deployment-versioned**; the
  discovery endpoint becomes part of this work rather than an optional Phase 4 item.
- Added [§3.10](feature-gating-design.md#310-customer-facing-discovery-api) specifying the
  discovery API: request/response shape, entitlement-aware results, and two new registry
  metadata fields (`DocsURL`, `RequestAccessURL`) per gate, plus an optional
  `TermsConditionsURL`. The Karpenter scenario is the worked example.
- Migration plan: discovery endpoint moved from "Phase 4 optional" to Phase 2 (with the
  registry that feeds it).

**Answer**: Agreed on deployment-versioned. The discovery API is in scope: `GET
/api/feature_gates` (optionally `?cluster_version=`) returns each gate with
enabled/disabled + reason for *the caller's account*, and the registry now carries
per-gate `DocsURL` / `RequestAccessURL` so customers can self-serve the Karpenter-style
"can I use this / how do I get it" question.

---

## 7. coderabbit — Account overrides must not bypass version support

> OR'ing `EnabledGates` with registry-derived gates lets an account enable a gate even when
> no version condition matches… Apply an account override only after the registry confirms
> version compatibility.

**Summary**: §3.5 proposed OR-ing per-account `EnabledGates` with registry-derived gates,
which lets an entitlement override enable a gate on an OCP version (or unknown version)
whose payload doesn't support it — breaking fail-closed.

**Assessment**: Valid — a real correctness bug in the proposed composition. An override
should widen *account* eligibility, never the *version* dimension.

**Resolution (adopted)**: §3.5 step 3 rewritten: overrides are intersected with version
support, not unioned. `IsGateEnabled` remains the sole version authority; an account
override may only (a) add a feature set to a condition that already matches the cluster's
version, or (b) enable a gate whose conditions are version-unconstrained. Concretely, the
resolver evaluates version first and only then consults overrides for the feature-set
dimension. Open question 8 updated accordingly.

**Answer**: Correct — fixed in the design: overrides now apply only to account/feature-set
eligibility after the registry confirms version compatibility; version conditions can
never be bypassed, and unknown versions remain fail-closed.

---

## 8. coderabbit — Unknown OCP version vs `Versions: nil` contradiction

> `Versions: nil` means "all versions," but this rule says an unknown version disables
> every gate. For `ocpVersion == ""`, these requirements conflict… Specify whether
> fail-closed applies only to version-constrained gates, or revise the compatibility claim.

**Summary**: §3.2 said both "gates with `Versions: nil` are fully backward compatible" and
"unknown/empty version fails closed", which contradict each other for a nil-versioned gate
on a cluster with unknown version.

**Assessment**: Valid — the spec was ambiguous about the `ocpVersion == ""` ×
`Versions == nil` cell.

**Resolution (adopted)**: §3.2 semantics and the worked-example table now state the rule
explicitly: an empty `ocpVersion` matches **only** conditions whose `Versions` is nil.
Version-unconstrained gates keep today's behavior (enabled per feature set even when the
version is unknown — backward compatible); version-constrained gates fail closed on
unknown version. `IsGateEnabled`'s doc comment and the fail-closed test list in Part 5
were updated to match.

**Answer**: Fail-closed on unknown version applies only to version-constrained gates;
`Versions: nil` conditions are treated as version-unconstrained and unaffected. This is
now stated explicitly in §3.2.

---

## 9. coderabbit — Typo in the Jira key (`ROSEAENG-61570`)

**Summary**: Three references use `ROSEAENG-61570` instead of `ROSAENG-61570`.

**Assessment**: Valid typo.

**Resolution (adopted)**: All three occurrences corrected to `ROSAENG-61570`.

**Answer**: Fixed.

---

## Summary of design-doc changes driven by this review

| # | Comment | Change |
| --- | --- | --- |
| 1 | Customer-facing example | OCPSTRAT-3600 autoscaler example in §3.4; discovery API §3.10 |
| 2 | Hypershift ownership | New §2.5 discussing ownership, markers-as-input strategy |
| 3 | Z-patch granularity | `VersionRange` supports optional patch bounds; OQ3 updated |
| 4 | Conservative update version | OQ2 closed (running version wins) |
| 5 | Which CRD | §3.8 clarified: Hyperfleet Cluster/NodePool CRDs |
| 6 | Discovery API | New §3.10 with request/response; `DocsURL`/`RequestAccessURL` metadata; endpoint moved to Phase 2; OQ9 first half closed |
| 7 | Override bypass | §3.5 rewritten: overrides intersect version support; OQ8 updated |
| 8 | Unknown-version rule | §3.2: empty version matches only nil-version conditions |
| 9 | Jira typo | ROSAENG-61570 ×3 |
