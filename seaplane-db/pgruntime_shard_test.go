package seaplanedb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	seaplanedb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	runtimescheme "sigs.k8s.io/controller-runtime/pkg/scheme"
)

// --- Gadget type for UnshardedGVKs testing ---

type Gadget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GadgetSpec `json:"spec,omitempty"`
}

type GadgetSpec struct {
	Size string `json:"size"`
}

type GadgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gadget `json:"items"`
}

func (in *Gadget) DeepCopyObject() runtime.Object {
	out := new(Gadget)
	*out = *in
	in.DeepCopyInto(&out.ObjectMeta)
	return out
}

func (in *GadgetList) DeepCopyObject() runtime.Object {
	out := new(GadgetList)
	*out = *in
	in.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Gadget, len(in.Items))
		for i := range in.Items {
			out.Items[i] = in.Items[i]
			in.Items[i].DeepCopyInto(&out.Items[i].ObjectMeta)
		}
	}
	return out
}

func (in *Gadget) GetObjectKind() schema.ObjectKind     { return &in.TypeMeta }
func (in *GadgetList) GetObjectKind() schema.ObjectKind { return &in.TypeMeta }

func init() {
	b := &runtimescheme.Builder{GroupVersion: testGV} //nolint:staticcheck
	b.Register(&Gadget{}, &GadgetList{})
	if err := b.AddToScheme(testScheme); err != nil {
		panic(err)
	}
}

// --- helpers ---

func hashtextResidue(t *testing.T, ns string, mod int) int {
	t.Helper()
	conn := sharedDB.Connect(t)
	var residue int
	err := conn.QueryRow(context.Background(),
		"SELECT abs(hashtext($1)::bigint) % $2", ns, mod).Scan(&residue)
	require.NoError(t, err)
	return residue
}

func newShardedManager(t *testing.T, shard *seaplanedb.ShardConfig) ctrl.Manager {
	t.Helper()
	conn := sharedDB.Connect(t)
	sharedDB.TruncateAll(t, conn)
	_ = conn.Close(context.Background())

	mgr, err := seaplanedb.NewManager(seaplanedb.Options{
		Scheme: testScheme,
		DSN:    sharedDB.ConnStr,
		Shard:  shard,
		Logger: logr.Discard(),
	})
	require.NoError(t, err)
	return mgr
}

// --- T3.1: Config validation ---

func TestShardConfig_Validation(t *testing.T) {
	tests := []struct {
		name        string
		shard       *seaplanedb.ShardConfig
		errContains string
	}{
		{
			name:        "Mod zero",
			shard:       &seaplanedb.ShardConfig{Mod: 0, Owned: []int{0}},
			errContains: "Mod must be > 0",
		},
		{
			name:        "Mod negative",
			shard:       &seaplanedb.ShardConfig{Mod: -1, Owned: []int{0}},
			errContains: "Mod must be > 0",
		},
		{
			name:        "empty Owned",
			shard:       &seaplanedb.ShardConfig{Mod: 2, Owned: []int{}},
			errContains: "Owned must be non-empty",
		},
		{
			name:        "Owned out of range",
			shard:       &seaplanedb.ShardConfig{Mod: 2, Owned: []int{3}},
			errContains: "out of range",
		},
		{
			name:        "duplicate Owned",
			shard:       &seaplanedb.ShardConfig{Mod: 4, Owned: []int{1, 1}},
			errContains: "duplicate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := seaplanedb.NewManager(seaplanedb.Options{
				Scheme: testScheme,
				DSN:    sharedDB.ConnStr,
				Shard:  tt.shard,
				Logger: logr.Discard(),
			})
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.errContains)
		})
	}

	t.Run("nil shard is valid", func(t *testing.T) {
		mgr := newManager(t)
		assert.NotNil(t, mgr)
	})

	t.Run("valid shard accepted", func(t *testing.T) {
		mgr := newShardedManager(t, &seaplanedb.ShardConfig{
			Mod: 2, Owned: []int{0},
		})
		assert.NotNil(t, mgr)
	})
}

// --- T3.4: Client ignores Shard ---

func TestShardedManager_ClientUnfiltered(t *testing.T) {
	mgr := newShardedManager(t, &seaplanedb.ShardConfig{
		Mod: 2, Owned: []int{0},
	})
	c := mgr.GetClient()
	ctx := context.Background()

	namespaces := []string{"ns-a", "ns-b", "ns-c", "ns-d"}
	for i, ns := range namespaces {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("w-%d", i)},
			Spec:       WidgetSpec{Color: "blue"},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	list := &WidgetList{}
	require.NoError(t, c.List(ctx, list))
	assert.Len(t, list.Items, len(namespaces),
		"direct client List must return all objects regardless of shard config")
}

// --- T3.8: API reader unfiltered ---

func TestShardedManager_APIReaderUnfiltered(t *testing.T) {
	mgr := newShardedManager(t, &seaplanedb.ShardConfig{
		Mod: 2, Owned: []int{0},
	})
	c := mgr.GetClient()
	apiReader := mgr.GetAPIReader()
	ctx := context.Background()

	namespaces := []string{"ns-a", "ns-b", "ns-c", "ns-d"}
	for i, ns := range namespaces {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("w-%d", i)},
			Spec:       WidgetSpec{Color: "red"},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	list := &WidgetList{}
	require.NoError(t, apiReader.List(ctx, list))
	assert.Len(t, list.Items, len(namespaces),
		"APIReader List must return all objects regardless of shard config")
}

// --- T3.6: Reconcile routes only owned-shard events ---

func TestShardedManager_ReconcileOnlyOwned(t *testing.T) {
	mod := 2
	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma", "ns-delta", "ns-epsilon"}

	owned := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, ns, mod) == 0 {
			owned[ns] = true
		}
	}
	require.NotEmpty(t, owned, "need at least one namespace in shard 0")
	require.Less(t, len(owned), len(namespaces), "need at least one namespace NOT in shard 0")

	mgr := newShardedManager(t, &seaplanedb.ShardConfig{
		Mod: mod, Owned: []int{0},
	})
	c := mgr.GetClient()

	reconciled := make(chan reconcile.Request, 20)
	err := ctrl.NewControllerManagedBy(mgr).
		Named("widget-reconcile-owned").
		For(&Widget{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			reconciled <- req
			return ctrl.Result{}, nil
		}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	for _, ns := range namespaces {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "obj"},
			Spec:       WidgetSpec{Color: "green"},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	var events []reconcile.Request
	deadline := time.After(10 * time.Second)
	for len(events) < len(owned) {
		select {
		case req := <-reconciled:
			events = append(events, req)
		case <-deadline:
			t.Fatalf("timeout: got %d events, want %d", len(events), len(owned))
		}
	}
	// Idle-drain: wait for 1s of silence to catch unexpected extras.
	idleTimer := time.NewTimer(1 * time.Second)
	defer idleTimer.Stop()
	for {
		select {
		case req := <-reconciled:
			events = append(events, req)
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(1 * time.Second)
		case <-idleTimer.C:
			goto done
		}
	}
done:

	assert.Len(t, events, len(owned))
	for _, ev := range events {
		assert.True(t, owned[ev.Namespace],
			"unexpected reconcile for namespace %q (not in shard 0)", ev.Namespace)
	}
}

// --- T3.7: Unsharded GVK sees all namespaces ---

func TestShardedManager_UnshardedGVK(t *testing.T) {
	mod := 2
	namespaces := []string{"ns-alpha", "ns-beta", "ns-gamma", "ns-delta"}

	notOwned := 0
	for _, ns := range namespaces {
		if hashtextResidue(t, ns, mod) != 0 {
			notOwned++
		}
	}
	require.Greater(t, notOwned, 0, "need at least one namespace NOT in shard 0")

	mgr := newShardedManager(t, &seaplanedb.ShardConfig{
		Mod:   mod,
		Owned: []int{0},
		UnshardedGVKs: []schema.GroupVersionKind{
			testGV.WithKind("Gadget"),
		},
	})
	c := mgr.GetClient()

	gadgetReconciled := make(chan reconcile.Request, 20)
	err := ctrl.NewControllerManagedBy(mgr).
		Named("gadget-unsharded").
		For(&Gadget{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			gadgetReconciled <- req
			return ctrl.Result{}, nil
		}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	for _, ns := range namespaces {
		g := &Gadget{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "g"},
			Spec:       GadgetSpec{Size: "large"},
		}
		require.NoError(t, c.Create(ctx, g))
	}

	var events []reconcile.Request
	deadline := time.After(10 * time.Second)
	for len(events) < len(namespaces) {
		select {
		case req := <-gadgetReconciled:
			events = append(events, req)
		case <-deadline:
			t.Fatalf("timeout: got %d events, want %d", len(events), len(namespaces))
		}
	}

	assert.Len(t, events, len(namespaces),
		"unsharded GVK must see events from all namespaces")
}

// --- T3.12: Two replicas, disjoint partition, complete coverage ---

func TestShardedManager_TwoReplicas(t *testing.T) {
	mod := 2
	namespaces := []string{"ns-1", "ns-2", "ns-3", "ns-4", "ns-5", "ns-6"}

	shard0 := map[string]bool{}
	shard1 := map[string]bool{}
	for _, ns := range namespaces {
		if hashtextResidue(t, ns, mod) == 0 {
			shard0[ns] = true
		} else {
			shard1[ns] = true
		}
	}
	require.NotEmpty(t, shard0, "need at least one namespace in shard 0")
	require.NotEmpty(t, shard1, "need at least one namespace in shard 1")

	conn := sharedDB.Connect(t)
	sharedDB.TruncateAll(t, conn)
	_ = conn.Close(context.Background())

	mgr0, err := seaplanedb.NewManager(seaplanedb.Options{
		Scheme: testScheme,
		DSN:    sharedDB.ConnStr,
		Shard:  &seaplanedb.ShardConfig{Mod: mod, Owned: []int{0}},
		Logger: logr.Discard(),
	})
	require.NoError(t, err)

	mgr1, err := seaplanedb.NewManager(seaplanedb.Options{
		Scheme: testScheme,
		DSN:    sharedDB.ConnStr,
		Shard:  &seaplanedb.ShardConfig{Mod: mod, Owned: []int{1}},
		Logger: logr.Discard(),
	})
	require.NoError(t, err)

	reconciled0 := make(chan reconcile.Request, 20)
	reconciled1 := make(chan reconcile.Request, 20)

	require.NoError(t, ctrl.NewControllerManagedBy(mgr0).
		Named("widget-shard0").
		For(&Widget{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			reconciled0 <- req
			return ctrl.Result{}, nil
		})))

	require.NoError(t, ctrl.NewControllerManagedBy(mgr1).
		Named("widget-shard1").
		For(&Widget{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			reconciled1 <- req
			return ctrl.Result{}, nil
		})))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go func() {
		if err := mgr0.Start(ctx); err != nil {
			t.Logf("mgr0 exited: %v", err)
		}
	}()
	go func() {
		if err := mgr1.Start(ctx); err != nil {
			t.Logf("mgr1 exited: %v", err)
		}
	}()
	time.Sleep(1 * time.Second)

	c := mgr0.GetClient()
	for _, ns := range namespaces {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"},
			Spec:       WidgetSpec{Color: "blue"},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	var events0, events1 []reconcile.Request
	deadline := time.After(10 * time.Second)
	for len(events0) < len(shard0) || len(events1) < len(shard1) {
		select {
		case req := <-reconciled0:
			events0 = append(events0, req)
		case req := <-reconciled1:
			events1 = append(events1, req)
		case <-deadline:
			t.Fatalf("timeout: shard0 %d/%d, shard1 %d/%d",
				len(events0), len(shard0), len(events1), len(shard1))
		}
	}
	// Idle-drain: wait for 1s of silence to catch unexpected extras.
	idleTimer := time.NewTimer(1 * time.Second)
	defer idleTimer.Stop()
	for {
		select {
		case req := <-reconciled0:
			events0 = append(events0, req)
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(1 * time.Second)
		case req := <-reconciled1:
			events1 = append(events1, req)
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(1 * time.Second)
		case <-idleTimer.C:
			goto verified
		}
	}
verified:

	assert.Len(t, events0, len(shard0), "shard 0 event count mismatch")
	assert.Len(t, events1, len(shard1), "shard 1 event count mismatch")

	for _, ev := range events0 {
		assert.True(t, shard0[ev.Namespace],
			"mgr0 got unexpected namespace %q", ev.Namespace)
	}
	for _, ev := range events1 {
		assert.True(t, shard1[ev.Namespace],
			"mgr1 got unexpected namespace %q", ev.Namespace)
	}

	allNs := map[string]bool{}
	for _, ev := range events0 {
		allNs[ev.Namespace] = true
	}
	for _, ev := range events1 {
		allNs[ev.Namespace] = true
	}
	assert.Len(t, allNs, len(namespaces), "union of both shards must cover all namespaces")
}
