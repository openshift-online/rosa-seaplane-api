# Cluster-UID Sharding

The seaplane operator partitions cache List/Watch streams across replicas by
the account namespace plus an effective UID key. A row with
`seaplane.io/cluster-uid` uses that UID; otherwise, it uses its own database UID.
The resource name is not part of the key.

```text
key = namespace + "/" + seaplane.io/cluster-uid   when the label is present
key = namespace + "/" + row UID                  otherwise
shard = abs(hashtext(key)::bigint) % replicaCount
```

The Cluster row has no `cluster-uid` label because seaplane-db assigns its UID
on insert, so its key is its namespace plus that UID. NodePools and Placements
are created after the Cluster exists and carry its UID in `cluster-uid`,
colocating them with the Cluster. The ownership label therefore doubles as the
shard key; no separate label or propagation logic is needed. Account-level resources
such as DNSReservations, OidcConfigs, and Indexes have no `cluster-uid` label and
shard by their own database UIDs within the account namespace. A
`claimed-by-cluster-uid` label does not change their shard key. Reads through the
direct client remain unsharded.

## How It Works

### Shard Assignment

The same parameterized PostgreSQL predicate is used for informer List and Watch
queries:

```sql
abs(hashtext(namespace || '/' || COALESCE(
    metadata->'labels'->>'seaplane.io/cluster-uid',
    uid::text
))::bigint) % $mod = ANY($owned)
```

If the label is absent, the namespace and row's database UID form the key. The
operator configures the label key via `pgruntime.ShardConfig`:

```go
pgruntime.ShardConfig{
    Mod:      replicaCount,
    Owned:    []int{ordinal},
    KeyLabel: v1alpha1.ClusterUIDLabel,
    UnshardedGVKs: []schema.GroupVersionKind{
        v1alpha1.SchemeGroupVersion.WithKind("ManagementCluster"),
    },
}
```

- **Mod**: the modulus (total number of shards, equal to `replicaCount`)
- **Owned**: the shard indices this replica owns (typically just `[ordinal]`)
- **KeyLabel**: the optional label whose value replaces the row UID within the
  namespace-scoped key; rows without it use their own UID
- **UnshardedGVKs**: GVKs exempt from sharding (every replica sees them)

### Operator Replica Assignment

The operator runs as a **StatefulSet**. Each pod parses its ordinal from the
hostname (e.g., `seaplane-operator-2` -> 2) and owns that shard from
`REPLICA_COUNT`:

```
my_shard = ordinal
```

For four replicas, each namespace-scoped Cluster UID hashes to one of shards
`0..3`. A Cluster and its owned children share a shard, while different
Clusters in one account can be handled by different replicas.

### ManagementCluster Visibility

ManagementCluster is cluster-scoped and declared in `UnshardedGVKs`. Every pod
sees all ManagementClusters through its standard manager client, so the
PlacementReconciler has the full registry.

### Cross-Cluster Decisions

Each operator pod's cache contains only its owned shard keys. Do not make
fleet-wide decisions—such as choosing the least-loaded ManagementCluster—from
counts in that shard-local cache. For scarce shared capacity, use an `Index`
claim or an optimistic update on a shared record. A direct read can see all rows,
but a list-then-act count is still not an atomic claim.

### Platform API and Reads

The platform API uses `pgruntime.NewClient()`, which is never sharded. The
operator's direct client reads likewise see all rows; only cache informer
List/Watch streams are partitioned.

## Configuration

| Variable        | Default | Description                                 |
| --------------- | ------- | ------------------------------------------- |
| `REPLICA_COUNT` | `1`     | Number of operator replicas (= shard count) |
| `POSTGRES_DSN`  | --      | PostgreSQL connection string (required)     |

```yaml
# Helm values (operator)
replicaCount: 4
```

The chart sets `REPLICA_COUNT` to match `replicaCount`.

## Scaling

To scale from 2 to 4 operator replicas:

1. Update `replicaCount: 4` in the operator Helm values.
2. The chart sets `REPLICA_COUNT=4` on every pod.
3. Each pod re-derives its shard on startup (rolling restart).

No per-pod configuration or manual shard assignment is needed.

### Constraints

- Changing `replicaCount` requires a rolling restart of the StatefulSet so each
  pod picks up the new modulus. During the rollout, some shard keys may
  temporarily be handled by two pods or none. Reconciler writes to PostgreSQL
  are idempotent, but external side effects such as DynamoDB desire writes may
  receive duplicate requests during the rollout; deterministic document IDs
  make those writes idempotent (see [DynamoDB strategy](dynamodb-strategy.md)).
- A shard belongs to one pod. While that pod is down or restarting, its rows
  are not reconciled; another pod does not automatically take over.
- The direct client and API remain unsharded and always see all rows.
