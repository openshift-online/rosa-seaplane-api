package reader_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/model"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/resourceversion"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/schema"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/writer"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/test/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashtextResidue returns the shard residue for a namespace using the same
// formula as the SQL shard clause: abs(hashtext(ns)::bigint) % mod.
func hashtextResidue(t *testing.T, db *testinfra.TestDB, ns string, mod int) int {
	t.Helper()
	conn := db.Connect(t)
	var residue int
	err := conn.QueryRow(context.Background(),
		"SELECT abs(hashtext($1)::bigint) % $2", ns, mod).Scan(&residue)
	require.NoError(t, err)
	return residue
}

// collectEvents drains the watcher's event channel until idle for idleTimeout.
func collectEvents(w *reader.Watcher, total time.Duration, idle time.Duration) []reader.Event { //nolint:unparam
	var events []reader.Event
	deadline := time.After(total)
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case ev := <-w.Events():
			events = append(events, ev)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return events
		case <-deadline:
			return events
		}
	}
}

func TestShardedWatcher_DeliversOnlyOwnedNamespaces(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma", "ns-delta", "ns-epsilon"}
	mod := 2

	owned0 := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, db, ns, mod) == 0 {
			owned0[ns] = true
		}
	}
	require.NotEmpty(t, owned0, "need at least one namespace in shard 0")
	require.Less(t, len(owned0), len(namespaces), "need at least one namespace NOT in shard 0")

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{0}},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() {
		watchCancel()
		<-done
		_ = pollConn.Close(context.Background())
	}()

	events := collectEvents(w, 5*time.Second, 1*time.Second)

	assert.Len(t, events, len(owned0))
	for _, ev := range events {
		assert.True(t, owned0[ev.Resource.Namespace],
			"unexpected namespace %q in shard 0 events", ev.Resource.Namespace)
	}
}

func TestUnshardedWatcher_DeliversAll(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma"}

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() {
		watchCancel()
		<-done
		_ = pollConn.Close(context.Background())
	}()

	events := collectEvents(w, 5*time.Second, 1*time.Second)
	assert.Len(t, events, len(namespaces))
}

func TestShardedList_FiltersNamespaces(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, conn))
	db.TruncateAll(t, conn)

	namespaces := []string{"ns-a", "ns-b", "ns-c", "ns-d", "ns-e", "ns-f"}
	mod := 3

	owned0 := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, db, ns, mod) == 0 {
			owned0[ns] = true
		}
	}

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for i, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: fmt.Sprintf("obj-%d", i),
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	listConn := db.Connect(t)
	shard := &reader.ShardSpec{Mod: mod, Owned: []int{0}}
	result, err := reader.List(ctx, listConn, "apps/v1/Deployment", shard.ToListFilter())
	require.NoError(t, err)

	assert.Len(t, result.Resources, len(owned0))
	for _, r := range result.Resources {
		assert.True(t, owned0[r.Namespace],
			"unexpected namespace %q in shard 0 list", r.Namespace)
	}

	allResult, err := reader.List(ctx, listConn, "apps/v1/Deployment", nil)
	require.NoError(t, err)
	assert.Len(t, allResult.Resources, len(namespaces))
}

// --- Finding 13: cluster-scoped resources (namespace="") ---

func TestShardedWatcher_ClusterScopedResources(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	mod := 2
	emptyNsResidue := hashtextResidue(t, db, "", mod)
	owningShard := emptyNsResidue
	nonOwningShard := 1 - owningShard

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	_, err := wr.Write(ctx, model.WriteRequest{
		GVK: "core/v1/Namespace", Namespace: "", Name: "cluster-obj",
		Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	// Watcher for the owning shard should see the event.
	pollOwn := connectManual(t, db)
	wOwn := reader.NewWatcher(pollOwn, nil, reader.WatcherConfig{
		GVK:              "core/v1/Namespace",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{owningShard}},
	}, nil)
	ownCtx, ownCancel := context.WithCancel(ctx)
	ownDone := runWatcher(wOwn, ownCtx)
	defer func() { ownCancel(); <-ownDone; _ = pollOwn.Close(context.Background()) }()

	ownEvents := collectEvents(wOwn, 5*time.Second, 1*time.Second)
	assert.Len(t, ownEvents, 1, "owning shard should see the cluster-scoped resource")

	// Watcher for the non-owning shard should see nothing.
	pollOther := connectManual(t, db)
	wOther := reader.NewWatcher(pollOther, nil, reader.WatcherConfig{
		GVK:              "core/v1/Namespace",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{nonOwningShard}},
	}, nil)
	otherCtx, otherCancel := context.WithCancel(ctx)
	otherDone := runWatcher(wOther, otherCtx)
	defer func() { otherCancel(); <-otherDone; _ = pollOther.Close(context.Background()) }()

	otherEvents := collectEvents(wOther, 3*time.Second, 1*time.Second)
	assert.Len(t, otherEvents, 0,
		"non-owning shard must NOT see cluster-scoped resources — use UnshardedGVKs for these")
}

// --- Finding 14: Mod=1 edge case ---

func TestShardedWatcher_Mod1ReturnsAll(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	namespaces := []string{"ns-a", "ns-b", "ns-c"}

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: 1, Owned: []int{0}},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() { watchCancel(); <-done; _ = pollConn.Close(context.Background()) }()

	events := collectEvents(w, 5*time.Second, 1*time.Second)
	assert.Len(t, events, len(namespaces), "Mod=1 should return all resources")
}

func TestShardedList_Mod1ReturnsAll(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, conn))
	db.TruncateAll(t, conn)

	namespaces := []string{"ns-a", "ns-b", "ns-c"}

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	shard := &reader.ShardSpec{Mod: 1, Owned: []int{0}}
	listConn := db.Connect(t)
	result, err := reader.List(ctx, listConn, "apps/v1/Deployment", shard.ToListFilter())
	require.NoError(t, err)
	assert.Len(t, result.Resources, len(namespaces), "Mod=1 list should return all resources")
}

// --- Finding 15: multiple Owned values ---

func TestShardedWatcher_MultipleOwnedResidues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	mod := 4
	namespaces := make([]string, 20)
	for i := range namespaces {
		namespaces[i] = fmt.Sprintf("ns-%04d", i)
	}

	owned02 := map[string]bool{}
	for _, ns := range namespaces {
		r := hashtextResidue(t, db, ns, mod)
		if r == 0 || r == 2 {
			owned02[ns] = true
		}
	}
	require.NotEmpty(t, owned02)
	require.Less(t, len(owned02), len(namespaces))

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{0, 2}},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() { watchCancel(); <-done; _ = pollConn.Close(context.Background()) }()

	events := collectEvents(w, 5*time.Second, 1*time.Second)

	assert.Len(t, events, len(owned02))
	for _, ev := range events {
		assert.True(t, owned02[ev.Resource.Namespace],
			"unexpected namespace %q in multi-owned shard events", ev.Resource.Namespace)
	}
}

// --- Finding 16: live streaming (write after watcher starts) ---

func TestShardedWatcher_LiveStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	mod := 2
	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma", "ns-delta"}
	owned0 := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, db, ns, mod) == 0 {
			owned0[ns] = true
		}
	}
	require.NotEmpty(t, owned0)
	require.Less(t, len(owned0), len(namespaces))

	// Start the watcher BEFORE writing any resources.
	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{0}},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() { watchCancel(); <-done; _ = pollConn.Close(context.Background()) }()

	// Let the watcher complete its initial poll.
	time.Sleep(500 * time.Millisecond)

	// Now write resources while the watcher is running.
	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for _, ns := range namespaces {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "live-obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	events := collectEvents(w, 5*time.Second, 1*time.Second)

	assert.Len(t, events, len(owned0),
		"live writes should be filtered by shard")
	for _, ev := range events {
		assert.True(t, owned0[ev.Resource.Namespace],
			"unexpected namespace %q in live shard events", ev.Resource.Namespace)
	}
}

// --- Finding 17: compaction + sharding interaction ---

func TestShardedWatcher_CompactionReturnsErrGone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	// Write a resource so there's data.
	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	_, err := wr.Write(ctx, model.WriteRequest{
		GVK: "apps/v1/Deployment", Namespace: "default", Name: "obj",
		Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	// Manually set compaction_horizon to a high value.
	adminConn := db.Connect(t)
	_, err = adminConn.Exec(ctx, `
		INSERT INTO compaction_horizon (gvk, compacted_xid)
		VALUES ('apps/v1/Deployment', 999999999)
		ON CONFLICT (gvk) DO UPDATE SET compacted_xid = 999999999`)
	require.NoError(t, err)

	// Start a sharded watcher with hwm=0 — below the compaction horizon.
	pollConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard:            &reader.ShardSpec{Mod: 2, Owned: []int{0}},
	}, nil)

	runErr := w.Run(ctx)
	require.ErrorIs(t, runErr, reader.ErrGone,
		"sharded watcher should return ErrGone when hwm < compacted_xid")
	_ = pollConn.Close(context.Background())
}

// --- Finding 18: doorbell with sharding (spurious cross-shard polls) ---

func TestShardedWatcher_DoorbellCrossShardIsHarmless(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	mod := 2
	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma", "ns-delta"}
	owned0 := map[string]bool{}
	notOwned := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, db, ns, mod) == 0 {
			owned0[ns] = true
		} else {
			notOwned[ns] = true
		}
	}
	require.NotEmpty(t, owned0)
	require.NotEmpty(t, notOwned)

	// Start watcher with LISTEN connection for doorbell.
	// Baseline is moderate so doorbell-triggered polls are the primary path,
	// but the baseline still fires within the test's collection window.
	pollConn := connectManual(t, db)
	listenConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, listenConn, reader.WatcherConfig{
		GVK:              "apps/v1/Deployment",
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 2 * time.Second,
		Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{0}},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() {
		watchCancel()
		<-done
		_ = pollConn.Close(context.Background())
		_ = listenConn.Close(context.Background())
	}()

	time.Sleep(500 * time.Millisecond)

	// Write to an unowned namespace — doorbell fires, but watcher finds nothing.
	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	for ns := range notOwned {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "cross-shard",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	// Wait for a baseline poll cycle to pass — should see no events.
	noEvents := collectEvents(w, 2*time.Second, 1*time.Second)
	assert.Empty(t, noEvents, "cross-shard writes should not produce events")

	// Now write to an owned namespace — should be delivered.
	for ns := range owned0 {
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "same-shard",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}

	ownedEvents := collectEvents(w, 5*time.Second, 1*time.Second)
	assert.Len(t, ownedEvents, len(owned0),
		"owned-shard writes should be delivered after cross-shard doorbell")
}
