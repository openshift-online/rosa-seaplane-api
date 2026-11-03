# Prometheus Metrics Reference

All metrics are namespaced under `pgctl_` and partitioned by subsystem.
Metrics are opt-in: each component accepts a `WithMetrics` call after construction.
Passing a `nil` registerer to any `New*Metrics` factory returns `nil`, which disables
instrumentation with zero overhead.

---

## Writer Metrics (`pgctl_writer_*`)

| Metric                                     | Type      | Labels          | Description                                                       |
| ------------------------------------------ | --------- | --------------- | ----------------------------------------------------------------- |
| `pgctl_writer_write_duration_seconds`      | Histogram | `gvk`, `result` | Duration of write operations (stored procedure call).             |
| `pgctl_writer_write_step_duration_seconds` | Histogram | `step`          | Duration of individual steps within a write transaction.          |
| `pgctl_writer_writes_total`                | Counter   | `gvk`, `result` | Total write operations by outcome.                                |
| `pgctl_writer_noop_suppressions_total`     | Counter   | —               | Writes suppressed because the row already held identical content. |
| `pgctl_writer_doorbell_errors_total`       | Counter   | —               | Failed `pg_notify` doorbell sends (fire-and-forget, non-fatal).   |

**Result label values:** `success`, `noop`, `conflict`, `already_exists`, `ambiguous_commit`, `error`.

**Step label values:** `stored_proc` (overall server-side call), `suppression_check`, `upsert` (returned by the stored procedure via `clock_timestamp()` instrumentation).

---

## Watcher Metrics (`pgctl_watcher_*`)

| Metric                                 | Type      | Labels | Description                                                                                        |
| -------------------------------------- | --------- | ------ | -------------------------------------------------------------------------------------------------- |
| `pgctl_watcher_poll_duration_seconds`  | Histogram | `gvk`  | Wall-clock duration of a single poll cycle.                                                        |
| `pgctl_watcher_poll_events_delivered`  | Histogram | `gvk`  | Number of events delivered per poll cycle.                                                         |
| `pgctl_watcher_doorbell_polls_total`   | Counter   | —      | Polls triggered by a `pg_notify` doorbell notification.                                            |
| `pgctl_watcher_baseline_polls_total`   | Counter   | —      | Polls triggered by the baseline timer (no doorbell).                                               |
| `pgctl_watcher_baseline_catches_total` | Counter   | —      | Baseline polls that found new events while LISTEN was configured — indicates missed notifications. |
| `pgctl_watcher_listen_errors_total`    | Counter   | —      | Errors from `WaitForNotification` on the LISTEN connection.                                        |
| `pgctl_watcher_reconnects_total`       | Counter   | —      | Successful LISTEN reconnections via `ListenConnFactory`.                                           |

**Operational notes:**

- A rising `baseline_catches_total` while `reconnects_total` is flat signals a
  persistent doorbell gap — the LISTEN connection may be wedged without error.
- `doorbell_polls_total` vs `baseline_polls_total` shows the ratio of fast-path
  vs liveness-backstop deliveries. Under healthy doorbells, baseline should be
  low relative to doorbell.
- **Sharding:** no new metrics are introduced for sharding. When running a
  sharded fleet, expect `poll_events_delivered` to frequently report zero events
  for individual shards — a shard only sees changes to its owned namespaces, so
  empty polls are normal and not a sign of doorbell failure. Use `baseline_catches_total`
  (not zero-event polls) to diagnose doorbell health.

---

## Verifier Metrics (`pgctl_verifier_*`)

| Metric                                   | Type      | Labels      | Description                                                                                                   |
| ---------------------------------------- | --------- | ----------- | ------------------------------------------------------------------------------------------------------------- |
| `pgctl_verifier_canary_delivery_seconds` | Histogram | —           | Write-to-delivery latency for canary objects — times the full pipeline from commit to event channel delivery. |
| `pgctl_verifier_violations_total`        | Counter   | `invariant` | Invariant violations detected (I2, I3, I4, I5). Any non-zero value should page.                               |
| `pgctl_verifier_events_checked_total`    | Counter   | —           | Total events processed by the verifier.                                                                       |

**Invariant label values:** `I2` (non-monotonic sequence), `I3` (duplicate delivery), `I5` (below-horizon gap).

---

## Integration

### Enabling metrics

```go
import "github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/metrics"

reg := prometheus.DefaultRegisterer

writerMetrics   := metrics.NewWriterMetrics(reg)
watcherMetrics  := metrics.NewWatcherMetrics(reg)
verifierMetrics := metrics.NewVerifierMetrics(reg)

w := writer.New(conn, hooks).WithMetrics(writerMetrics)
watcher := reader.NewWatcher(pollConn, listenConn, cfg, hooks).WithMetrics(watcherMetrics)
ver := verifier.New(pollConn, canaryConn, cfg).WithMetrics(verifierMetrics)
```

### Exposing metrics

```go
import "github.com/prometheus/client_golang/prometheus/promhttp"

http.Handle("/metrics", promhttp.Handler())
```

### Disabling metrics (tests)

Pass `nil` to any `New*Metrics` factory — the `WithMetrics(nil)` call is a no-op
and all instrumentation is skipped with zero overhead.
