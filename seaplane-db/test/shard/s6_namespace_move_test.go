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
// between namespaces (by deleting from one and creating in the other), sharded
// watchers route the two incarnations by their namespace-scoped UIDs.
//
// Sequence:
//  1. Create a resource in ns-A and delete it.
//  2. Create the same name in ns-B, retrying the UID if necessary to get a
//     different shard from the first incarnation.
//  3. The two namespace/UID-derived shard residues differ.
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
		nsA  = "account-a"
		nsB  = "account-b"
	)

	probeConn := freshConn(t)
	t.Logf("resource namespace transition: ns-A=%q to ns-B=%q", nsA, nsB)

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
	shardA := hashtextResidue(t, probeConn, nsA+"/"+uidA.String(), mod)

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

	// Create the same name in ns-B. Retry the new incarnation until its
	// namespace-scoped UID key hashes to the other shard.
	resB, err := wr.Write(ctx, model.WriteRequest{
		GVK: gvk, Namespace: nsB, Name: name,
		Spec: json.RawMessage(`{"phase":"B"}`), Status: json.RawMessage(`{}`),
		Metadata: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.True(t, resB.Changed)
	uidB := resB.UID
	shardB := hashtextResidue(t, probeConn, nsB+"/"+uidB.String(), mod)
	for attempt := 0; shardB == shardA && attempt < 100; attempt++ {
		now := time.Now()
		_, err = wr.Write(ctx, model.WriteRequest{
			GVK: gvk, Namespace: nsB, Name: name,
			Spec: json.RawMessage(`{"phase":"B"}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`), DeletionTimestamp: &now,
			ExpectedVersion: resB.ObjectVersion,
		})
		require.NoError(t, err)
		resB, err = wr.Write(ctx, model.WriteRequest{
			GVK: gvk, Namespace: nsB, Name: name,
			Spec: json.RawMessage(`{"phase":"B"}`), Status: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		uidB = resB.UID
		shardB = hashtextResidue(t, probeConn, nsB+"/"+uidB.String(), mod)
	}
	require.NotEqual(t, shardA, shardB, "the two incarnations should exercise different namespace/UID shards")
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

	// The first UID's shard sees the deleted source row.
	shardAEvents := shardEvents[shardA]
	// The watcher polls from hwm=0, so it picks up the final state of the row.
	// Since PK is (gvk, namespace, name) and ns-A's row has deletion_timestamp set,
	// the watcher sees exactly one DELETED event for ns-A.
	require.Len(t, shardAEvents, 1, "the first UID shard should see the deleted source row")
	assert.Equal(t, reader.EventDeleted, shardAEvents[0].Type)
	assert.Equal(t, nsA, shardAEvents[0].Resource.Namespace)
	assert.Equal(t, name, shardAEvents[0].Resource.Name)
	assert.Equal(t, uidA, shardAEvents[0].Resource.UID)

	// The second UID's shard sees the recreated row.
	shardBEvents := shardEvents[shardB]
	require.Len(t, shardBEvents, 1, "the second UID shard should see the recreated row")
	assert.Equal(t, reader.EventAdded, shardBEvents[0].Type)
	assert.Equal(t, nsB, shardBEvents[0].Resource.Namespace)
	assert.Equal(t, name, shardBEvents[0].Resource.Name)
	assert.Equal(t, uidB, shardBEvents[0].Resource.UID)

	// Cross-shard: the UIDs must differ.
	assert.NotEqual(t, shardAEvents[0].Resource.UID, shardBEvents[0].Resource.UID,
		"UIDs across the two shards must differ (different objects)")
}
