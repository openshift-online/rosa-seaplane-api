# Namespace-Hash Sharding

The seaplane operator shards its informer cache across replicas. Each replica owns
the namespaces where `abs(hashtext(namespace)::bigint) % replicaCount` equals its
ordinal, and its cache lists and watches only those rows, except for kinds listed
in `UnshardedGVKs`. Direct reads are never sharded.

Without sharding, every replica would hold and poll every object, so per-pod
memory and watch load would grow with the fleet. Because the cache also decides
which pod receives an object's events, sharding splits reconcile work (AWS calls,
DynamoDB desires, retries) across pods, and a stuck or crash-looping pod delays
only the accounts it owns.

Customer resources live in `account-<accountID>`, so a shard is effectively a set
of **accounts**: a Cluster, its NodePools and Placement, and the account's
DNSReservations and OidcConfigs all go to the same pod. See
[Limitations](#limitations).

## How It Works

### Shard Assignment

The pgruntime cache partitions its List/Watch streams using PostgreSQL's
`hashtext()` function:

```
shard = abs(hashtext(namespace)::bigint) % replicaCount
```

The sharding is configured via `pgruntime.ShardConfig`:

```go
pgruntime.ShardConfig{
    Mod:   replicaCount,
    Owned: []int{ordinal},
    UnshardedGVKs: []schema.GroupVersionKind{
        // kinds every replica must watch
    },
}
```

- **Mod**: the modulus (total number of shards, equal to `replicaCount`)
- **Owned**: the shard indices this replica owns (typically just `[ordinal]`)
- **UnshardedGVKs**: GVKs every replica watches in full, for kinds whose changes
  all replicas must react to (e.g. a cluster-scoped regional config object).
  Kinds that are only read need not be listed.

### Operator Replica Shard Assignment

The operator runs as a **StatefulSet**. Each pod derives its shard from two
values:

- **Ordinal**: parsed from the pod hostname (e.g., `seaplane-operator-2` -> 2)
- **REPLICA_COUNT**: total number of replicas (env var, set by Helm to match `replicaCount`)

Each pod owns the shard equal to its ordinal:

```
my_shard = ordinal
```

Example with 4 replicas:

| Pod   | Shard | Reconciles namespaces where                 |
| ----- | ----- | ------------------------------------------- |
| Pod-0 | 0     | `abs(hashtext(namespace)::bigint) % 4 == 0` |
| Pod-1 | 1     | `abs(hashtext(namespace)::bigint) % 4 == 1` |
| Pod-2 | 2     | `abs(hashtext(namespace)::bigint) % 4 == 2` |
| Pod-3 | 3     | `abs(hashtext(namespace)::bigint) % 4 == 3` |

### Platform API

The API uses `pgruntime.NewClient()` which is never sharded. It sees all data
regardless of shard configuration. No shard-related configuration is needed for
the API.

## Limitations

- **An account is the smallest unit.** An account with 200 Clusters is handled by
  one pod. Adding replicas does not split it, and load is uneven when account
  sizes vary.
- **No failover.** While a pod is down, its accounts do not progress until it
  returns.
- **The cache is shard-local.** Counting Placements in the cache to pick the
  least-loaded management cluster sees only this pod's accounts. Use an `Index`
  claim or an optimistic update on a shared record instead.
- **Watched objects must share the namespace.** If a Cluster's controller watched
  an object in a separate internal namespace, that object's events would go to
  the pod owning that namespace, not the Cluster's pod.

## Why Not Shard by UID

A per-Cluster UID key would split large accounts, but it breaks when a name is
reused. Reconcile requests carry only namespace and name, and reconcilers read by
name. If Cluster `prod` on pod 0 is deleted and recreated, the new UID can hash to
pod 1. A request for `account-x/prod` that still reaches pod 0, such as a late
status event, makes pod 0 read the new `prod` and reconcile it, and its periodic
requeue keeps that going. Two pods then reconcile the same Cluster until pod 0
restarts, doubling DynamoDB and management-cluster writes and racing each other's
updates.

A name never changes namespace, so namespace sharding cannot hit this. If
per-Cluster spreading is ever needed, derive the key from the name instead:
`hashtext(namespace || '/' || split_part(name, '.', 1))`.

## Configuration

### Environment Variables

| Variable        | Default | Description                                 |
| --------------- | ------- | ------------------------------------------- |
| `REPLICA_COUNT` | `1`     | Number of operator replicas (= shard count) |
| `POSTGRES_DSN`  | --      | PostgreSQL connection string (required)     |

### Helm Values (Operator)

```yaml
replicaCount: 4
```

The chart automatically sets `REPLICA_COUNT` to match `replicaCount`.

## Scaling

To scale from 2 to 4 operator replicas:

1. Update `replicaCount: 4` in the operator Helm values
2. The chart sets `REPLICA_COUNT=4` on every pod
3. Each pod re-derives its shard on startup (rolling restart)

No per-pod configuration, no manual shard assignment.

### Constraints

- Changing `replicaCount` requires a rolling restart of the StatefulSet so each
  pod picks up the new modulus. During the rollout, some namespaces may
  temporarily be handled by two pods or none. Reconciler spec-writes to
  PostgreSQL are idempotent (content-equal updates are suppressed), so duplicate
  processing converges safely for internal state. External side effects such as
  DynamoDB desire writes may receive duplicate requests during the rollout
  window; these are handled by the deterministic document-ID scheme (see
  [DynamoDB strategy](dynamodb-strategy.md)).
- The direct client (used by `pgruntime.NewClient()`) is never sharded and
  always sees all data. Only the cache/watch layer is partitioned.
