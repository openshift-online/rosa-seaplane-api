# NodePool Controller

## Creation Flow

```mermaid
sequenceDiagram
    participant API as Platform API
    participant PG as PostgreSQL
    participant NPC as NodePool Controller
    participant DDB as DynamoDB

    participant KA as kube-applier-aws
    participant MC as Management Cluster

    API->>PG: Create NodePool CR (ownerRef + cluster-uid label set by API)
    NPC->>PG: Watch detects new NodePool
    NPC->>PG: Add finalizer (seaplane.io/nodepool), requeue
    NPC->>PG: Resolve parent Cluster via ownerReference name, compare UID → get PlacementRef
    alt Cluster not found or no PlacementRef
        NPC->>PG: Set phase=WaitingForCluster, requeue
    else Cluster has Bound Placement
        NPC->>NPC: Generate NodePool manifest (child portion of name)
        NPC->>DDB: Write ApplyDesire (nodepools/{childName})
        NPC->>DDB: Write ReadDesire for NodePool status
        NPC->>PG: Set phase=Provisioning
        KA->>DDB: Read ApplyDesires
        KA->>MC: Apply NodePool resource
        KA->>DDB: Write status (status table)
        DDB-->>NPC: GSI poll event via EventRouter (~15s)
        NPC->>DDB: Read status (consistent read)
        NPC->>PG: Update NodePool status (conditions, phase)
    end
```

### Reconcile Steps

1. **Finalizer**: Adds `seaplane.io/nodepool` finalizer on first reconcile, requeues
2. **Parent Cluster lookup**: Resolves the parent Cluster by the ownerReference name in the same account namespace, then compares UID. Waits if the Cluster is not found or has no `PlacementRef`
3. **Manifest generation**: Extracts the child portion of the `<cluster>.<child>` name and generates a HyperShift NodePool manifest
4. **ApplyDesire**: Writes one ApplyDesire to `{mc}-specs-applydesires`
5. **ReadDesire**: Creates a ReadDesire for the NodePool to get status feedback (extracts "Ready" condition from the remote NodePool)
6. **Status propagation**: Reads status from DynamoDB, updates NodePool CR conditions (Synced, Ready) and phase
7. **Requeue**: Requeues every 5 minutes as a fallback; GSI two-speed polling via EventRouter provides the primary notification path (~15s latency)

### Generated Resource

The NodePool manifest on the MC uses the child portion of the `<cluster>.<child>` name and lives in namespace `cluster-<Cluster UID>`.

| Resource              | Name          | Namespace               | Purpose                                   |
| --------------------- | ------------- | ----------------------- | ----------------------------------------- |
| NodePool (HyperShift) | `{childName}` | `cluster-<Cluster UID>` | Worker node set on the management cluster |

## Deletion Flow

When a NodePool is deleted (either standalone or as part of Cluster cascade deletion):

```mermaid
sequenceDiagram
    participant User as User/API
    participant PG as PostgreSQL
    participant NPC as NodePool Controller
    participant DDB as DynamoDB

    User->>PG: Delete NodePool CR (sets DeletionTimestamp)
    NPC->>PG: Detect DeletionTimestamp, set phase=Deleting
    NPC->>PG: Look up parent Cluster → get PlacementRef
    NPC->>DDB: Upsert ApplyDesire with Type=Delete (same document ID)
    NPC->>DDB: Wait for kube-applier to confirm MC resource deletion
    NPC->>NPC: Requeue until confirmed
    NPC->>DDB: Delete ApplyDesire and ReadDesire specs
    NPC->>PG: Remove finalizer → CR deleted
```

### Deletion Steps

1. **PlacementRef lookup**: Gets the parent Cluster's PlacementRef to determine the target MC
2. **ApplyDesire Type=Delete**: Upserts the existing ApplyDesire with `Type=Delete` using the same document ID. kube-applier sees a MODIFY event and deletes the NodePool from the MC
3. **Confirmation**: Waits for kube-applier to confirm the MC resource deletion via the status table
4. **DynamoDB cleanup**: Deletes the ApplyDesire and ReadDesire specs from DynamoDB
5. **Finalizer removal**: Removes finalizer, allowing the CR to be garbage-collected

The parent Cluster and Placement are unaffected by standalone NodePool deletion.
