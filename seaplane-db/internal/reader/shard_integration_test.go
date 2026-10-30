package reader_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/model"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/resourceversion"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/schema"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/writer"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/test/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashtextResidue returns the shard residue for a key using the same formula
// as the SQL shard clause: abs(hashtext(key)::bigint) % mod.
func hashtextResidue(t *testing.T, db *testinfra.TestDB, key string, mod int) int {
	t.Helper()
	conn := db.Connect(t)
	return hashtextResidueConn(t, conn, key, mod)
}

func hashtextResidueConn(t *testing.T, conn *pgx.Conn, key string, mod int) int {
	t.Helper()
	var residue int
	err := conn.QueryRow(context.Background(),
		"SELECT abs(hashtext($1)::bigint) % $2", key, mod).Scan(&residue)
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

func TestShardedWatcher_DeliversOnlyOwnedNamespaceUIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	const mod = 2
	const numResources = 32

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	expectedShard := make(map[string]int, numResources)
	for i := range numResources {
		namespace := fmt.Sprintf("ns-%02d", i)
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: namespace, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		expectedShard[result.UID.String()] = hashtextResidue(t, db, namespace+"/"+result.UID.String(), mod)
	}
	owned := 0
	unowned := 0
	for _, shard := range expectedShard {
		if shard == 0 {
			owned++
		} else {
			unowned++
		}
	}
	require.Greater(t, owned, 0, "need at least one row UID in shard 0")
	require.Greater(t, unowned, 0, "need at least one row UID outside shard 0")

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

	assert.Len(t, events, owned)
	for _, ev := range events {
		assert.Equal(t, 0, expectedShard[ev.Resource.UID.String()],
			"unexpected UID %s in shard 0 events", ev.Resource.UID)
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

func TestShardedList_FiltersNamespaceUIDShards(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, conn))
	db.TruncateAll(t, conn)

	const mod = 3
	const numResources = 32

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	expectedShard := make(map[string]int, numResources)
	for i := range numResources {
		namespace := fmt.Sprintf("ns-%02d", i)
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: namespace, Name: fmt.Sprintf("obj-%d", i),
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		expectedShard[result.UID.String()] = hashtextResidue(t, db, namespace+"/"+result.UID.String(), mod)
	}
	owned := 0
	for _, shard := range expectedShard {
		if shard == 0 {
			owned++
		}
	}
	require.Greater(t, owned, 0)
	require.Less(t, owned, numResources)

	listConn := db.Connect(t)
	shard := &reader.ShardSpec{Mod: mod, Owned: []int{0}}
	result, err := reader.List(ctx, listConn, "apps/v1/Deployment", shard.ToListFilter())
	require.NoError(t, err)

	assert.Len(t, result.Resources, owned)
	for _, r := range result.Resources {
		assert.Equal(t, 0, expectedShard[r.UID.String()],
			"unexpected UID %s in shard 0 list", r.UID)
	}

	allResult, err := reader.List(ctx, listConn, "apps/v1/Deployment", nil)
	require.NoError(t, err)
	assert.Len(t, allResult.Resources, numResources)
}

func TestShardedListAndWatcher_ClusterUIDWithRowUIDFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	db := testinfra.StartPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	setupConn := db.Connect(t)
	require.NoError(t, schema.Migrate(ctx, setupConn))
	db.TruncateAll(t, setupConn)

	const (
		mod       = 8
		namespace = "account-123"
		gvk       = "apps/v1/Deployment"
	)

	hashConn := db.Connect(t)
	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	type testResource struct {
		name string
		key  string
	}
	resources := make([]testResource, 0, 20)
	writeResource := func(name string, labels map[string]string) string {
		t.Helper()
		metadata := json.RawMessage(`{}`)
		if labels != nil {
			encoded, err := json.Marshal(map[string]any{"labels": labels})
			require.NoError(t, err)
			metadata = encoded
		}
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: gvk, Namespace: namespace, Name: name,
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`), Metadata: metadata,
		})
		require.NoError(t, err)
		return result.UID.String()
	}
	addResource := func(name, key string) {
		resources = append(resources, testResource{name: name, key: key})
	}

	// The Cluster has no cluster-uid label, so it falls back to its database UID.
	// Its owned children use that same UID as cluster-uid.
	clusterAUID := writeResource("cluster-a", nil)
	addResource("cluster-a", clusterAUID)
	writeResource("nodepool-a", map[string]string{testClusterUIDLabel: clusterAUID})
	addResource("nodepool-a", clusterAUID)
	writeResource("placement-a", map[string]string{testClusterUIDLabel: clusterAUID})
	addResource("placement-a", clusterAUID)

	clusterBUID := writeResource("cluster-b", nil)
	addResource("cluster-b", clusterBUID)
	writeResource("nodepool-b", map[string]string{testClusterUIDLabel: clusterBUID})
	addResource("nodepool-b", clusterBUID)

	// Account-level rows without cluster-uid remain keyed by their own UID.
	accountResidues := make(map[int]bool)
	for i := 0; i < 100 && len(accountResidues) < 2; i++ {
		name := fmt.Sprintf("dns-reservation-%d", i)
		uid := writeResource(name, map[string]string{
			"seaplane.io/claimed-by-cluster-uid": clusterAUID,
		})
		addResource(name, uid)
		accountResidues[hashtextResidueConn(t, hashConn, namespace+"/"+uid, mod)] = true
	}
	require.GreaterOrEqual(t, len(accountResidues), 2,
		"account-level rows in one namespace should distribute by their own UIDs")

	expectedShard := make(map[string]int, len(resources))
	for _, resource := range resources {
		expectedShard[namespace+"/"+resource.name] = hashtextResidueConn(t, hashConn, namespace+"/"+resource.key, mod)
	}
	clusterShard := expectedShard[namespace+"/cluster-a"]
	assert.Equal(t, clusterShard, expectedShard[namespace+"/nodepool-a"], "NodePool must share its Cluster's UID shard")
	assert.Equal(t, clusterShard, expectedShard[namespace+"/placement-a"], "Placement must share its Cluster's UID shard")

	listConn := db.Connect(t)
	seen := make(map[string]int, len(resources))
	for shard := range mod {
		filter := (&reader.ShardSpec{
			Mod: mod, Owned: []int{shard}, KeyLabel: testClusterUIDLabel,
		}).ToListFilter()
		result, err := reader.List(ctx, listConn, gvk, filter)
		require.NoError(t, err)
		for _, resource := range result.Resources {
			id := resource.Namespace + "/" + resource.Name
			assert.Equal(t, shard, expectedShard[id], "resource %q was listed by the wrong shard", id)
			seen[id]++
		}
	}
	for _, resource := range resources {
		id := namespace + "/" + resource.name
		assert.Equal(t, 1, seen[id], "resource %q must appear in exactly one shard", id)
	}

	pollConn := connectManual(t, db)
	listenConn := connectManual(t, db)
	w := reader.NewWatcher(pollConn, listenConn, reader.WatcherConfig{
		GVK:              gvk,
		StartRV:          resourceversion.RV{Watermark: 0},
		BaselineInterval: 300 * time.Millisecond,
		Shard: &reader.ShardSpec{
			Mod: mod, Owned: []int{clusterShard}, KeyLabel: testClusterUIDLabel,
		},
	}, nil)

	watchCtx, watchCancel := context.WithCancel(ctx)
	done := runWatcher(w, watchCtx)
	defer func() {
		watchCancel()
		<-done
		_ = pollConn.Close(context.Background())
		_ = listenConn.Close(context.Background())
	}()

	events := collectEvents(w, 5*time.Second, time.Second)
	expectedOnShard := 0
	for _, residue := range expectedShard {
		if residue == clusterShard {
			expectedOnShard++
		}
	}
	require.Len(t, events, expectedOnShard)
	seenOnShard := make(map[string]bool, len(events))
	for _, event := range events {
		id := event.Resource.Namespace + "/" + event.Resource.Name
		assert.Equal(t, clusterShard, expectedShard[id], "watch delivered %q to the wrong shard", id)
		seenOnShard[id] = true
	}
	assert.True(t, seenOnShard[namespace+"/cluster-a"])
	assert.True(t, seenOnShard[namespace+"/nodepool-a"])
	assert.True(t, seenOnShard[namespace+"/placement-a"])
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

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	result, err := wr.Write(ctx, model.WriteRequest{
		GVK: "core/v1/Namespace", Namespace: "", Name: "cluster-obj",
		Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	owningShard := hashtextResidue(t, db, "/"+result.UID.String(), mod)
	nonOwningShard := 1 - owningShard

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

	const mod = 4
	const numResources = 40
	namespaces := make([]string, numResources)
	for i := range namespaces {
		namespaces[i] = fmt.Sprintf("ns-%04d", i)
	}

	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	expectedShard := make(map[string]int, numResources)
	for _, ns := range namespaces {
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		expectedShard[result.UID.String()] = hashtextResidue(t, db, ns+"/"+result.UID.String(), mod)
	}
	owned := 0
	unowned := 0
	for _, shard := range expectedShard {
		if shard == 0 || shard == 2 {
			owned++
		} else {
			unowned++
		}
	}
	require.Greater(t, owned, 0)
	require.Greater(t, unowned, 0)

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

	assert.Len(t, events, owned)
	for _, ev := range events {
		assert.Contains(t, []int{0, 2}, expectedShard[ev.Resource.UID.String()],
			"unexpected UID %s in multi-owned shard events", ev.Resource.UID)
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

	const mod = 2
	const numResources = 32
	namespaces := make([]string, numResources)
	for i := range namespaces {
		namespaces[i] = fmt.Sprintf("ns-%02d", i)
	}

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
	hashConn := db.Connect(t)
	expectedShard := make(map[string]int, numResources)
	for _, ns := range namespaces {
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: ns, Name: "live-obj",
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		expectedShard[result.UID.String()] = hashtextResidueConn(t, hashConn, ns+"/"+result.UID.String(), mod)
	}
	owned := 0
	unowned := 0
	for _, shard := range expectedShard {
		if shard == 0 {
			owned++
		} else {
			unowned++
		}
	}
	require.Greater(t, owned, 0)
	require.Greater(t, unowned, 0)

	events := collectEvents(w, 5*time.Second, 1*time.Second)

	assert.Len(t, events, owned,
		"live writes should be filtered by shard")
	for _, ev := range events {
		assert.Equal(t, 0, expectedShard[ev.Resource.UID.String()],
			"unexpected UID %s in live shard events", ev.Resource.UID)
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

	const mod = 2
	const numResources = 32

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

	// The doorbell is shared across shards, but the UID predicate must keep
	// events limited to rows owned by this replica.
	writerConn := db.Connect(t)
	wr := writer.New(writerConn, nil)
	hashConn := db.Connect(t)
	expectedShard := make(map[string]int, numResources)
	for i := range numResources {
		namespace := fmt.Sprintf("ns-%02d", i)
		result, err := wr.Write(ctx, model.WriteRequest{
			GVK: "apps/v1/Deployment", Namespace: namespace, Name: fmt.Sprintf("event-%02d", i),
			Spec: json.RawMessage(`{}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		expectedShard[result.UID.String()] = hashtextResidueConn(t, hashConn, namespace+"/"+result.UID.String(), mod)
	}
	owned := 0
	unowned := 0
	for _, shard := range expectedShard {
		if shard == 0 {
			owned++
		} else {
			unowned++
		}
	}
	require.Greater(t, owned, 0)
	require.Greater(t, unowned, 0)

	events := collectEvents(w, 6*time.Second, 3*time.Second)
	assert.Len(t, events, owned,
		"only rows on this UID shard should be delivered despite cross-shard doorbells")
	for _, event := range events {
		assert.Equal(t, 0, expectedShard[event.Resource.UID.String()],
			"unexpected UID %s after cross-shard doorbell", event.Resource.UID)
	}
}
