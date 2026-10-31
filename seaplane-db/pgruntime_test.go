package seaplanedb_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	seaplanedb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/test/testinfra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	runtimescheme "sigs.k8s.io/controller-runtime/pkg/scheme"
)

// --- Test types ---

var (
	testGV      = schema.GroupVersion{Group: "test.example.com", Version: "v1"}
	testBuilder = &runtimescheme.Builder{GroupVersion: testGV} //nolint:staticcheck
	testScheme  = runtime.NewScheme()
)

func init() {
	testBuilder.Register(&Widget{}, &WidgetList{})
	if err := testBuilder.AddToScheme(testScheme); err != nil {
		panic(err)
	}
}

type Widget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WidgetSpec   `json:"spec,omitempty"`
	Status            WidgetStatus `json:"status,omitempty"`
}

type WidgetSpec struct {
	Color string `json:"color"`
}

type WidgetStatus struct {
	Phase string `json:"phase,omitempty"`
}

type WidgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Widget `json:"items"`
}

func (in *Widget) DeepCopyObject() runtime.Object {
	out := new(Widget)
	*out = *in
	in.DeepCopyInto(&out.ObjectMeta)
	return out
}

func (in *WidgetList) DeepCopyObject() runtime.Object {
	out := new(WidgetList)
	*out = *in
	in.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Widget, len(in.Items))
		for i := range in.Items {
			out.Items[i] = in.Items[i]
			in.Items[i].DeepCopyInto(&out.Items[i].ObjectMeta)
		}
	}
	return out
}

func (in *Widget) GetObjectKind() schema.ObjectKind     { return &in.TypeMeta }
func (in *WidgetList) GetObjectKind() schema.ObjectKind { return &in.TypeMeta }

// --- Test infrastructure ---

var sharedDB *testinfra.TestDB

func TestMain(m *testing.M) {
	sharedDB = testinfra.StartPostgresForTestMain()
	code := m.Run()
	sharedDB.Stop()
	os.Exit(code)
}

func newManager(t *testing.T) ctrl.Manager {
	t.Helper()
	conn := sharedDB.Connect(t)
	sharedDB.TruncateAll(t, conn)
	_ = conn.Close(context.Background())

	mgr, err := seaplanedb.NewManager(seaplanedb.Options{
		Scheme: testScheme,
		DSN:    sharedDB.ConnStr,
		Logger: logr.Discard(),
	})
	require.NoError(t, err)
	return mgr
}

// --- Tests ---

func TestClient_CRUD(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "blue-widget"}

	// Create
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "blue-widget"},
		Spec:       WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))
	assert.NotEmpty(t, w.GetUID())
	assert.Equal(t, "1", w.GetResourceVersion())

	// Get
	got := &Widget{}
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, "blue", got.Spec.Color)
	assert.Equal(t, "1", got.GetResourceVersion())

	// Update
	got.Spec.Color = "red"
	require.NoError(t, c.Update(ctx, got))
	assert.Equal(t, "2", got.GetResourceVersion())

	got2 := &Widget{}
	require.NoError(t, c.Get(ctx, key, got2))
	assert.Equal(t, "red", got2.Spec.Color)

	// Delete
	require.NoError(t, c.Delete(ctx, got2))

	err := c.Get(ctx, key, &Widget{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestClient_CreateIgnoresClientUID(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	clientUID := types.UID("11111111-1111-4111-8111-111111111111")
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "uid-is-database-owned",
			UID:       clientUID,
		},
		Spec: WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))
	assert.NotEmpty(t, w.UID)
	assert.NotEqual(t, clientUID, w.UID, "Create must ignore a client-supplied UID")

	got := &Widget{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(w), got))
	assert.Equal(t, w.UID, got.UID)
}

func TestClient_NotFound(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "nope"}, &Widget{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestClient_AlreadyExists(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dup"},
		Spec:       WidgetSpec{Color: "green"},
	}
	require.NoError(t, c.Create(ctx, w))

	w2 := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dup"},
		Spec:       WidgetSpec{Color: "different"},
	}
	err := c.Create(ctx, w2)
	assert.True(t, apierrors.IsAlreadyExists(err), "expected AlreadyExists, got: %v", err)

	// Identical content is still a duplicate Create. No-op suppression applies to
	// updates, not to a second create at an occupied name.
	w3 := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dup"},
		Spec:       WidgetSpec{Color: "green"},
	}
	err = c.Create(ctx, w3)
	assert.True(t, apierrors.IsAlreadyExists(err), "identical duplicate Create must return AlreadyExists, got: %v", err)
}

func TestClient_Conflict(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "stale"},
		Spec:       WidgetSpec{Color: "v1"},
	}
	require.NoError(t, c.Create(ctx, w))

	// Get a copy, update the original to bump the version
	stale := &Widget{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "stale"}, stale))
	stale2 := stale.DeepCopyObject().(*Widget)

	stale.Spec.Color = "v2"
	require.NoError(t, c.Update(ctx, stale))

	// Now try to update with the stale copy
	stale2.Spec.Color = "v3"
	err := c.Update(ctx, stale2)
	assert.True(t, apierrors.IsConflict(err), "expected Conflict, got: %v", err)
}

func TestClient_StaleUIDCannotModifyRecreatedName(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "recreated"}

	old := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec:       WidgetSpec{Color: "old"},
	}
	require.NoError(t, c.Create(ctx, old))
	staleUpdate := old.DeepCopyObject().(*Widget)
	staleStatus := old.DeepCopyObject().(*Widget)
	staleDelete := old.DeepCopyObject().(*Widget)

	require.NoError(t, c.Delete(ctx, old))
	current := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec:       WidgetSpec{Color: "new"},
	}
	require.NoError(t, c.Create(ctx, current))
	require.NotEqual(t, old.UID, current.UID)
	assert.Equal(t, old.ResourceVersion, current.ResourceVersion,
		"recreated resources start at the same object version")

	staleUpdate.Spec.Color = "stale-update"
	err := c.Update(ctx, staleUpdate)
	assert.True(t, apierrors.IsConflict(err), "stale UID update should conflict, got: %v", err)

	staleStatus.Status.Phase = "stale-status"
	err = c.Status().Update(ctx, staleStatus)
	assert.True(t, apierrors.IsConflict(err), "stale UID status update should conflict, got: %v", err)

	err = c.Delete(ctx, staleDelete)
	assert.True(t, apierrors.IsConflict(err), "stale UID delete should conflict, got: %v", err)

	got := &Widget{}
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, "new", got.Spec.Color)
	assert.Empty(t, got.Status.Phase)
	assert.Equal(t, current.UID, got.UID)
}

func TestClient_StatusUpdate(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "with-status"},
		Spec:       WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))

	got := &Widget{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "with-status"}, got))

	got.Status.Phase = "Ready"
	require.NoError(t, c.Status().Update(ctx, got))
	assert.Equal(t, "2", got.GetResourceVersion())

	got2 := &Widget{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "with-status"}, got2))
	assert.Equal(t, "Ready", got2.Status.Phase)
	assert.Equal(t, "blue", got2.Spec.Color)
}

func TestClient_List(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c"} {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
			Spec:       WidgetSpec{Color: name},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	// Also create one in a different namespace
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "d"},
		Spec:       WidgetSpec{Color: "d"},
	}
	require.NoError(t, c.Create(ctx, w))

	// List all
	list := &WidgetList{}
	require.NoError(t, c.List(ctx, list))
	assert.Len(t, list.Items, 4)

	// List with namespace filter
	list2 := &WidgetList{}
	require.NoError(t, c.List(ctx, list2, client.InNamespace("default")))
	assert.Len(t, list2.Items, 3)
}

func TestClient_MetadataRoundTrip(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "labeled",
			Labels:      map[string]string{"app": "test", "env": "dev"},
			Annotations: map[string]string{"note": "hello"},
			Finalizers:  []string{"test.example.com/cleanup"},
		},
		Spec: WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))

	got := &Widget{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "labeled"}, got))
	assert.Equal(t, map[string]string{"app": "test", "env": "dev"}, got.GetLabels())
	assert.Equal(t, map[string]string{"note": "hello"}, got.GetAnnotations())
	assert.Equal(t, []string{"test.example.com/cleanup"}, got.GetFinalizers())
}

func TestClient_FinalizerLifecycle(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "with-finalizer"}

	// Create with finalizer
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "with-finalizer",
			Finalizers: []string{"test.example.com/cleanup"},
		},
		Spec: WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))

	// Delete sets deletionTimestamp but object stays visible (has finalizer)
	require.NoError(t, c.Delete(ctx, w))

	got := &Widget{}
	require.NoError(t, c.Get(ctx, key, got), "object with finalizer should still be visible after Delete")
	assert.NotNil(t, got.GetDeletionTimestamp(), "deletionTimestamp should be set")
	assert.Equal(t, []string{"test.example.com/cleanup"}, got.GetFinalizers())

	// Remove finalizer via Update
	got.SetFinalizers(nil)
	require.NoError(t, c.Update(ctx, got))

	// Now Get should return NotFound (fully deleted)
	err := c.Get(ctx, key, &Widget{})
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound after finalizer removal, got: %v", err)
}

func TestClient_FinalizerNotInList(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	// Create a live resource
	live := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "live"},
		Spec:       WidgetSpec{Color: "green"},
	}
	require.NoError(t, c.Create(ctx, live))

	// Create a resource with finalizer, then delete it
	dying := &Widget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "dying",
			Finalizers: []string{"test.example.com/cleanup"},
		},
		Spec: WidgetSpec{Color: "red"},
	}
	require.NoError(t, c.Create(ctx, dying))
	require.NoError(t, c.Delete(ctx, dying))

	// Create and fully delete a resource (no finalizer)
	gone := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gone"},
		Spec:       WidgetSpec{Color: "gray"},
	}
	require.NoError(t, c.Create(ctx, gone))
	require.NoError(t, c.Delete(ctx, gone))

	// List should include live + dying (has finalizer), but not gone (fully deleted)
	list := &WidgetList{}
	require.NoError(t, c.List(ctx, list))
	assert.Len(t, list.Items, 2, "should include live and dying-with-finalizer")

	names := map[string]bool{}
	for _, item := range list.Items {
		names[item.Name] = true
	}
	assert.True(t, names["live"])
	assert.True(t, names["dying"])
}

func TestClient_SubresourceIsolation(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "isolated"}

	// Create
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "isolated"},
		Spec:       WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))

	// Set status
	got := &Widget{}
	require.NoError(t, c.Get(ctx, key, got))
	got.Status.Phase = "Ready"
	require.NoError(t, c.Status().Update(ctx, got))

	// Update spec — should NOT clobber status
	got2 := &Widget{}
	require.NoError(t, c.Get(ctx, key, got2))
	assert.Equal(t, "Ready", got2.Status.Phase, "status should be preserved before spec update")

	got2.Spec.Color = "red"
	require.NoError(t, c.Update(ctx, got2))

	// Verify status is preserved
	got3 := &Widget{}
	require.NoError(t, c.Get(ctx, key, got3))
	assert.Equal(t, "red", got3.Spec.Color)
	assert.Equal(t, "Ready", got3.Status.Phase, "status should be preserved after spec update")
}

func TestClient_Generation(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "gen-test"}

	// Create — generation should be 1
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "gen-test"},
		Spec:       WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))
	assert.Equal(t, int64(1), w.GetGeneration())

	got := &Widget{}
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, int64(1), got.GetGeneration())

	// Update spec — generation should increment
	got.Spec.Color = "red"
	require.NoError(t, c.Update(ctx, got))

	got2 := &Widget{}
	require.NoError(t, c.Get(ctx, key, got2))
	assert.Equal(t, int64(2), got2.GetGeneration())

	// Update with same spec (only labels change) — generation should NOT increment
	got2.SetLabels(map[string]string{"env": "test"})
	require.NoError(t, c.Update(ctx, got2))

	got3 := &Widget{}
	require.NoError(t, c.Get(ctx, key, got3))
	assert.Equal(t, int64(2), got3.GetGeneration(), "generation should not increment for metadata-only changes")
	assert.Equal(t, map[string]string{"env": "test"}, got3.GetLabels())
}

func TestClient_DeleteBumpsGeneration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deleteBy func(stored *Widget) *Widget
	}{
		{
			name:     "with resourceVersion",
			deleteBy: func(stored *Widget) *Widget { return stored },
		},
		{
			name: "with resourceVersion and local edits, which delete ignores",
			deleteBy: func(stored *Widget) *Widget {
				w := stored.DeepCopyObject().(*Widget)
				w.DeletionTimestamp = nil
				w.Generation = 7
				return w
			},
		},
		{
			name: "without resourceVersion",
			deleteBy: func(stored *Widget) *Widget {
				return &Widget{ObjectMeta: metav1.ObjectMeta{Namespace: stored.Namespace, Name: stored.Name}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newManager(t)
			c := mgr.GetClient()
			ctx := context.Background()
			key := types.NamespacedName{Namespace: "default", Name: "delete-gen"}

			w := &Widget{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:  "default",
					Name:       "delete-gen",
					Finalizers: []string{"test.example.com/cleanup"},
				},
				Spec: WidgetSpec{Color: "blue"},
			}
			require.NoError(t, c.Create(ctx, w))

			stored := &Widget{}
			require.NoError(t, c.Get(ctx, key, stored))
			require.Equal(t, int64(1), stored.GetGeneration())

			// Starting the delete bumps generation, like the Kubernetes apiserver.
			require.NoError(t, c.Delete(ctx, tc.deleteBy(stored)))

			deleting := &Widget{}
			require.NoError(t, c.Get(ctx, key, deleting))
			require.NotNil(t, deleting.GetDeletionTimestamp())
			assert.Equal(t, int64(2), deleting.GetGeneration(), "generation should increment when delete starts")

			// Deleting again neither bumps generation nor moves deletionTimestamp.
			require.NoError(t, c.Delete(ctx, tc.deleteBy(deleting)))

			again := &Widget{}
			require.NoError(t, c.Get(ctx, key, again))
			assert.Equal(t, int64(2), again.GetGeneration(), "repeated delete should not increment generation")
			assert.True(t, deleting.GetDeletionTimestamp().Equal(again.GetDeletionTimestamp()),
				"repeated delete should keep the original deletionTimestamp")
		})
	}
}

func TestClient_DeleteWithoutRV(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "no-rv"}

	// Create
	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-rv"},
		Spec:       WidgetSpec{Color: "blue"},
	}
	require.NoError(t, c.Create(ctx, w))

	// Delete using a bare object (no ResourceVersion)
	bare := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-rv"},
	}
	require.NoError(t, c.Delete(ctx, bare))

	// Should be gone
	err := c.Get(ctx, key, &Widget{})
	assert.True(t, apierrors.IsNotFound(err), "expected NotFound after delete without RV, got: %v", err)
}

func TestClient_DeleteWithoutRV_NotFound(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	bare := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "nonexistent"},
	}
	err := c.Delete(ctx, bare)
	assert.True(t, apierrors.IsNotFound(err), "delete of nonexistent object should return NotFound, got: %v", err)
}

func TestClient_ListWithFieldSelector(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	for _, w := range []Widget{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "red-0"}, Spec: WidgetSpec{Color: "red"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "blue-0"}, Spec: WidgetSpec{Color: "blue"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "blue-1"}, Spec: WidgetSpec{Color: "blue"}},
	} {
		require.NoError(t, c.Create(ctx, w.DeepCopyObject().(*Widget)))
	}

	t.Run("by spec field", func(t *testing.T) {
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFields{"spec.color": "blue"}))
		assert.Len(t, list.Items, 2)
	})

	t.Run("by metadata.name", func(t *testing.T) {
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFields{"metadata.name": "red-0"}))
		assert.Len(t, list.Items, 1)
		assert.Equal(t, "red-0", list.Items[0].Name)
	})

	t.Run("by metadata.uid", func(t *testing.T) {
		red := &Widget{}
		require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "red-0"}, red))
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFields{"metadata.uid": string(red.UID)}))
		assert.Len(t, list.Items, 1)
		assert.Equal(t, red.UID, list.Items[0].UID)
	})

	t.Run("no matches", func(t *testing.T) {
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFields{"spec.color": "nonexistent"}))
		assert.Empty(t, list.Items)
	})

	t.Run("not equals", func(t *testing.T) {
		sel, err := fields.ParseSelector("spec.color!=red")
		require.NoError(t, err)
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFieldsSelector{Selector: sel}))
		assert.Len(t, list.Items, 2)
		for _, item := range list.Items {
			assert.Equal(t, "blue", item.Spec.Color)
		}
	})

	t.Run("invalid field errors", func(t *testing.T) {
		err := c.List(ctx, &WidgetList{}, client.MatchingFields{"invalid": "value"})
		assert.ErrorContains(t, err, "invalid field selector")
	})

	t.Run("combined with limit", func(t *testing.T) {
		list := &WidgetList{}
		require.NoError(t, c.List(ctx, list, client.MatchingFields{"spec.color": "blue"}, client.Limit(1)))
		assert.Len(t, list.Items, 1)
		assert.NotEmpty(t, list.Continue)
	})
}

type widgetLister interface {
	List(context.Context, client.ObjectList, ...client.ListOption) error
}

func collectRedWidgetNames(t *testing.T, ctx context.Context, lister widgetLister) []string {
	t.Helper()

	var names []string
	token := ""
	for {
		list := &WidgetList{}
		opts := []client.ListOption{
			client.MatchingLabels{"team": "red"},
			client.Limit(2),
		}
		if token != "" {
			opts = append(opts, client.Continue(token))
		}
		require.NoError(t, lister.List(ctx, list, opts...))
		for _, item := range list.Items {
			names = append(names, item.Name)
		}
		token = list.Continue
		if token == "" {
			return names
		}
	}
}

func TestClient_ListWithLabelSelectorAndPagination(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	for i := range 7 {
		team := "blue"
		if i%2 == 0 {
			team = "red"
		}
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      fmt.Sprintf("label-page-%d", i),
				Labels:    map[string]string{"team": team},
			},
			Spec: WidgetSpec{Color: team},
		}
		require.NoError(t, c.Create(ctx, w))
	}
	withoutTeam := &Widget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "label-page-unlabeled",
			Labels:    map[string]string{"app": "api"},
		},
		Spec: WidgetSpec{Color: "unlabeled"},
	}
	require.NoError(t, c.Create(ctx, withoutTeam))

	wantRed := []string{"label-page-0", "label-page-2", "label-page-4", "label-page-6"}
	assert.ElementsMatch(t, wantRed, collectRedWidgetNames(t, ctx, c))
	assert.ElementsMatch(t, wantRed, collectRedWidgetNames(t, ctx, mgr.GetCache()))

	notRed, err := labels.Parse("team notin (red)")
	require.NoError(t, err)
	list := &WidgetList{}
	require.NoError(t, c.List(ctx, list, client.MatchingLabelsSelector{Selector: notRed}))
	var notRedNames []string
	for _, item := range list.Items {
		notRedNames = append(notRedNames, item.Name)
	}
	assert.ElementsMatch(t, []string{"label-page-1", "label-page-3", "label-page-5", "label-page-unlabeled"}, notRedNames,
		"NotIn must include objects where the label key is absent")
}

func TestClient_ListWithNumericLabelSelectors(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	for name, labels := range map[string]map[string]string{
		"gt-match":            {"count": "11"},
		"lt-match":            {"count": "-1"},
		"equal":               {"count": "10"},
		"non-integer":         {"count": "ten"},
		"overflow-positive":   {"count": "9223372036854775808"},
		"overflow-negative":   {"count": "-9223372036854775809"},
		"missing-label-value": {"app": "api"},
	} {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, Labels: labels},
			Spec:       WidgetSpec{Color: name},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	for _, tt := range []struct {
		selector string
		want     string
	}{
		{selector: "count>10", want: "gt-match"},
		{selector: "count<0", want: "lt-match"},
	} {
		t.Run(tt.selector, func(t *testing.T) {
			sel, err := labels.Parse(tt.selector)
			require.NoError(t, err)
			list := &WidgetList{}
			require.NoError(t, c.List(ctx, list, client.MatchingLabelsSelector{Selector: sel}))
			require.Len(t, list.Items, 1)
			assert.Equal(t, tt.want, list.Items[0].Name)
		})
	}
}

func TestClient_ListWithLimit(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()

	for i := range 5 {
		w := &Widget{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("w-%d", i)},
			Spec:       WidgetSpec{Color: "blue"},
		}
		require.NoError(t, c.Create(ctx, w))
	}

	t.Run("paginates through all items", func(t *testing.T) {
		seen := map[string]bool{}
		token := ""
		for page := 0; ; page++ {
			list := &WidgetList{}
			opts := []client.ListOption{client.Limit(2)}
			if token != "" {
				opts = append(opts, client.Continue(token))
			}
			require.NoError(t, c.List(ctx, list, opts...))
			for _, item := range list.Items {
				seen[item.Name] = true
			}
			token = list.Continue
			if token == "" {
				break
			}
			require.Less(t, page, 10, "pagination should terminate")
		}
		assert.Len(t, seen, 5)
	})

	t.Run("invalid continue token", func(t *testing.T) {
		err := c.List(ctx, &WidgetList{}, client.Limit(10), client.Continue("bad!!!"))
		assert.ErrorContains(t, err, "invalid continue token")
	})
}

func TestManager_FullReconcileLoop(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()

	reconciled := make(chan reconcile.Request, 10)

	err := ctrl.NewControllerManagedBy(mgr).
		For(&Widget{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			reconciled <- req
			return ctrl.Result{}, nil
		}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	// Give the manager a moment to start the cache
	time.Sleep(500 * time.Millisecond)

	w := &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "trigger"},
		Spec:       WidgetSpec{Color: "green"},
	}
	require.NoError(t, c.Create(ctx, w))

	select {
	case req := <-reconciled:
		assert.Equal(t, "default", req.Namespace)
		assert.Equal(t, "trigger", req.Name)
	case <-ctx.Done():
		t.Fatal("timed out waiting for reconcile")
	}
}

func TestClient_UnsupportedOptions(t *testing.T) {
	mgr := newManager(t)
	c := mgr.GetClient()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "default", Name: "opts-test"}

	require.NoError(t, c.Create(ctx, &Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec:       WidgetSpec{Color: "blue"},
	}))

	fresh := func() *Widget {
		w := &Widget{}
		require.NoError(t, c.Get(ctx, key, w))
		return w
	}

	tests := []struct {
		name, errContains string
		fn                func() error
	}{
		{"Create DryRun", "DryRun not supported", func() error {
			return c.Create(ctx, &Widget{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dry"},
				Spec:       WidgetSpec{Color: "blue"},
			}, client.DryRunAll)
		}},
		{"Create GenerateName", "GenerateName not supported", func() error {
			return c.Create(ctx, &Widget{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "widget-"},
				Spec:       WidgetSpec{Color: "blue"},
			})
		}},
		{"Update DryRun", "DryRun not supported", func() error {
			return c.Update(ctx, fresh(), client.DryRunAll)
		}},
		{"Delete DryRun", "DryRun not supported", func() error {
			return c.Delete(ctx, fresh(), client.DryRunAll)
		}},
		{"Delete GracePeriod", "GracePeriodSeconds not supported", func() error {
			return c.Delete(ctx, fresh(), client.GracePeriodSeconds(30))
		}},
		{"Delete PropagationPolicy", "PropagationPolicy not supported", func() error {
			return c.Delete(ctx, fresh(), client.PropagationPolicy(metav1.DeletePropagationForeground))
		}},
		{"Status Update DryRun", "DryRun not supported", func() error {
			return c.Status().Update(ctx, fresh(), &client.SubResourceUpdateOptions{
				UpdateOptions: client.UpdateOptions{DryRun: []string{metav1.DryRunAll}},
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorContains(t, tt.fn(), tt.errContains)
		})
	}
}
