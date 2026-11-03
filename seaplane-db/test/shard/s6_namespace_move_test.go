package shard_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/model"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/resourceversion"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/writer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS6_CrossShardNamespaceMove verifies that when a resource is "moved"
// between namespaces that hash to different shards (by deleting from one and
// creating in the other), each sharded watcher sees exactly the events
// belonging to its shard, and the two creates have different UIDs.
//
// Sequence:
//  1. Find two namespaces ns-A and ns-B that hash to different residues (mod 2).
//  2. Create resource (ns-A, "mover").
//  3. Delete it (set deletion_timestamp).
//  4. Create resource (ns-B, "mover") — same name, different namespace.
//  5. Shard-0 watcher sees exactly the events for namespaces in its shard.
//  6. Shard-1 watcher sees exactly the events for namespaces in its shard.
//  7. The UIDs of the two creates differ.
func TestS6_CrossShardNamespaceMove(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	truncateAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		mod  = 2
		gvk  = "apps/v1/Deployment"
		name = "mover"
	)

	// Find namespaces that hash to shard 0 and shard 1.
	probeConn := freshConn(t)
	nsByResidue := findNamespacesForShards(t, probeConn, mod)
	nsA := nsByResidue[0] // hashes to shard 0
	nsB := nsByResidue[1] // hashes to shard 1
	t.Logf("ns-A=%q (shard 0), ns-B=%q (shard 1)", nsA, nsB)

	// Step 1: Create resource in ns-A.
	wr := writer.New(freshConn(t), nil)
	resA, err := wr.Write(ctx, model.WriteRequest{
		GVK: gvk, Namespace: nsA, Name: name,
		Spec: json.RawMessage(`{"phase":"A"}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.True(t, resA.Changed)
	uidA := resA.UID
	t.Logf("created in ns-A: uid=%s, version=%d", uidA, resA.ObjectVersion)

	// Step 2: Delete resource in ns-A (set deletion_timestamp).
	now := time.Now()
	delResult, err := wr.Write(ctx, model.WriteRequest{
		GVK: gvk, Namespace: nsA, Name: name,
		Spec: json.RawMessage(`{"phase":"A"}`), Status: json.RawMessage(`{}`),
		Metadata:          json.RawMessage(`{}`),
		DeletionTimestamp: &now,
		ExpectedVersion:   resA.ObjectVersion,
	})
	require.NoError(t, err)
	require.True(t, delResult.Changed)
	t.Logf("deleted in ns-A: version=%d", delResult.ObjectVersion)

	// Step 3: Create resource in ns-B (same name, different namespace).
	resB, err := wr.Write(ctx, model.WriteRequest{
		GVK: gvk, Namespace: nsB, Name: name,
		Spec: json.RawMessage(`{"phase":"B"}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.True(t, resB.Changed)
	uidB := resB.UID
	t.Logf("created in ns-B: uid=%s, version=%d", uidB, resB.ObjectVersion)

	// UIDs must differ — these are distinct objects.
	require.NotEqual(t, uidA, uidB, "UIDs of creates in different namespaces must differ")
	require.NotEqual(t, uuid.Nil, uidA)
	require.NotEqual(t, uuid.Nil, uidB)

	// Start two sharded watchers from hwm=0.
	type shardResult struct {
		shard  int
		events []reader.Event
	}
	resultCh := make(chan shardResult, mod)

	for shard := range mod {
		pollConn := connectManual(t)
		w := reader.NewWatcher(pollConn, nil, reader.WatcherConfig{
			GVK:              gvk,
			StartRV:          resourceversion.RV{Watermark: 0},
			BaselineInterval: 200 * time.Millisecond,
			Shard:            &reader.ShardSpec{Mod: mod, Owned: []int{shard}},
		}, nil)

		watchCtx, watchCancel := context.WithCancel(ctx)
		done := runWatcher(w, watchCtx)

		go func() {
			defer func() {
				watchCancel()
				<-done
				_ = pollConn.Close(context.Background())
			}()

			var events []reader.Event
			timeout := time.NewTimer(3 * time.Second)
			defer timeout.Stop()
			for {
				select {
				case ev, ok := <-w.Events():
					if !ok {
						resultCh <- shardResult{shard: shard, events: events}
						return
					}
					events = append(events, ev)
					if !timeout.Stop() {
						select {
						case <-timeout.C:
						default:
						}
					}
					timeout.Reset(1 * time.Second)
				case <-timeout.C:
					resultCh <- shardResult{shard: shard, events: events}
					return
				case <-ctx.Done():
					resultCh <- shardResult{shard: shard, events: events}
					return
				}
			}
		}()
	}

	shardEvents := make(map[int][]reader.Event, mod)
	for range mod {
		r := <-resultCh
		shardEvents[r.shard] = r.events
	}

	// Shard 0 should see: ADDED (ns-A) then DELETED (ns-A) — both belong to ns-A.
	shard0 := shardEvents[0]
	t.Logf("shard 0 events: %d", len(shard0))
	for i, ev := range shard0 {
		t.Logf("  [%d] %s ns=%s name=%s uid=%s", i, ev.Type, ev.Resource.Namespace, ev.Resource.Name, ev.Resource.UID)
	}

	// Shard 1 should see: ADDED (ns-B) — belongs to ns-B.
	shard1 := shardEvents[1]
	t.Logf("shard 1 events: %d", len(shard1))
	for i, ev := range shard1 {
		t.Logf("  [%d] %s ns=%s name=%s uid=%s", i, ev.Type, ev.Resource.Namespace, ev.Resource.Name, ev.Resource.UID)
	}

	// Shard 0 assertions: should see events only for ns-A.
	// The watcher polls from hwm=0, so it picks up the final state of the row.
	// Since PK is (gvk, namespace, name) and ns-A's row has deletion_timestamp set,
	// the watcher sees exactly one DELETED event for ns-A.
	require.Len(t, shard0, 1, "shard 0 should see exactly 1 event (DELETED in ns-A)")
	assert.Equal(t, reader.EventDeleted, shard0[0].Type)
	assert.Equal(t, nsA, shard0[0].Resource.Namespace)
	assert.Equal(t, name, shard0[0].Resource.Name)

	// Shard 1 assertions: should see events only for ns-B.
	require.Len(t, shard1, 1, "shard 1 should see exactly 1 event (ADDED in ns-B)")
	assert.Equal(t, reader.EventAdded, shard1[0].Type)
	assert.Equal(t, nsB, shard1[0].Resource.Namespace)
	assert.Equal(t, name, shard1[0].Resource.Name)
	assert.Equal(t, uidB, shard1[0].Resource.UID,
		"shard 1's ADDED event should carry the UID from the second create")

	// Cross-shard: the UIDs must differ.
	assert.NotEqual(t, shard0[0].Resource.UID, shard1[0].Resource.UID,
		"UIDs across the two shards must differ (different objects)")
}
