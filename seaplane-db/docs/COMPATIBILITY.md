# Controller-Runtime Compatibility

`pgruntime.NewManager()` returns a standard `manager.Manager` backed by PostgreSQL instead of kube-apiserver + etcd.

## What works

The standard reconciler pattern works unchanged:

- `For()`, `Owns()`, `Watches()` with all standard predicates (`GenerationChanged`, `LabelChanged`, `AnnotationChanged`, `ResourceVersionChanged`)
- `MaxConcurrentReconciles`
- `client.Get()`, `client.List()` with `InNamespace`, `MatchingLabels`, `MatchingFields`, `Limit` / `Continue`
- `Create()`, `Update()`, `Delete()`, `Status().Update()`
- Finalizers, labels, annotations
- Generation tracking
- Optimistic concurrency via `ResourceVersion` and UID (409 Conflict on stale versions or object incarnations)
- Health and readiness checks
- `apierrors.IsNotFound()` / `IsConflict()` / `IsAlreadyExists()`

Your CRD types must have exported fields named exactly `Spec` and `Status` (found by reflection). Both the type and its `List` type must be registered in the scheme.

## What's different

These features work but behave differently from standard controller-runtime against kube-apiserver:

| What                              | Standard kube                                           | pgruntime                                                                                                                                                                                                                                                                          |
| --------------------------------- | ------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Read consistency                  | `GetClient()` reads from cache — can be seconds stale   | `GetClient()` reads from DB — always current                                                                                                                                                                                                                                       |
| No-op writes                      | May or may not bump `ResourceVersion`                   | Content-equal updates with current UID/version are suppressed; duplicate Create always returns `AlreadyExists`                                                                                                                                                                     |
| ResourceVersion                   | Global etcd revision; never reused across objects       | Per-object `object_version`, reset to 1 when a name is recreated. Only meaningful within one incarnation, so update/status writes and version-checked deletes also carry `metadata.uid`. List ResourceVersions are DB watermarks and are not comparable to object ResourceVersions |
| UID on writes                     | UID may be omitted from the request object              | `Update()` and `Status().Update()` require `metadata.uid`; `Delete()` requires it when the object has a `ResourceVersion`. Missing UID returns `BadRequest` (400); deleting by name without a `ResourceVersion` still works                                                        |
| Delete lifecycle                  | Object removed immediately after finalizers clear       | Tombstone row persists until compaction (24h default). Invisible to callers — `Get()` returns NotFound, `List()` excludes them                                                                                                                                                     |
| Horizontal scaling                | Leader election (1 active replica) or external sharding | Multiple replicas via namespace-hash sharding (`Options.Shard`); direct client always sees full dataset                                                                                                                                                                            |
| Event delivery                    | HTTP/2 streaming watch                                  | Poll (5s baseline) with `pg_notify` doorbell (~100ms typical delivery)                                                                                                                                                                                                             |
| `GetAPIReader()` vs `GetClient()` | Different: uncached vs cached reads                     | Identical: both go to DB                                                                                                                                                                                                                                                           |
| Periodic resync                   | Informers re-list every 10h to catch missed events      | No resync — poll-based watch can't miss events within the compaction window                                                                                                                                                                                                        |
| `IndexField()`                    | Registers cache indexes used by field selectors         | No-op — accepted but ignored. `MatchingFields` queries the database directly                                                                                                                                                                                                       |
| Cluster-scoped resources          | RESTMapper marks types as cluster-scoped; empty ns      | Works with empty namespace                                                                                                                                                                                                                                                         |

### Sharding specifics

When `Options.Shard` is set, the cache's informer List/Watch queries include a field-selector-like restriction (`hashtext(namespace) % Mod = ANY(Owned)`). This means the informer cache only contains the replica's owned namespaces. The direct client (`GetClient()`, `GetAPIReader()`) is never restricted — it always queries the full dataset.

`UnshardedGVKs` exempts specific GVKs from the shard predicate so every replica watches them fully. Use this for cluster-scoped or shared configuration resources.

**PostgreSQL version caveat:** `hashtext()` return values may change across PostgreSQL major versions. A major-version upgrade reshuffles all namespace-to-shard assignments. This is benign — all replicas restart during a major upgrade, and the transient period of overlapping watches resolves after the rolling restart. No data is lost; the only effect is a burst of duplicate reconciles.

## What's not supported

These features return an error, panic, or are not available. If you're porting an existing controller, check for these.

| Feature                           | What happens                 | What to do instead                                        |
| --------------------------------- | ---------------------------- | --------------------------------------------------------- |
| `Patch()`                         | Returns `MethodNotSupported` | Read-modify-`Update()` with 409 retry                     |
| `Status().Patch()`                | Returns `MethodNotSupported` | `Status().Update()`                                       |
| `Apply()` (server-side apply)     | Returns `MethodNotSupported` | `Create()` / `Update()`                                   |
| `DeleteAllOf()`                   | Returns `MethodNotSupported` | `List()` then `Delete()` individually                     |
| `GetWebhookServer()`              | Panics                       | Don't call. No webhook support                            |
| `GetConfig()` / `GetHTTPClient()` | Panics                       | Don't create additional kube clients from the manager     |
| `DryRun` option                   | Returns error                | Don't use                                                 |
| `GracePeriodSeconds` option       | Returns error                | Handle graceful shutdown in your reconciler               |
| `PropagationPolicy` option        | Returns error                | Clean up children via finalizers                          |
| `Preconditions` option            | Returns error                | Set `ResourceVersion` and UID on the object instead       |
| `GenerateName`                    | Returns error                | Set `Name` explicitly before `Create()`                   |
| Event recording                   | Not available                | Use structured logging or Prometheus metrics              |
| Admission webhooks                | Not available                | Validate inputs in your reconciler                        |
| CRD schema validation             | Not available                | Any JSON accepted. Validate in your reconciler            |
| RBAC / authorization              | Not available                | All clients have full access. Gate access above pgruntime |
| Unstructured objects              | Not available                | All types must be registered in the scheme                |

## What fails silently

These are the features that accept input without error but don't behave as expected. If you're porting a controller that relies on these, you'll get wrong behavior with no warning.

| Feature          | What happens                    | What to do instead                                |
| ---------------- | ------------------------------- | ------------------------------------------------- |
| Owner references | Stored but no garbage collector | Use finalizers on the parent to clean up children |
