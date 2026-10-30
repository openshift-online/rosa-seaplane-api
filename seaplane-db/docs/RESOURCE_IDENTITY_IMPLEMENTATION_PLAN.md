# Seaplane Resource Identity and Lifecycle Implementation Plan

**Status:** Active target and implementation roadmap
**Scope:** `seaplane-db`, `seaplane-api`, `seaplane-operator`, `api`, and `clientset`

This plan brings every component that writes or reconciles Seaplane resources into
one identity and ownership model. It records the target contract, the current
implementation baseline, the changes needed in dependency order, and the tests that
demonstrate the target behavior. It is kept with seaplane-db because PR 1 established
the database primitives that later identity phases rely on; PR 2 is coordinated across
the API, operator, and clientset as well as seaplane-db.

## Guidelines at a glance

How far the code follows each rule in
[seaplane-db-guidelines.md](../../docs/seaplane-db-guidelines.md). The phase that
closes each gap is listed under [Remaining work](#remaining-work).

| Guideline rule                     | Status  | Implemented                                                                                                                                                         | Missing                                                                                                                                                                                          |
| ---------------------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1. Namespace is the account        | Partial | Cluster, NodePool, OidcConfig, and DNSReservation live in `account-<id>`; a different namespace returns 400; `Index` scopes are server-only.                        | An empty request namespace is filled in instead of rejected (Phase 6).                                                                                                                           |
| 2. Name is for humans              | Done    | Client-chosen, immutable names validated as DNS labels; children are `<cluster>.<child>`; the API never rewrites them.                                              | SDK helper that builds `<cluster>.<child>` (Phase 4).                                                                                                                                            |
| 3. UID is for machines             | Partial | seaplane-db mints UIDs; Cluster references to OidcConfig and DNSReservation, ownerReferences, and DNS/OIDC claims store UIDs.                                       | SDK waiters don't check UIDs (Phase 4). The Spot termination queue claim is held by the Cluster name; the DNSReservation controller finds its Cluster by listing the namespace (Phase 6).        |
| 4. Labels belong to the platform   | Partial | Client `seaplane.io/*` labels are discarded; NodePool and Placement carry an ownerReference and `cluster-uid`; claims use `owner-uid` and `claimed-by-cluster-uid`. | Customer labels outside `seaplane.io/` are stored; annotations aren't validated; Cedar reads labels; the storage client sets the namespace and account label instead of checking them (Phase 6). |
| 5. Every controller cleans up      | Partial | The Cluster finalizer deletes its children by `cluster-uid`, waits for them, and releases its DNS and OIDC claims; DNSReservation releases its own Indexes.         | No generic collector for orphaned children (Phase 3). No finalizer releases the Spot queue claim; it is reclaimed after a timeout (Phase 6).                                                     |
| 6. Uniqueness from name or `Index` | Partial | Duplicate Create returns `AlreadyExists` (409); DNS prefixes and OIDC issuer URLs are `Index` claims held by a UID.                                                 | The Spot queue `Index` is held by a name (Phase 6).                                                                                                                                              |
| 7. Business rules in code          | Done    | The only schema addition is the non-unique GIN labels index; architecture docs describe optimistic per-object writes.                                                  | None.                                                                                                                                                                                            |
| 8. Write what you checked          | Partial | Updates re-read, re-validate, and write the read snapshot on each retry; DNSReservation and OidcConfig deletes write the checked snapshot.                          | Cluster and NodePool deletes re-read by name in the storage client (Phase 6).                                                                                                                    |
| 9. Reconcilers are idempotent      | Partial | DNSReservation allocation recovers its prefix on retry and removes duplicate Indexes after overlapping reconciles. Sharding uses namespace plus `seaplane.io/cluster-uid`, falling back to namespace plus row UID. | The rest of the operator hasn't been reviewed against this rule (Phase 7).                                                                                                                        |

Guidelines §4 also says not to re-check parents on reads, and recommends batching
instead of calling `Get` inside loops. Read paths haven't been reviewed against
either yet (Phase 7). List endpoints still paginate in memory (Phase 4).

## Remaining work

Phases 1, 2, and 5 are implemented. Phase 5 closes the sharding and documentation
work only; it does not complete the identity roadmap. Phases 3, 4, 6, and 7 remain
future work. Phases 3–5 are detailed in [§5](#5-implementation-phases); phases 6
and 7 are only listed here until they are designed.

**Phase 3 — Garbage collection**

- Add a collector that deletes NodePools and Placements whose Cluster is missing,
  recreated, or deleting.
- The Cluster finalizer waits for its children instead of deleting them itself.

**Phase 4 — Public selectors and SDK**

- Label selectors on the public API and clientset, matching platform labels only.
  Land them after Phase 6 stops storing customer labels.
- Filter and paginate list endpoints in the database, not in memory.
- SDK helper that builds `<cluster>.<child>`.
- SDK waiters treat a changed UID as the object being gone.

**Phase 5 — Cluster-UID sharding and docs (implemented)**

- Shard by namespace plus `seaplane.io/cluster-uid` when present; otherwise
  fall back to namespace plus the row's database UID. A Cluster uses its own UID,
  and owned children use the same UID from their ownership label.
- Remove fenced writes and stored procedures from `architecture.md` and
  `sharding.md`.

**Phase 6 — Metadata, namespaces, and checked writes**

- Store no customer labels; customer metadata goes in validated annotations, with
  `seaplane.io/` reserved. Cedar and `docs/authz.md` read annotations.
- Reject an empty request namespace with 400.
- The storage client checks the namespace and account label instead of setting them.
- Cluster and NodePool deletes write the snapshot the handler checked.
- Claim the Spot termination queue after the Cluster is created, held by the Cluster
  UID, and release it in the Cluster finalizer. Remove the timeout reclaim.
- The DNSReservation controller finds its Cluster by UID.

**Phase 7 — Guideline review**

- Remove parent re-checks from read paths; skip children whose parent can't be
  resolved.
- Batch `Get`s inside loops over lists that can grow with the account.
- Check that reconcilers are safe to run twice and make no cross-cluster decisions
  from local counts.

## 1. Target contract

1. **Customer account namespace rule.** Customer-created resources and resources
   tied to them in the customer-facing API/seaplane-db use `account-<accountID>`. The
   account ID comes from the authenticated caller, never from an untrusted body
   value. Independent operator-internal resources, such as `ManagementCluster`, may
   use operator-controlled namespaces; internal namespaces used for `Index` claims
   are operator-only.
2. **Name identifies the human-facing resource.** The client chooses it and it is
   immutable. A Cluster name is one DNS label of at most 63 characters. A child name
   is `<cluster>.<child>`; each part is a DNS label of at most 63 characters. The API
   validates the full name but does not rewrite it.
3. **UID identifies the object incarnation.** seaplane-db mints the UID. Clients cannot
   choose or change it. Delete and recreate at the same name produces a new UID.
4. **References identify UIDs.** When a name is used as a lookup hint, compare the
   fetched object's UID with the stored expected UID before acting on it.
5. **Owned children have an ownerReference and a
   `seaplane.io/cluster-uid` label.** Both are set at create from the same parent.
   Claims use a holder UID label such as `seaplane.io/owner-uid` on Indexes or
   `seaplane.io/claimed-by-cluster-uid` on a claim held by a Cluster.
6. **Cleanup is explicit.** A generic garbage collector deletes owned children with
   a missing, replaced, or deleting owner. The owner's finalizer waits for children
   to disappear and releases its claims. A child's finalizer must not wait forever
   for a missing owner.
7. **Uniqueness uses existing primitives.** The primary key
   `(kind, namespace, name)` handles per-account name uniqueness. An `Index` create
   claims a value in a wider scope. Do not add business-specific database constraints.
8. **Business rules stay in code.** seaplane-db provides object storage, selectors,
   versions, and atomic single-row writes, but no admission or multi-object
   transactions. Reconcilers converge through retries and are safe to run more than
   once.
9. **Sharding follows the owning Cluster UID within the account namespace.** Cache
   List/Watch work is keyed by namespace plus `seaplane.io/cluster-uid` when present
   and otherwise by namespace plus the row's database UID. The Cluster has no
   owner-UID label and therefore hashes by its namespace and own UID; its children
   carry that UID label and share its shard. Claim labels do not change an
   account-level row's key. Reads through the direct client remain unsharded.

On the management cluster, Cluster resources use namespace `cluster-<cluster UID>`.
New Clusters persist the UID-derived `spec.hostedClusterName`, sized so the
HyperShift-generated `<namespace>-<name>` control-plane namespace remains a DNS
label. Legacy Clusters without that service-set field retain their existing
customer name. NodePool resources use the child portion of the stored name
(`workers`) and the same internal HostedCluster name in their HyperShift reference.

### DNS pre-reservation contract

Customers must be able to reserve a base domain **before** creating a Cluster. A
customer-created DNSReservation is therefore a real account-scoped resource and
claim, not an internal mirror that can be deleted from the product flow:

- A customer creates a DNSReservation in their account namespace and receives its
  database UID. The operator allocates a unique prefix by creating an internal
  `Index` whose `owner-uid` is the DNSReservation UID. The reservation exposes the
  resulting base domain in status. Customers can use it to prepare shared VPC or
  other DNS-dependent configuration before Cluster creation.
- `status.phase` is `Pending` while allocation or recovery is in progress, and
  `Ready` only after an Index owned by the reservation UID exists and
  `status.baseDomain` is assigned. Prefix collisions are retried. If the bounded
  candidate attempts are exhausted, the phase stays `Pending`, `baseDomain` stays
  empty, and a `Ready=False` condition reports the allocation failure; the
  reservation cannot be claimed. Transient database and status-write failures are
  retried without turning into a terminal resource state. A later successful
  allocation clears the condition and transitions the reservation to `Ready`.
- The DNS Index holder is the DNSReservation UID, not the Cluster UID: the claim
  exists before a Cluster does, and its holder remains stable when the reservation
  is claimed. `claimed-by-cluster-uid` records which Cluster currently consumes it.
- Cluster creation references the reservation by UID. seaplane-api validates
  that it is ready, belongs to the caller's account, and is not already claimed.
  Once the database returns the new Cluster UID, the API claims the reservation with
  `claimed-by-cluster-uid`; persist the reservation UID, not its name, in the
  Cluster's internal reference. The operator must not provision the Cluster until
  that claim is confirmed.
- The Cluster copies the assigned base domain into `Cluster.status.baseDomain` for
  rendering. The DNSReservation remains the claim holder; its Index is not exposed
  to customers.
- Customer deletion of a claimed reservation returns a conflict. On Cluster
  deletion, the Cluster finalizer deletes the reservation by UID and waits for it to
  disappear; the reservation's finalizer deletes only Indexes labelled with its own
  UID. An unclaimed reservation can be explicitly deleted by its customer.

DNSReservation is a claim resource, not an ownerReference child of the Cluster.
Its `claimed-by-cluster-uid` binding label does not change its sharding key;
without `cluster-uid`, it remains keyed by its own database UID within the account
namespace. The generic garbage collector initially applies to Cluster-owned NodePool
and Placement objects.

## 2. Current implementation status after PR 1

| Area                  | Current implementation                                                                                                                                                                                                                                                                                                                  | Remaining gap to the target                                                                                                                                                                                   |
| --------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| seaplane-db UID       | UID is database-generated and returned by Create; client-sent UIDs are ignored; tombstone revival assigns a fresh UID. Controller-runtime updates, status writes, and deletes use UID plus resourceVersion preconditions.                                                                                                               | seaplane-api conversions still replace the seaplane-db UID in public Cluster, NodePool, and OidcConfig responses.                                                                                             |
| seaplane-db selectors | `metadata.name`, `metadata.namespace`, and `metadata.uid` field selectors map to columns. Label selectors are translated to SQL before pagination, with a GIN index on labels. Equality, set, existence, negative, and numeric range selectors are supported; invalid/missing/out-of-int64 label values do not match numeric selectors. | Public API and clientset do not yet expose label selectors.                                                                                                                                                   |
| Create semantics      | Duplicate Create returns `AlreadyExists`, even for identical content. Content-equal updates are suppressed only when UID and resourceVersion are current; stale versions or UIDs conflict.                                                                                                                                              | API-level idempotency keys for safely replaying POSTs after a lost response are deferred future work; see §4.                                                                                                 |
| Platform identity     | Cluster names are client-facing; OidcConfig storage already uses an account namespace.                                                                                                                                                                                                                                                  | Cluster and NodePool storage uses `cluster-<generated UUID>`. Cluster, NodePool, and OidcConfig responses override database UIDs. Namespace and DNS-label validation is incomplete.                           |
| Protected metadata    | seaplane-db stores and returns labels and ownerReferences. seaplane-api update handlers fetch the stored object and merge spec, preserving its metadata.                                                                                                                                                                                | Create conversions copy request ObjectMeta; reserved labels and ownerReferences are not consistently stripped or set by the server.                                                                           |
| Operator ownership    | Cluster, NodePool, and OidcConfig finalizers exist.                                                                                                                                                                                                                                                                                     | No generic garbage collector or Cluster ownerReferences/UID labels are set. NodePool parent lookup lists by namespace and selects the first Cluster. Cluster deletion manually deletes children by namespace. |
| DNS/OIDC claims       | `Index` uses seaplane-db's primary-key uniqueness; issuer URLs are normalized and hashed. The operator has an internal DNSReservation type.                                                                                                                                                                                             | DNSReservation is currently created by the Cluster reconciler after the Index claim; there is no customer-facing reservation API. DNS and OIDC claim ownership uses namespace/name labels rather than UIDs.   |
| Clientset             | SigV4 signing and polling waiters exist.                                                                                                                                                                                                                                                                                                | Namespace paths currently influence account headers and NodePool `clusterId`; updates route by UID; waiters do not treat a changed UID as gone.                                                               |
| Sharding/docs         | List/Watch sharding and unsharded GVKs are implemented. Rows use namespace plus `cluster-uid` when present and otherwise namespace plus their own UID; direct reads are unsharded.                                                                                                   | None for the Phase 5 sharding target.                                                                                                                                                                      |

The conversion layer already projects ObjectMeta on reads, so returning labels and
ownerReferences is partly supported once the correct metadata is stored. The required
work is to set and protect those fields consistently, add tests for the public
behavior, and expose selectors on list endpoints.

## 3. Cutover assumption: recreate the whole environment

This plan assumes the code changes are completed and verified first, then the **whole
environment is recreated** using the new modules. The new environment starts with an
empty seaplane-db and no active external state from the old deployment. This is the chosen
cutover path; this plan does not include an in-place row migration, data backfill,
dual-read period, or compatibility shim.

Recreate the external state along with seaplane-db: DynamoDB Apply/Read desires,
management-cluster resources and namespaces, and any old DNS reservations/Indexes.
Recreating only the database would leave old external resources without the records
the old operator used to manage them.

Provision the new environment only after the target schema, generated CRDs/OpenAPI,
API, operator, clientset, and DNS reservation flow have passed the acceptance and
verification gates in §§6–7.

## 4. Decisions to settle before the identity switch

These choices are settled for the PR 2 identity switch. They are recorded here to
keep the cross-component implementation aligned, not as reasons to add database
constraints:

- **OidcConfig identity:** clients choose `metadata.name`; seaplane-db UID is the machine
  identity. Cluster references and claim ownership use the UID, while the name remains
  the API lookup value.
- **OIDC claim ordering:** create the Cluster first, then claim the OidcConfig using
  the seaplane-db-minted Cluster UID. Roll back the Cluster on claim conflict, and gate
  operator provisioning until the claim is confirmed.
- **Managed issuer URL:** retain a separate server-generated issuer path identifier.
  It is independent of both the client-selected OidcConfig name and the seaplane-db UID,
  preserving the managed issuer URL path without substituting object identity.
- **Stored references:** `Cluster.Spec.OidcConfigID` and
  `Cluster.Spec.DNSReservationID` store UIDs. Placement names and
  `Placement.Spec.ClusterName` remain lookup/display hints; ownerReferences and
  `cluster-uid` labels carry machine ownership. Remove `IndexRef` from both
  DNSReservation and OidcConfig: DNS Indexes are recovered and cleaned up by
  `owner-uid`, while an OIDC Index name is recomputed from the normalized issuer URL
  and ownership is checked by OidcConfig UID.
- **DNS pre-reservation API:** customers create/get/list/delete reservations by
  `metadata.name`; create needs no customer-selected spec fields. The operator uses
  the regional default DNS zone and shard `0` and reports the assigned domain in
  status. Unclaimed reservations stay reserved until explicit deletion. Cluster
  create may bind a pre-created reservation by UID; when omitted, the Cluster
  reconciler creates and claims a DNSReservation automatically and stores its UID on
  the Cluster, preserving the legacy on-demand allocation flow.
- **DNS reservation identity and claim:** Cluster references the reservation by UID;
  the reservation's Index uses `owner-uid=<DNSReservation UID>`, and the mutable
  reservation claim uses `claimed-by-cluster-uid=<Cluster UID>`. On Cluster deletion,
  delete the reservation; its finalizer releases only its own Indexes.
- **DNS retry behavior:** after creating an Index but before updating reservation
  status, a retry must find an Index already carrying the reservation UID and recover
  its prefix. Handle overlapping reconciles so one reservation does not retain
  multiple DNS claims.
- **Create/no-op contract:** preserve no-op suppression for content-equal updates,
  status writes, and desired-state reapplication. Do not let it turn a duplicate
  Create into success; update `DESIGN.md` and its tests to match the target contract.
- **Future API idempotency keys:** an optional key may make client POST retries safe
  after a lost response. Design this separately, including account/operation scope,
  request-digest checks, replayed responses, and retention. It does not change
  seaplane-db Create semantics: a duplicate name still returns `AlreadyExists`.
- **Labels index:** add a non-unique GIN index for the JSON labels selector path.
  This is an additive index, not a business uniqueness constraint. Define its schema
  installation alongside the seaplane-db migration work.

## 5. Implementation phases

### Phase 1 — seaplane-db query and identity correctness (PR 1 — implemented)

**Primary files:** `seaplane-db/pgclient.go`, `pgcache.go`, `fieldselector.go`,
`internal/reader/list.go`, `internal/schema/`, and `internal/writer/`.

Actions:

- Translate supported Kubernetes label-selector operators into parameterized SQL.
  Use predicates compatible with a GIN index on `metadata->'labels'`; do not silently
  drop unsupported selector requirements.
- Apply label filters in SQL before limit/offset for both direct and cache List.
- Map `metadata.uid` to `uid` (compare a textual UUID value safely); keep name and
  namespace mapped to their columns.
- Add the non-unique GIN labels index. No business-specific uniqueness constraints
  or object-specific database paths are introduced.
- Change Create conflict behavior so an existing live/dying name returns
  `AlreadyExists`, even for identical content.
- Add expected-UID checks for writes from an existing object so an old incarnation
  cannot mutate a recreated object with the same name and object version.

Tests:

- Client-sent UID is ignored; Create returns database UID; delete/recreate returns a
  different UID; duplicate Create returns `AlreadyExists`.
- `metadata.uid` field selectors work against the UID column.
- Label selectors, including set/existence/negative and numeric comparisons, match
  the expected rows in SQL and paginate without skipped matches. Numeric selectors
  do not match missing, non-integer, or out-of-int64-range label values.
- A stale UID cannot update, status-update, or delete a same-name replacement.

### Phase 2 — Identity and customer DNS reservation switch (PR 2)

This phase is coordinated across `api`, `seaplane-api`, `seaplane-operator`, and
`clientset`; do not deploy only part of it.

**API types and OpenAPI**

- Update comments and generated validation markers for account namespaces, Cluster
  names, child-name parts, and the public DNSReservation API. Regenerate public types
  and OpenAPI and verify generated output.
- Keep seaplane-db's lack of schema/admission validation explicit: enforce request rules
  in seaplane-api.

**seaplane-api and seaplane-db wrapper**

- Store customer-facing Cluster, NodePool, OidcConfig, and DNSReservation resources,
  and resources tied to them, in the authenticated account namespace. Require
  customer-scoped request namespaces to match `account-<caller account>`; this rule
  does not constrain independent operator-internal resources.
- Add customer-facing DNSReservation create/get/list/delete operations. A customer
  creates a reservation before its Cluster and waits for the assigned base domain;
  the API never exposes the internal Index namespace or object.
- Remove application-generated Cluster IDs and conversion UID overrides. Return
  seaplane-db UIDs without substitution.
- Route Cluster get/update/delete by name. Use the account namespace and name for
  storage lookups.
- Remove Cluster list-before-create uniqueness as the enforcement point and map the
  database's duplicate-name error to 409.
- Validate Cluster and child names. Child create parses `<cluster>.<child>`, fetches
  the Cluster by account namespace/name, rejects a deleting parent, and sets the
  ownerReference and `cluster-uid` label from that fetched Cluster UID. The Cluster
  name prefix is the parent lookup hint; NodePool has no `spec.clusterId` field.
- Cluster create accepts a DNSReservation lookup value, resolves it to the
  reservation UID, creates the Cluster, then claims the reservation using the new
  Cluster UID. On claim conflict, roll back/mark the new Cluster and return a
  conflict. Gate Cluster reconciliation until the claim is confirmed. Once confirmed,
  the Cluster reconciler copies the reservation's ready base domain into Cluster
  status for rendering.
- Ignore client-supplied UIDs, ownerReferences, and protected `seaplane.io/*`
  labels. Preserve stored protected metadata on update.
- Change `clusterId` NodePool list filtering to `seaplane.io/cluster-uid`; use the
  account namespace for account scoping.
- Retry Cluster/NodePool optimistic updates by refetching and reapplying the partial
  spec update.

**DNSReservation and Index claim lifecycle**

- Create the DNSReservation row first so seaplane-db assigns its UID. A reservation
  reconciler creates the Index in the internal DNS shard namespace with
  `owner-uid=<DNSReservation UID>`, then stores the assigned domain in reservation
  status.
- Make the reconciliation idempotent: on retry, list Indexes by reservation UID and
  recover an existing prefix before generating another. Use conflict-aware status
  writes and clean up duplicate Indexes owned by the same reservation after
  overlapping reconciles.
- On Cluster create, claim the ready reservation with
  `claimed-by-cluster-uid=<Cluster UID>`, and copy the domain to
  `Cluster.status.baseDomain` for management-cluster rendering.
- Reject customer deletion of a claimed DNSReservation. On Cluster deletion, the
  Cluster finalizer deletes the claimed reservation by its stored UID and waits for
  it to become NotFound. The reservation finalizer releases only Indexes whose
  `owner-uid` matches its own database UID. An unclaimed reservation can be
  explicitly deleted by its customer.
- Treat DNSReservation as a claim resource, not a Cluster-owned child: it remains
  sharded by its namespace and own UID before and after claim, and is cleaned up
  through the Cluster's stored reservation UID. Do not change its shard key by adding
  `cluster-uid` at bind time.
- Remove the current ClusterReconciler's internal “Index then DNSReservation”
  second-write mirror. Keep the DNSReservation capability as the customer-first API
  workflow described here.

**Operator and rendering**

- Use `cluster-<Cluster UID>` for management-cluster namespaces; stop extracting a
  Cluster ID from the seaplane-db namespace.
- Create Placement with a dotted child name, ownerReference, and `cluster-uid`
  label. Use `Owns()` mappings instead of name-based map functions where ownership
  is the relation.
- Resolve NodePool's parent by ownerReference name and compare UID. Do not list
  Clusters by namespace and choose an arbitrary item.
- List a Cluster's NodePools/Placements by `cluster-uid`, not `InNamespace`.
- Render the child part of the NodePool name on the management cluster.
- Use `owner-uid=<OidcConfig UID>` on the issuer Index and
  `claimed-by-cluster-uid=<Cluster UID>` for the Cluster's OidcConfig claim.
- Keep DNSReservation in the scheme and CRD output as the customer-facing claim
  resource. Neither DNSReservation nor OidcConfig stores `IndexRef`: DNS Indexes are
  recovered by `owner-uid`, and the OIDC Index key is deterministic from the
  normalized issuer URL.

**Clientset**

- Keep `X-Amz-Account-Id` sourced from client configuration/authentication; do not
  derive it from the namespace path. Continue stripping generated namespace URL
  segments if required by the flat platform routes.
- Remove `adaptNodePoolScope`. Expose NodePool as non-namespaced in the typed SDK so
  callers use `NodePools()` without supplying an account namespace; the platform still
  stores the CRD in `account-<accountID>`. Add an explicit Cluster UID list option
  rather than deriving `clusterId` from namespace.
- Add clientset operations for customer DNSReservation create/get/list/delete and a
  waiter for the assigned base domain.
- Change resource routes and updates to use names. Keep the Cluster UID as object
  identity, not as a URL substitute.

Tests:

- A customer can reserve a domain, receive it before Cluster creation, and use that
  reservation when creating a Cluster.
- The same DNSReservation cannot be claimed by two Clusters. Deleting the Cluster
  releases the reservation and its Index; deleting an unclaimed reservation also
  releases its Index. Customer deletion of a claimed reservation returns a conflict.
- Retrying after Index creation but before status update recovers the same domain.
- Two Clusters in one account can each have a NodePool child named `workers`.
- Deleting one Cluster does not alter the other's children, placements, DNS
  reservation, or OIDC claim.
- A recreated same-name Cluster has a new UID and receives none of the old
  Cluster's children or claims.
- Wrong account namespace, invalid DNS labels, client-supplied protected metadata,
  deleting parent, and duplicate names have the specified API responses.

### Phase 3 — Generic garbage collection (PR 3)

**Primary files:** new controller in `seaplane-operator/internal/controller/`,
Cluster/NodePool finalizers, manager registration, tests, and RBAC.

- Add one generic collector for each Cluster-owned kind, initially NodePool and
  Placement. DNSReservation is a claim resource with its own UID and finalizer; the
  Cluster releases it by UID instead of treating it as an ownerReference child.
- For every owned child, fetch the owner named by ownerReference and compare UID.
  Delete the child if the owner is missing, has a different UID, or is deleting.
- Cluster finalizer stops deleting NodePools/Placements directly. It waits for the
  `cluster-uid` label list to become empty, releases its DNS/OIDC claims, then
  removes its finalizer.
- Keep NodePool teardown in the NodePool finalizer. Make the missing-owner behavior
  finite and explicit; retain enough management-cluster information to clean up
  external desires where possible.
- Verify the create/delete race: seaplane-api rejects a parent already deleting;
  a child inserted after the owner finishes is still collected as an orphan.

Tests:

- Child inserted during Cluster deletion is deleted.
- Child referring to an old UID is deleted after the Cluster name is reused.
- Collector never deletes an object owned by a different UID.
- Owner finalizer waits until no child carries its UID and releases only its own
  claims. DNSReservation finalization releases only Indexes carrying its UID.

### Phase 4 — Public selectors and SDK identity behavior (PR 4)

**Primary files:** platform list handlers, clientset list options/wrappers, OpenAPI,
SDK helper tests.

- Parse public `labelSelector` query parameters and pass them through to seaplane-db.
- Expose label selectors and explicit Cluster UID filtering in clientset list
  options. Push filters and pagination into the DB rather than fetching the full
  account list and filtering/paginating in memory.
- Add an SDK helper that builds `<cluster>.<child>` and validates its two parts.
- Waiters get by name and treat NotFound or a changed UID as “the original object
  is gone.” Preserve labels and ownerReferences on public reads; add response tests.
- Update OpenAPI route parameter names/descriptions to describe names, not legacy
  IDs.

### Phase 5 — Cluster-UID sharding and docs (implemented)

**Primary files:** `seaplane-db/internal/reader/shard.go`, shard tests,
`seaplane-db/labels.go`, `api/v1alpha1/labels.go`,
`seaplane-operator/cmd/manager/main.go`, `seaplane-operator/docs/sharding.md`,
architecture docs, and root `CLAUDE.md`.

- Use the same shard key for List and Watch:

  ```sql
  abs(hashtext(namespace || '/' || COALESCE(
      metadata->'labels'->>'seaplane.io/cluster-uid',
      uid::text
  ))::bigint) % $mod = ANY($owned)
  ```

- Update shard unit, integration, and manager tests. Prove that a Cluster row and
  children carrying its UID label share a shard within one account namespace;
  account-level rows without that label shard by their own UIDs within the account
  namespace; direct reads remain unsharded; `UnshardedGVKs` remain visible to every
  replica.
- Reuse `cluster-uid` for ownership, cleanup, selectors, and child shard affinity;
  the Cluster row falls back to its database UID.
- Rewrite sharding docs to explain why Cluster affinity is needed, the
  rescale overlap, no failover while a pod is down, and avoiding cross-Cluster
  decisions from local counts.
- Remove unsupported “fenced writes” claims from `sharding.md` and
  `architecture.md`. Document fixed buckets (for example `Mod=256`) with Postgres
  advisory-lock ownership as a future failover/no-overlap upgrade, not part of this
  implementation.
- Add the target development rules to root `CLAUDE.md` and update the documented
  component/CRD list to describe the customer-facing DNSReservation workflow.

Phases 6 and 7 are listed under [Remaining work](#remaining-work).

## 6. End-to-end acceptance criteria

The identity switch is ready when integration tests can:

1. Create two Clusters in the same account namespace and observe distinct database
   UIDs.
2. Create a DNSReservation before a Cluster, receive its assigned domain, use it to
   prepare shared-VPC configuration, and bind it to one Cluster.
3. Reject a second Cluster attempting to claim that same reservation.
4. Create `cluster-a.workers` and `cluster-b.workers` with ownerReferences and
   `cluster-uid` labels matching their respective Clusters.
5. Confirm both NodePools render as `workers` under their respective
   `cluster-<UID>` management namespaces.
6. List by account, by Cluster UID, and by label selector with correct pagination.
7. Delete one Cluster and verify only its children, DNS reservation, and OIDC claim
   are cleaned up; its DNS Index is released.
8. Recreate the same Cluster name and verify a new UID and no inherited children,
   claims, or waiter state.
9. Exercise the late-child/GC race and same-name/different-UID owner case.
10. Verify reservation retry after Index creation recovers the same domain, and
    deleting an unclaimed reservation releases only its own Index.

## 7. Verification commands

Run focused tests as each phase lands, then verify generated output and modules:

```bash
make test-seaplane-db
make test-api
make test-operator
make test-clientset
make test-operator-int
make verify
make verify-mod
make lint
```

Use `make generate` when API types, CRD manifests, OpenAPI, or clientset generation
inputs change; `make verify` must pass before the identity switch is considered
complete.
