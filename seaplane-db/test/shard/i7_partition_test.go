package shard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/model"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/resourceversion"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/writer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestI7_PartitionCompleteness verifies that 4 shards with Mod=4 form a
// complete, disjoint partition of the namespace space. We write 50 resources
// across random namespaces, run 4 sharded watchers (one per residue), and
// assert:
//   - The union of events from all watchers equals the full set (no gaps).
//   - Pairwise intersections are empty (no duplicates across shards).
func TestI7_PartitionCompleteness(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	truncateAll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		mod        = 4
		numObjects = 50
		gvk        = "apps/v1/Deployment"
	)

	// Write 50 resources with distinct UUID-like namespaces.
	wr := writer.New(freshConn(t), nil)
	type written struct {
		Namespace string
		Name      string
	}
	allWritten := make([]written, 0, numObjects)

	for i := range numObjects {
		ns := fmt.Sprintf("ns-%08d", i)
		name := fmt.Sprintf("obj-%d", i)
		_, err := wr.Write(ctx, model.WriteRequest{
			GVK:       gvk,
			Namespace: ns,
			Name:      name,
			Spec:      json.RawMessage(`{"i":` + fmt.Sprintf("%d", i) + `}`),
			Status:    json.RawMessage(`{}`),
			Metadata:  json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		allWritten = append(allWritten, written{Namespace: ns, Name: name})
	}

	// Build lookup of expected residue for each namespace.
	residueConn := freshConn(t)
	expectedResidue := make(map[string]int, numObjects)
	for _, w := range allWritten {
		expectedResidue[w.Namespace] = hashtextResidue(t, residueConn, w.Namespace, mod)
	}

	// Start 4 sharded watchers, one per residue.
	type watcherResult struct {
		shard  int
		events []reader.Event
	}
	results := make(chan watcherResult, mod)

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
			// Collect events until we've gone 1s without a new one.
			timeout := time.NewTimer(3 * time.Second)
			defer timeout.Stop()
			for {
				select {
				case ev, ok := <-w.Events():
					if !ok {
						results <- watcherResult{shard: shard, events: events}
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
					results <- watcherResult{shard: shard, events: events}
					return
				case <-ctx.Done():
					results <- watcherResult{shard: shard, events: events}
					return
				}
			}
		}()
	}

	// Collect results from all 4 watchers.
	shardEvents := make(map[int][]reader.Event, mod)
	for range mod {
		r := <-results
		shardEvents[r.shard] = r.events
	}

	// Assert: union equals the full set.
	type key struct{ Namespace, Name string }
	allSeen := make(map[key]int) // key -> shard that saw it
	for shard, events := range shardEvents {
		for _, ev := range events {
			k := key{ev.Resource.Namespace, ev.Resource.Name}
			if prevShard, dup := allSeen[k]; dup {
				t.Errorf("duplicate: (%s, %s) seen by shard %d AND shard %d",
					k.Namespace, k.Name, prevShard, shard)
			}
			allSeen[k] = shard
		}
	}

	// Every written object must appear exactly once.
	for _, w := range allWritten {
		k := key(w)
		shard, found := allSeen[k]
		if !assert.True(t, found, "object (%s, %s) not seen by any shard", w.Namespace, w.Name) {
			continue
		}
		// Verify the shard assignment matches the hash.
		assert.Equal(t, expectedResidue[w.Namespace], shard,
			"object (%s, %s) delivered to shard %d but hashtext residue is %d",
			w.Namespace, w.Name, shard, expectedResidue[w.Namespace])
	}

	assert.Equal(t, numObjects, len(allSeen),
		"union of all shard events should equal total objects written")

	// Report per-shard distribution.
	for shard := range mod {
		t.Logf("shard %d: %d events", shard, len(shardEvents[shard]))
	}
}
