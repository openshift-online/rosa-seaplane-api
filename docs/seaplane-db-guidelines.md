# seaplane-db Development Guidelines

Rules for any code that stores or reconciles Seaplane resources through seaplane-db:
seaplane-api, seaplane-operator, and anything new. New and reviewed code follows them.

## The rules

1. **Namespace is the account.** `account-<id>`. Nothing else is encoded in it.
   Internal namespaces (e.g. `Index` scopes) are server-only; clients can't reach
   them.
2. **Name is for humans.** Chosen by the client and never changed. A child of a
   cluster is named `<cluster>.<child>`.
3. **UID is for machines.** Minted by the database. Every stored reference uses the
   uid, never the name.
4. **Labels belong to the platform.** Customers never write them; customer metadata
   goes in annotations. Owned objects carry an ownerReference and the
   `seaplane.io/cluster-uid` label. Claims carry their holder's uid in a label.
5. **Every controller cleans up what it created.** Its finalizer deletes and releases
   it. Orphans are a cleanup problem, never a read-path one.
6. **Uniqueness comes from the name, or from an `Index` claim.**
7. **Business rules live in code, not in the database.** No new constraints and no
   special query paths.
8. **Write what you checked.** Updates and deletes carry the version of the snapshot
   that was validated and authorized.
9. **Reconcilers are idempotent.** There are no cross-object transactions, so the
   system converges through retries.

---

## 1. What seaplane-db is

A controller-runtime client and cache backed by Postgres. It looks like Kubernetes, but
it is **not an apiserver**. Know the differences:

| You might expect…                                | In seaplane-db                                                                           |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------- |
| admission / CRD schema validation                | **none**. CEL and OpenAPI markers enforce nothing; validate in seaplane-api              |
| a cached, possibly stale `Get`/`List`            | **always a Postgres query**. Never stale, but never free ([§4](#4-references-and-reads)) |
| `Patch`, `DeleteAllOf`, `GenerateName`, `DryRun` | **not supported**. Use `Get` + `Update`, and set names explicitly                        |
| Kubernetes garbage collection                    | **none**. `PropagationPolicy` is rejected ([§6](#6-ownership-and-cleanup))               |
| unique fields beyond the name                    | **only the primary key** `(kind, namespace, name)`. Use an `Index` ([§7](#7-uniqueness)) |
| transactions across objects                      | **none**. Each write is one row; converge in reconcile                                   |

What works as usual:

- **Updates are optimistic.** An update carries the uid and `resourceVersion` it read.
  A stale version, or a uid that changed because the name was reused, gets
  `Conflict`. Status is a separate write (`Status().Update`).
- **Writes that change nothing are skipped.** They don't bump the version or wake
  watchers, so writing desired state every reconcile is cheap.
- **Label selectors** run in SQL on a GIN index. **Field selectors** on
  `metadata.name`, `metadata.namespace`, and `metadata.uid` hit columns. Selectors on
  `spec`/`status` paths scan JSON, so keep them off hot paths.
- **Delete** sets a deletion timestamp. While finalizers remain, the object still
  exists, and creating the same name gets `AlreadyExists`. Once the last finalizer is
  removed, the row is a tombstone, and creating the same name again makes a **new
  object with a new uid**.

## 2. Identity

```yaml
kind: Cluster
metadata:
  namespace: account-123456789012
  name: typeidhcp
  uid: 4610b27e-8f77-4f4c-9661-c11b42e04dec # the cluster ID
  labels:
    seaplane.io/account-id: "123456789012"
---
kind: NodePool
metadata:
  namespace: account-123456789012
  name: typeidhcp.workers
  uid: 9c2f5e1a-1b2c-4d3e-8f90-a1b2c3d4e5f6
  labels:
    seaplane.io/account-id: "123456789012"
    seaplane.io/cluster-uid: 4610b27e-8f77-4f4c-9661-c11b42e04dec
  annotations:
    team: platform # customer metadata
  ownerReferences:
    - apiVersion: seaplane.io/v1alpha1
      kind: Cluster
      name: typeidhcp
      uid: 4610b27e-8f77-4f4c-9661-c11b42e04dec
      controller: true
```

| Field                               | Set by                  | Rule                                                            |
| ----------------------------------- | ----------------------- | --------------------------------------------------------------- |
| `namespace`                         | client                  | required; must be `account-<caller's account>`, else 400        |
| `name`                              | client                  | see [Names](#3-names); unique per kind and namespace; immutable |
| `annotations`                       | client                  | key format, value size, and number of keys are validated        |
| `uid`                               | database                | client value ignored; immutable                                 |
| `ownerReferences`                   | seaplane-api / operator | client value ignored; immutable                                 |
| `labels`                            | seaplane-api / operator | client value ignored; never changed by a customer update        |
| `spec.accountId`, other service-set | seaplane-api            | client value ignored; never change on update                    |

A wrong namespace returns 400, not 403 or 404, so the response tells a caller
nothing about other accounts. The caller's account comes from API Gateway, which sets
`X-Amz-Account-Id` from the SigV4-verified identity. It never comes from the request
body.

**From request to row:**

- **Client-set fields** (namespace, name, spec, annotations) are validated and never
  rewritten.
- **Platform-set fields** (the account label, `spec.accountId`, and other service-set
  fields) are set once, by seaplane-api's conversion step, before authorization.
- **Storage checks, it doesn't fix.** The storage layer verifies ownership fields and
  returns an error on a mismatch. If storage changed the object, what gets written
  would differ from what was authorized, and a missing handler check would stay
  hidden.

### Labels and annotations

**Labels are platform-owned.** seaplane-api and the operator set them. Customers never
write them: not on create, not on update, and not through any other endpoint.

**Why.** Labels are GIN-indexed, and the platform's selectors run on them: ownership
(`cluster-uid`), cleanup, claims (`owner-uid`, `claimed-by-cluster-uid`), the account,
and sharding. Customer keys would grow and churn that index with data nobody selects
on. They would also sit next to the keys that decide ownership, guarded only by a
prefix check: one missed check and a customer moves a resource or releases a claim.

**Customer metadata goes in annotations.** Key format, value size, and number of keys
are validated, and `seaplane.io/` is reserved. Annotations aren't indexed; don't
select on them. A public API may call them labels or tags; they are stored as
annotations.

A claim label changes only when the server claims or releases
([§5](#5-lifecycle-and-writes)).

## 3. Names

| Object                           | Name format         | Example             |
| -------------------------------- | ------------------- | ------------------- |
| Cluster                          | `<cluster>`         | `typeidhcp`         |
| Child of a cluster (NodePool, …) | `<cluster>.<child>` | `typeidhcp.workers` |

- **Each part is a DNS label:** lowercase letters, digits, and `-`, starting and ending
  with a letter or digit, and **no dots**. Each part is at most 63 characters.
  Without a `DNSReservation`, `<cluster>` is at most 18: HyperShift builds the
  control-plane namespace `cluster-<uid>-<cluster>` from it. With one, the internal
  HostedCluster name comes from the reservation's uid instead.
- **The dot makes child names collision-free.** Neither part contains a dot, so
  `cluster-test.np` and `cluster.test-np` stay distinct. With `-` as the joiner, both
  would be `cluster-test-np`.
- **The client sends the full name.** seaplane-api checks it, but never rewrites it.
- **On the management cluster**, a cluster's objects live in its namespace
  `cluster-<uid>` under plain names (`workers`, `pull-secret`).
- `<cluster>.<child>` is the **only** name ever built from other names.

## 4. References and reads

Names can be reused after a delete, but uids can't. Anything that points at another
object stores its **uid**.

| To…                                                      | Use                                                                           |
| -------------------------------------------------------- | ----------------------------------------------------------------------------- |
| record an object's owner                                 | an `ownerReference` **and** the `seaplane.io/cluster-uid` label               |
| list a cluster's objects                                 | label selector on `seaplane.io/cluster-uid` (not `InNamespace`)               |
| react to changes in owned objects                        | `Owns(&Placement{})`                                                          |
| hold an `Index` claim                                    | label `seaplane.io/owner-uid` ([§7](#7-uniqueness))                           |
| claim an account object (`OidcConfig`, `DNSReservation`) | label `seaplane.io/claimed-by-cluster-uid` (set on claim, removed on release) |

The ownerReference and the label are set once, at create, from the same parent, so they
can't drift apart. Both are needed because an ownerReference can't be used in a label
selector or the shard key.

**Lookups:**

- **By name:** `Get` with namespace and name.
- **By uid:** `List` with `client.MatchingFields{"metadata.uid": uid}`.
- **Holding both** (e.g. an ownerReference): get by name, then compare the uid. A
  different uid means the name was reused, and the object you meant is gone.
- **Never key cached state by name alone.** A delete followed by a create can reach a
  watcher as a single "update" whose uid changed.

**Avoid reads inside loops.** Every `Get` is a query, so a `Get` per item makes the
request cost grow with the account. This is a recommendation, not a rule: a loop over
a few items is fine. When the list can grow, such as each node pool's parent cluster,
load the related objects once in a batch and reuse the result. Don't fetch an object
again when the caller already loaded it; pass it down.

```go
var clusters v1alpha1.ClusterList
if err := c.List(ctx, &clusters, client.InNamespace(ns)); err != nil {
	return err
}
byUID := make(map[string]*v1alpha1.Cluster, len(clusters.Items))
for i := range clusters.Items {
	byUID[string(clusters.Items[i].UID)] = &clusters.Items[i]
}
for i := range nodePools.Items {
	parent, ok := byUID[nodePools.Items[i].Labels[clusterUIDLabel]]
	if !ok {
		continue // orphan: cleanup's job, not this request's
	}
	// use parent
}
```

**Orphans are a cleanup problem, not a read-path problem.** Don't add safety-net
filters that re-check parents on reads. Where a parent really is needed (for example
its labels for authorization), load it once as above, and skip children whose parent
can't be resolved instead of failing the whole request.

## 5. Lifecycle and writes

- **Create:** the client sends namespace and name. The database mints the uid and
  returns it. A duplicate name gets 409.
- **Create a child:** seaplane-api gets the parent (404 if it is missing, 409 if it is
  being deleted), sets the ownerReference and label, then inserts.
- **Use an account object:** things a cluster needs before it exists (`OidcConfig`, and
  `DNSReservation` so a shared-VPC hosted zone can be made for its domain) are
  top-level objects the client creates first. The cluster references one by uid.
  seaplane-api checks it is ready (422) and unclaimed (409), creates the cluster with
  its finalizer, then sets `seaplane.io/claimed-by-cluster-uid`. If the claim fails,
  it deletes the cluster. The operator doesn't provision a cluster until its claims
  are confirmed. When the client passes no `DNSReservation`, the operator creates and
  claims one.
- **Update:** spec and annotations only. Identity, ownerReferences, labels, and
  service-set fields never change.
- **Delete:** by name. The owner's finalizer deletes its children and finishes once
  they are gone. As in Kubernetes, starting a delete sets the deletion timestamp and
  bumps `generation`, so `GenerationChangedPredicate` sees it. Deleting again changes
  neither.

**Write what you checked.** Updates and deletes use the `resourceVersion` of the
snapshot that was validated and authorized. Don't re-read between the check and the
write. On a conflict, a retry re-reads, re-validates, and re-authorizes before it
writes again.

## 6. Ownership and cleanup

Every controller cleans up what it created, in its finalizer. Anything left behind is
a bug. For a cluster, that means deleting its children (listed by its
`seaplane.io/cluster-uid` label), waiting until they are gone, and releasing its
claims: its `Index` claims, its `OidcConfig`, and its `DNSReservation`, which is
deleted or released according to its `clusterDeletionPolicy`. Each child's own
finalizer tears down what that child created.

seaplane-db doesn't garbage-collect. ownerReferences are stored and drive `Owns()`
watches, but nothing deletes an object because of them, and `blockOwnerDeletion` does
nothing. Orphans don't break correctness: uids are never reused, so a recreated
cluster never picks up an old cluster's children. Removing them is cleanup's job
([§4](#4-references-and-reads)).

## 7. Uniqueness

| Unique within…          | Tool                   | Example                                                        |
| ----------------------- | ---------------------- | -------------------------------------------------------------- |
| one kind in one account | the name (primary key) | only one cluster `typeidhcp` in the account                    |
| any wider scope         | an `Index` claim       | DNS prefix `f7a3` in shard 0; an OIDC issuer URL in the region |

An `Index` is an empty object. Its namespace is the scope and its name is the value.
The primary key allows one per (scope, value), so **creating it is the claim**.

```yaml
kind: Index
metadata:
  namespace: dns-shard-0-reservations # the scope
  name: f7a3 # the claimed value
  labels:
    seaplane.io/owner-uid: 2b8e0c4d-5a6f-4e7b-9c1d-3e4f5a6b7c8d # the DNSReservation
spec: {}
```

- **Claim:** create it. On `AlreadyExists`, get it. If `owner-uid` is yours, you already
  hold it (retries are safe). Otherwise it's taken: pick another value or report a
  conflict.
- **The holder is a uid.** Never a name: a recreated object with the same name must not
  inherit or release the old one's claim. If the holder doesn't exist yet, create it
  first and claim with its uid.
- **Values that aren't valid names** (e.g. URLs): normalize, then hash
  (`IssuerURLIndexName`).
- **Keep the data on the owner** (e.g. `dnsReservation.status.baseDomain`). The Index
  is only the lock; don't add a second object.
- **Release:** the owner's finalizer deletes Indexes carrying its uid, and never ones
  carrying someone else's.

**Why Index.** It relies on the one guarantee seaplane-db already has, so a new
uniqueness rule needs no migration. A per-rule DB constraint would put business rules
in the schema. Checking in seaplane-api (list, then insert) is not atomic. A generic
unique-keys table written in the same transaction as the owner would release keys
automatically, but it adds new write behavior to seaplane-db; revisit it only if
uniqueness rules multiply.

## 8. Sharding

Each operator replica reconciles one slice of the rows, keyed by the owning cluster's
uid: the `seaplane.io/cluster-uid` label, or the row's own uid if it has no owner.

**Why.** Reconcile work (AWS and management-cluster calls, retries, rate limits) grows
with the fleet. Sharding spreads it across replicas and limits the blast radius of one
bad pod. Keying by cluster keeps a cluster and all its objects on one replica, so
while the replica count is stable, each cluster is reconciled in one place. Only
watches and reconciles are sharded;
reads always see every row. Details and limits are in
[sharding.md](../seaplane-operator/docs/sharding.md).

**What it means for your code:**

- **Carry the label.** An object whose watch triggers a cluster reconcile must carry
  that cluster's `seaplane.io/cluster-uid`. Otherwise its events go to the wrong
  replica.
- **Be idempotent.** While the replica count changes, a cluster can briefly be
  reconciled by two pods.
- **No decisions from local counts across clusters.** For something like placing onto
  a management cluster with limited capacity, claim with an `Index` or use an
  optimistic update on a shared object.

## Review checklist

- [ ] References another object? It stores the **uid**.
- [ ] Belongs to a cluster? **ownerReference + `seaplane.io/cluster-uid`**.
- [ ] Lists a cluster's objects? The **label**, not `InNamespace`.
- [ ] Builds a name? Only `<cluster>.<child>`.
- [ ] Per-cluster object on the management cluster? In **`cluster-<uid>`**, plain name.
- [ ] Customer metadata? **Annotations**, validated. No customer path writes labels.
- [ ] Platform-set field? Set once in **conversion**, before authorization.
- [ ] Storage layer changes the object? It should **check and error** instead.
- [ ] `Get` inside a loop over a list that can grow, or a re-fetch of something the caller loaded? Prefer to **batch** and pass it down.
- [ ] Read path re-checks parents? Remove it; **skip** unresolvable children.
- [ ] Update or delete? Writes the **checked snapshot's** `resourceVersion`; a retry re-checks.
- [ ] Something unique beyond the name? One **`Index`** held by a **uid**, value stored on the owner.
- [ ] Needed before the cluster exists (like DNS)? A **top-level account object** the cluster claims.
- [ ] Validation? In **seaplane-api**; CRD markers aren't enforced.
- [ ] Reconciler safe to run twice? No **local counts** for cross-cluster decisions.
- [ ] New database constraint or special query? Use code instead.
