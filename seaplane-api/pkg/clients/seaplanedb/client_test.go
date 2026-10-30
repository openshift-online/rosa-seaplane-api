package seaplanedb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = seaplanev1alpha1.AddToScheme(s)
	return s
}

func ctxAccount(id string) context.Context {
	return context.WithValue(context.Background(), middleware.ContextKeyAccountID, id)
}

func newUIDIndexedClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithIndex(&seaplanev1alpha1.DNSReservation{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&seaplanev1alpha1.OidcConfig{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithStatusSubresource(&seaplanev1alpha1.DNSReservation{}, &seaplanev1alpha1.OidcConfig{}).
		Build()
}

func testNodePoolFor(parent *seaplanev1alpha1.Cluster) *seaplanev1alpha1.NodePool {
	return &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      parent.Name + ".workers",
			Namespace: parent.Namespace,
			Labels: map[string]string{
				accountIDLabel:  parent.Labels[accountIDLabel],
				clusterUIDLabel: string(parent.UID),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			AccountID: parent.Spec.AccountID,
			NodePool:  seaplanev1alpha1.NodePoolSpecPassthrough{ClusterName: parent.Name},
		},
	}
}

func TestClient_CreateCluster_KeepsScopedIdentity(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	c := NewClientFrom(fc, logger)

	// The conversion scopes the candidate; create keeps it and fills in spec.accountId.
	cluster := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster",
			Namespace: accountNamespace("acct-123"),
			Labels:    map[string]string{accountIDLabel: "acct-123"},
		},
	}

	if err := c.CreateCluster(ctxAccount("acct-123"), cluster); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	if cluster.Labels[accountIDLabel] != "acct-123" || cluster.Namespace != accountNamespace("acct-123") || cluster.Spec.AccountID != "acct-123" {
		t.Errorf("created identity = %q/%q/%q, want acct-123", cluster.Namespace, cluster.Labels[accountIDLabel], cluster.Spec.AccountID)
	}
}

func TestClient_CreateNodePool_KeepsScopedIdentity(t *testing.T) {
	parent := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "parent", Namespace: "account-acct-123", UID: types.UID("parent-uid"),
		Labels: map[string]string{accountIDLabel: "acct-123"},
	}}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(parent).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	c := NewClientFrom(fc, logger)

	np := testNodePoolFor(parent)
	np.Namespace = accountNamespace("acct-123")
	np.Labels[accountIDLabel] = "acct-123"
	np.Spec.AccountID = ""
	if err := c.CreateNodePool(ctxAccount("acct-123"), np); err != nil {
		t.Fatalf("CreateNodePool: %v", err)
	}

	if got := np.Labels[accountIDLabel]; got != "acct-123" {
		t.Errorf("account-id label = %q, want %q", got, "acct-123")
	}
	if np.Namespace != accountNamespace("acct-123") {
		t.Errorf("namespace = %q, want %q", np.Namespace, accountNamespace("acct-123"))
	}
	if np.Spec.AccountID != "acct-123" {
		t.Errorf("spec.accountID = %q, want %q", np.Spec.AccountID, "acct-123")
	}
}

func TestClient_CreateNodePool_RequiresOwnedParent(t *testing.T) {
	foreign := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: "account-acct-2", UID: types.UID("foreign-uid"),
		Labels: map[string]string{accountIDLabel: "acct-2"},
	}}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(foreign).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	np := testNodePoolFor(foreign)
	np.Namespace = accountNamespace("acct-1")
	np.Labels[accountIDLabel] = "acct-1"
	np.Spec.AccountID = ""
	if err := c.CreateNodePool(ctxAccount("acct-1"), np); !IsNotFound(err) {
		t.Fatalf("CreateNodePool into foreign parent: want NotFound, got %v", err)
	}
}

// An update never moves ownership: a snapshot whose owner differs from the caller is rejected.
func TestClient_Update_RejectsForeignOwner(t *testing.T) {
	meta := metav1.ObjectMeta{Name: "owned", Namespace: "account-acct-1", Labels: map[string]string{accountIDLabel: "acct-1"}}
	cr := &seaplanev1alpha1.Cluster{ObjectMeta: *meta.DeepCopy()}
	np := &seaplanev1alpha1.NodePool{ObjectMeta: *meta.DeepCopy()}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cr, np).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	for name, update := range map[string]func() error{
		"cluster":  func() error { return c.UpdateCluster(ctxAccount("acct-2"), cr.DeepCopy()) },
		"nodepool": func() error { return c.UpdateNodePool(ctxAccount("acct-2"), np.DeepCopy()) },
	} {
		if err := update(); err == nil {
			t.Errorf("%s: foreign caller updated acct-1 object", name)
		}
	}
	var stored seaplanev1alpha1.Cluster
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(cr), &stored); err != nil || stored.Labels[accountIDLabel] != "acct-1" {
		t.Fatalf("cluster owner = %q (%v), want acct-1", stored.Labels[accountIDLabel], err)
	}
}

func TestClient_ListClusters_FiltersByAccount(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		&seaplanev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-cluster", Namespace: "account-acct-1",
				Labels: map[string]string{accountIDLabel: "acct-1"},
			},
		},
		&seaplanev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "other-cluster", Namespace: "account-acct-2",
				Labels: map[string]string{accountIDLabel: "acct-2"},
			},
		},
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	c := NewClientFrom(fc, logger)

	list, err := c.ListClusters(ctxAccount("acct-1"))
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 cluster for acct-1, got %d", len(list.Items))
	}
	if list.Items[0].Name != "my-cluster" {
		t.Errorf("got cluster %q, want my-cluster", list.Items[0].Name)
	}
}

func TestClient_GetNodePool_ScopedToAccount(t *testing.T) {
	alpha := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "alpha", Namespace: "account-acct-1", UID: types.UID("alpha-uid"),
		Labels: map[string]string{accountIDLabel: "acct-1"},
	}}
	beta := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "beta", Namespace: "account-acct-1", UID: types.UID("beta-uid"),
		Labels: map[string]string{accountIDLabel: "acct-1"},
	}}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		alpha, beta, testNodePoolFor(alpha), testNodePoolFor(beta),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	c := NewClientFrom(fc, logger)

	np, err := c.GetNodePool(ctxAccount("acct-1"), "beta.workers")
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	if np.Namespace != "account-acct-1" {
		t.Errorf("namespace = %q, want account-acct-1", np.Namespace)
	}
}

func TestClient_GetNodePool_NotFoundOutsideAccount(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		&seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
			Name: "one", Namespace: "account-acct-1", Labels: map[string]string{accountIDLabel: "acct-1"},
		}},
		&seaplanev1alpha1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "alpha.workers", Namespace: "account-acct-2",
				Labels: map[string]string{accountIDLabel: "acct-1"},
			},
		},
	).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	_, err := c.GetNodePool(ctxAccount("acct-1"), "alpha.workers")
	if !IsNotFound(err) {
		t.Fatalf("GetNodePool across account namespace: expected NotFound, got %v", err)
	}
}

func TestClient_NodePoolReadsIgnoreParent(t *testing.T) {
	ctx := ctxAccount("acct-1")
	owned := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "owned", Namespace: "account-acct-1", UID: types.UID("owned-uid"),
			Labels: map[string]string{accountIDLabel: "acct-1"},
		},
	}
	foreign := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foreign", Namespace: "account-acct-2", UID: types.UID("foreign-uid"),
			Labels: map[string]string{accountIDLabel: "acct-2"},
		},
	}
	forged := testNodePoolFor(foreign)
	forged.Namespace = owned.Namespace
	forged.Labels[accountIDLabel] = "acct-1"
	stale := testNodePoolFor(owned)
	stale.Name = "owned.stale"
	stale.OwnerReferences[0].UID = types.UID("old-uid")
	stale.Labels[clusterUIDLabel] = "old-uid"
	unbound := testNodePoolFor(owned)
	unbound.Name = "owned.unbound"
	unbound.OwnerReferences = nil

	for _, tc := range []struct {
		name string
		pool *seaplanev1alpha1.NodePool
	}{
		{"foreign parent with forged account label", forged},
		{"previous parent incarnation", stale},
		{"missing parent binding", unbound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := testNodePoolFor(owned)
			fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(owned, foreign, valid, tc.pool).Build()
			c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

			// Storage reads stay namespace-scoped; handlers check the parent while authorizing.
			if _, err := c.GetNodePool(ctx, tc.pool.Name); err != nil {
				t.Errorf("GetNodePool(%q): %v", tc.pool.Name, err)
			}
			list, err := c.ListNodePools(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			// List is scoped by namespace only; stale parents are a deletion bug, not a list filter.
			if len(list.Items) != 2 {
				t.Errorf("account list should contain both account nodepools, got %v", list.Items)
			}
			if _, err := c.GetNodePool(ctx, valid.Name); err != nil {
				t.Errorf("owned nodepool should remain accessible: %v", err)
			}
		})
	}
}

func TestNodePoolAccountIsolation(t *testing.T) {
	ctx := ctxAccount("acct-1")
	owned := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "own", Namespace: "account-acct-1", UID: types.UID("own-uid"),
		Labels: map[string]string{accountIDLabel: "acct-1"},
	}}
	foreign := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "other", Namespace: "account-acct-2", UID: types.UID("other-uid"),
		Labels: map[string]string{accountIDLabel: "acct-2"},
	}}
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		owned, foreign, testNodePoolFor(owned), testNodePoolFor(foreign),
	).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if _, err := c.GetNodePool(ctx, "other.workers"); !IsNotFound(err) {
		t.Errorf("GetNodePool should hide other account's nodepool, got %v", err)
	}
	for _, clusterUID := range []string{"other-uid", ""} {
		list, err := c.ListNodePools(ctx, clusterUID)
		if err != nil {
			t.Fatalf("ListNodePools(clusterUID=%q): %v", clusterUID, err)
		}
		if clusterUID != "" && len(list.Items) != 0 {
			t.Errorf("scoped list should hide other account's nodepool, got %v", list.Items)
		}
		if clusterUID == "" && (len(list.Items) != 1 || list.Items[0].Name != "own.workers") {
			t.Errorf("account list should contain only own.workers, got %v", list.Items)
		}
	}
}

func TestClient_GetCluster_ScopedToAccount(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		&seaplanev1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-cluster", Namespace: "account-acct-1",
				Labels: map[string]string{accountIDLabel: "acct-1"},
			},
		},
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	c := NewClientFrom(fc, logger)

	// Correct account can access
	cr, err := c.GetCluster(ctxAccount("acct-1"), "my-cluster")
	if err != nil {
		t.Fatalf("GetCluster with correct account: %v", err)
	}
	if cr.Name != "my-cluster" {
		t.Errorf("got cluster %q, want my-cluster", cr.Name)
	}

	// Wrong account gets not-found
	_, err = c.GetCluster(ctxAccount("acct-2"), "my-cluster")
	if !IsNotFound(err) {
		t.Errorf("GetCluster with wrong account: expected NotFound, got %v", err)
	}
}

func TestClient_ClusterUpdateAndDelete(t *testing.T) {
	ctx := ctxAccount("acct-1")
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		&seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "update-me", Namespace: "account-acct-1"}},
		&seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "delete-object", Namespace: "account-acct-1"}},
	).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	cluster, err := c.GetCluster(ctx, "update-me")
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	cluster.Labels = map[string]string{"updated": "true"}
	if err := c.UpdateCluster(ctx, cluster); err != nil {
		t.Fatalf("UpdateCluster: %v", err)
	}
	updated, err := c.GetCluster(ctx, "update-me")
	if err != nil || updated.Labels["updated"] != "true" {
		t.Fatalf("updated Cluster = %+v, err = %v", updated, err)
	}

	if err := c.DeleteCluster(ctx, "update-me"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if _, err := c.GetCluster(ctx, "update-me"); !IsNotFound(err) {
		t.Fatalf("GetCluster after DeleteCluster error = %v, want NotFound", err)
	}

	directDelete, err := c.GetCluster(ctx, "delete-object")
	if err != nil {
		t.Fatalf("GetCluster before DeleteClusterObject: %v", err)
	}
	if err := c.DeleteClusterObject(ctx, directDelete); err != nil {
		t.Fatalf("DeleteClusterObject: %v", err)
	}
	if _, err := c.GetCluster(ctx, "delete-object"); !IsNotFound(err) {
		t.Fatalf("GetCluster after DeleteClusterObject error = %v, want NotFound", err)
	}
	if err := c.DeleteCluster(ctx, "missing"); !IsNotFound(err) {
		t.Fatalf("DeleteCluster missing object error = %v, want NotFound", err)
	}
}

func TestClient_NodePoolListUpdateAndDelete(t *testing.T) {
	ctx := ctxAccount("acct-1")
	alpha := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "alpha", Namespace: "account-acct-1", UID: types.UID("cluster-1"),
		Labels: map[string]string{accountIDLabel: "acct-1"},
	}}
	beta := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name: "beta", Namespace: "account-acct-1", UID: types.UID("cluster-2"),
		Labels: map[string]string{accountIDLabel: "acct-1"},
	}}
	foreign := testNodePoolFor(alpha)
	foreign.Name = "foreign.workers"
	foreign.Namespace = "account-acct-2"
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		alpha, beta, testNodePoolFor(alpha), testNodePoolFor(beta), foreign,
	).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	filtered, err := c.ListNodePools(ctx, "cluster-1")
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Name != "alpha.workers" {
		t.Fatalf("ListNodePools by cluster UID = %+v, err = %v", filtered, err)
	}
	all, err := c.ListNodePools(ctx, "")
	if err != nil || len(all.Items) != 2 {
		t.Fatalf("ListNodePools without cluster filter returned %d items, err = %v", len(all.Items), err)
	}

	nodepool, err := c.GetNodePool(ctx, "alpha.workers")
	if err != nil {
		t.Fatalf("GetNodePool: %v", err)
	}
	nodepool.Labels["updated"] = "true"
	if err := c.UpdateNodePool(ctx, nodepool); err != nil {
		t.Fatalf("UpdateNodePool: %v", err)
	}
	updated, err := c.GetNodePool(ctx, "alpha.workers")
	if err != nil || updated.Labels["updated"] != "true" {
		t.Fatalf("updated NodePool = %+v, err = %v", updated, err)
	}

	if err := c.DeleteNodePool(ctx, "alpha.workers"); err != nil {
		t.Fatalf("DeleteNodePool: %v", err)
	}
	if _, err := c.GetNodePool(ctx, "alpha.workers"); !IsNotFound(err) {
		t.Fatalf("GetNodePool after DeleteNodePool error = %v, want NotFound", err)
	}
	if err := c.DeleteNodePool(ctx, "missing.workers"); !IsNotFound(err) {
		t.Fatalf("DeleteNodePool missing object error = %v, want NotFound", err)
	}
}

func TestClient_DNSReservationOperations(t *testing.T) {
	ctx := ctxAccount("acct-1")
	fc := newUIDIndexedClient(t)
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{Name: "dns-one", UID: types.UID("dns-uid-1"), Namespace: accountNamespace("acct-1"), Labels: map[string]string{accountIDLabel: "acct-1"}},
	}

	if err := c.CreateDNSReservation(ctx, reservation); err != nil {
		t.Fatalf("CreateDNSReservation: %v", err)
	}
	if reservation.Namespace != accountNamespace("acct-1") || reservation.Labels[accountIDLabel] != "acct-1" {
		t.Fatalf("created reservation namespace/labels = %q/%v", reservation.Namespace, reservation.Labels)
	}
	byName, err := c.GetDNSReservation(ctx, "dns-one")
	if err != nil {
		t.Fatalf("GetDNSReservation: %v", err)
	}
	byUID, err := c.GetDNSReservationByUID(ctx, "dns-uid-1")
	if err != nil || byUID.Name != byName.Name {
		t.Fatalf("GetDNSReservationByUID = %+v, err = %v", byUID, err)
	}
	list, err := c.ListDNSReservations(ctx)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("ListDNSReservations returned %d items, err = %v", len(list.Items), err)
	}

	byName.Labels["example.com/updated"] = "true"
	if err := c.UpdateDNSReservation(ctx, byName); err != nil {
		t.Fatalf("UpdateDNSReservation: %v", err)
	}
	byName.Status.Phase = seaplanev1alpha1.DNSReservationPhaseReady
	byName.Status.BaseDomain = "dns.example.com"
	if err := c.UpdateDNSReservationStatus(ctx, byName); err != nil {
		t.Fatalf("UpdateDNSReservationStatus: %v", err)
	}
	updated, err := c.GetDNSReservation(ctx, "dns-one")
	if err != nil || updated.Status.BaseDomain != "dns.example.com" || updated.Labels["example.com/updated"] != "true" {
		t.Fatalf("updated DNSReservation = %+v, err = %v", updated, err)
	}

	if _, err := c.GetDNSReservationByUID(ctx, "missing-uid"); !IsNotFound(err) {
		t.Fatalf("GetDNSReservationByUID missing UID error = %v, want NotFound", err)
	}
	if err := c.DeleteDNSReservation(ctx, "dns-one"); err != nil {
		t.Fatalf("DeleteDNSReservation: %v", err)
	}
	if _, err := c.GetDNSReservation(ctx, "dns-one"); !IsNotFound(err) {
		t.Fatalf("GetDNSReservation after delete error = %v, want NotFound", err)
	}

	second := &seaplanev1alpha1.DNSReservation{ObjectMeta: metav1.ObjectMeta{Name: "dns-two", UID: types.UID("dns-uid-2"), Namespace: accountNamespace("acct-1"), Labels: map[string]string{accountIDLabel: "acct-1"}}}
	if err := c.CreateDNSReservation(ctx, second); err != nil {
		t.Fatalf("CreateDNSReservation second: %v", err)
	}
	if err := c.DeleteDNSReservationObject(ctx, second); err != nil {
		t.Fatalf("DeleteDNSReservationObject: %v", err)
	}
}

func TestClient_ManifestAndManagementClusterOperations(t *testing.T) {
	ctx := context.Background()
	fc := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))

	manifest := &seaplanev1alpha1.Manifest{ObjectMeta: metav1.ObjectMeta{Name: "my-manifest"}}
	if err := c.CreateManifest(ctx, "management", manifest); err != nil {
		t.Fatalf("CreateManifest: %v", err)
	}
	if manifest.Namespace != "management" {
		t.Fatalf("Manifest namespace = %q, want management", manifest.Namespace)
	}
	if _, err := c.GetManifest(ctx, "management", "my-manifest"); err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if err := c.DeleteManifest(ctx, "management", "my-manifest"); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if _, err := c.GetManifest(ctx, "management", "my-manifest"); !IsNotFound(err) {
		t.Fatalf("GetManifest after delete error = %v, want NotFound", err)
	}

	managementCluster := &seaplanev1alpha1.ManagementCluster{ObjectMeta: metav1.ObjectMeta{Name: "mc-one", Namespace: "ignored"}}
	if err := c.CreateManagementCluster(ctx, managementCluster); err != nil {
		t.Fatalf("CreateManagementCluster: %v", err)
	}
	if managementCluster.Namespace != "" {
		t.Fatalf("ManagementCluster namespace = %q, want cluster-scoped empty namespace", managementCluster.Namespace)
	}
	var stored seaplanev1alpha1.ManagementCluster
	if err := fc.Get(ctx, client.ObjectKey{Name: "mc-one"}, &stored); err != nil {
		t.Fatalf("stored ManagementCluster: %v", err)
	}
}

func TestClient_OidcConfigAndIssuerIndexOperations(t *testing.T) {
	ctx := ctxAccount("acct-1")
	fc := newUIDIndexedClient(t)
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	config := &seaplanev1alpha1.OidcConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-one", UID: types.UID("oidc-uid-1"), Namespace: accountNamespace("acct-1"), Labels: map[string]string{accountIDLabel: "acct-1"}},
		Spec: seaplanev1alpha1.OidcConfigSpec{
			Type:      seaplanev1alpha1.OidcConfigTypeManaged,
			AccountID: "acct-1",
		},
	}
	if err := c.CreateOidcConfig(ctx, config); err != nil {
		t.Fatalf("CreateOidcConfig: %v", err)
	}
	if config.Namespace != accountNamespace("acct-1") || config.Labels[accountIDLabel] != "acct-1" {
		t.Fatalf("created OIDC config namespace/labels = %q/%v", config.Namespace, config.Labels)
	}
	byName, err := c.GetOidcConfig(ctx, "oidc-one")
	if err != nil {
		t.Fatalf("GetOidcConfig: %v", err)
	}
	byUID, err := c.GetOidcConfigByUID(ctx, "oidc-uid-1")
	if err != nil || byUID.Name != byName.Name {
		t.Fatalf("GetOidcConfigByUID = %+v, err = %v", byUID, err)
	}
	list, err := c.ListOidcConfigs(ctx)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("ListOidcConfigs returned %d items, err = %v", len(list.Items), err)
	}
	if _, err := c.GetOidcConfigByUID(ctx, "missing-uid"); !IsNotFound(err) {
		t.Fatalf("GetOidcConfigByUID missing UID error = %v, want NotFound", err)
	}

	timestamp := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	if err := c.UpdateOidcConfigLastUsedTimestamp(ctx, "oidc-uid-1", timestamp); err != nil {
		t.Fatalf("UpdateOidcConfigLastUsedTimestamp: %v", err)
	}
	byName, err = c.GetOidcConfig(ctx, "oidc-one")
	if err != nil || byName.Status.LastUsedTimestamp == nil || !byName.Status.LastUsedTimestamp.Equal(&timestamp) {
		t.Fatalf("lastUsedTimestamp = %v, err = %v", byName.Status.LastUsedTimestamp, err)
	}
	byName.Labels[seaplanev1alpha1.ClaimedByClusterUIDLabel] = "cluster-uid"
	if err := c.UpdateOidcConfigObject(ctx, byName); err != nil {
		t.Fatalf("UpdateOidcConfigObject: %v", err)
	}
	if err := c.DeleteOidcConfig(ctx, "oidc-one"); err != nil {
		t.Fatalf("DeleteOidcConfig: %v", err)
	}
	if _, err := c.GetOidcConfig(ctx, "oidc-one"); !IsNotFound(err) {
		t.Fatalf("GetOidcConfig after delete error = %v, want NotFound", err)
	}

	second := &seaplanev1alpha1.OidcConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "oidc-two", UID: types.UID("oidc-uid-2"), Namespace: accountNamespace("acct-1"), Labels: map[string]string{accountIDLabel: "acct-1"}},
		Spec:       seaplanev1alpha1.OidcConfigSpec{Type: seaplanev1alpha1.OidcConfigTypeManaged},
	}
	if err := c.CreateOidcConfig(ctx, second); err != nil {
		t.Fatalf("CreateOidcConfig second: %v", err)
	}
	if err := c.DeleteOidcConfigObject(ctx, second); err != nil {
		t.Fatalf("DeleteOidcConfigObject: %v", err)
	}

	index := &seaplanev1alpha1.Index{ObjectMeta: metav1.ObjectMeta{
		Name:      "issuer-index",
		Namespace: seaplanev1alpha1.OidcIssuerReservationsNamespace,
	}}
	if err := fc.Create(ctx, index); err != nil {
		t.Fatalf("create issuer index: %v", err)
	}
	if _, err := c.GetOidcIssuerIndex(ctx, index.Name); err != nil {
		t.Fatalf("GetOidcIssuerIndex: %v", err)
	}
	if _, err := c.GetOidcIssuerIndex(ctx, "missing-index"); !IsNotFound(err) {
		t.Fatalf("GetOidcIssuerIndex missing error = %v, want NotFound", err)
	}
}

func TestClientClose(t *testing.T) {
	closed := false
	c := &Client{close: func() { closed = true }}
	c.Close()
	if !closed {
		t.Error("Close did not invoke the underlying cleanup")
	}
}

func TestClientErrorPredicates(t *testing.T) {
	resource := schema.GroupResource{Group: "seaplane.io", Resource: "clusters"}
	if !IsAlreadyExists(apierrors.NewAlreadyExists(resource, "my-cluster")) {
		t.Error("IsAlreadyExists did not recognize an AlreadyExists error")
	}
	if !IsConflict(apierrors.NewConflict(resource, "my-cluster", fmt.Errorf("resource version changed"))) {
		t.Error("IsConflict did not recognize a Conflict error")
	}
	if IsAlreadyExists(fmt.Errorf("ordinary error")) || IsConflict(fmt.Errorf("ordinary error")) {
		t.Error("error predicates matched an ordinary error")
	}
}

func TestNewClient_RequiresDSN(t *testing.T) {
	_, err := NewClient(context.Background(), "", slog.Default())
	if err == nil || !strings.Contains(err.Error(), "DSN is required") {
		t.Fatalf("NewClient error = %v, want missing DSN error", err)
	}
}

const (
	spotQueueTestAccountID = "123456789012"
	spotQueueTestURL       = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
)

func spotQueueTestReservation(owner string, claimedAt time.Time) *seaplanev1alpha1.Index {
	return &seaplanev1alpha1.Index{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
			Name:      seaplanev1alpha1.SpotTerminationQueueIndexName(spotQueueTestAccountID, spotQueueTestURL),
			Labels:    map[string]string{seaplanev1alpha1.SpotTerminationQueueOwnerLabel: owner},
			Annotations: map[string]string{
				seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation: claimedAt.UTC().Format(time.RFC3339Nano),
			},
		},
	}
}

func newSpotQueueTestClient(objects ...client.Object) (*Client, client.Client) {
	fc := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
	return NewClientFrom(fc, slog.New(slog.NewTextHandler(os.Stdout, nil))), fc
}

func TestClient_ClaimSpotTerminationQueueCreatesReservation(t *testing.T) {
	c, fc := newSpotQueueTestClient()
	created, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1")
	if err != nil || !created {
		t.Fatalf("ClaimSpotTerminationQueue() = (%t, %v), want (true, nil)", created, err)
	}

	var reservation seaplanev1alpha1.Index
	key := types.NamespacedName{
		Namespace: seaplanev1alpha1.SpotTerminationQueueReservationsNamespace,
		Name:      seaplanev1alpha1.SpotTerminationQueueIndexName(spotQueueTestAccountID, spotQueueTestURL),
	}
	if err := fc.Get(context.Background(), key, &reservation); err != nil {
		t.Fatalf("getting created reservation: %v", err)
	}
	if got := reservation.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel]; got != spotQueueOwnerKey("cluster-1") {
		t.Errorf("reservation owner = %q, want %q", got, spotQueueOwnerKey("cluster-1"))
	}
	if _, err := time.Parse(time.RFC3339Nano, reservation.Annotations[seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation]); err != nil {
		t.Errorf("reservation timestamp is invalid: %v", err)
	}
}

func TestClient_ClaimSpotTerminationQueueRefreshesSameOwner(t *testing.T) {
	reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-1"), time.Now().Add(-time.Hour))
	reservation.Annotations = nil
	c, fc := newSpotQueueTestClient(reservation)
	created, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1")
	if err != nil || created {
		t.Fatalf("ClaimSpotTerminationQueue() = (%t, %v), want (false, nil)", created, err)
	}

	var updated seaplanev1alpha1.Index
	key := types.NamespacedName{Namespace: reservation.Namespace, Name: reservation.Name}
	if err := fc.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("getting refreshed reservation: %v", err)
	}
	if updated.Annotations[seaplanev1alpha1.SpotTerminationQueueClaimedAtAnnotation] == "" {
		t.Error("expected refresh to initialize the claim timestamp")
	}
}

func TestClient_ClaimSpotTerminationQueueRejectsActiveOwner(t *testing.T) {
	reservation := spotQueueTestReservation(spotQueueOwnerKey("owner"), time.Now())
	owner := spotQueueTestCluster("owner", spotQueueTestURL)
	c, _ := newSpotQueueTestClient(reservation, owner)
	created, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "other")
	if created || !IsAlreadyExists(err) {
		t.Fatalf("ClaimSpotTerminationQueue() = (%t, %v), want an already-exists error", created, err)
	}
}

func TestClient_ClaimSpotTerminationQueueReclaimsExpiredReservation(t *testing.T) {
	reservation := spotQueueTestReservation(spotQueueOwnerKey("deleted-cluster"), time.Now().Add(-2*spotQueueClaimTimeout))
	c, fc := newSpotQueueTestClient(reservation)
	created, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-new")
	if err != nil || !created {
		t.Fatalf("ClaimSpotTerminationQueue() = (%t, %v), want (true, nil)", created, err)
	}

	var current seaplanev1alpha1.Index
	key := types.NamespacedName{Namespace: reservation.Namespace, Name: reservation.Name}
	if err := fc.Get(context.Background(), key, &current); err != nil {
		t.Fatalf("getting reclaimed reservation: %v", err)
	}
	if got := current.Labels[seaplanev1alpha1.SpotTerminationQueueOwnerLabel]; got != spotQueueOwnerKey("cluster-new") {
		t.Errorf("reservation owner = %q, want %q", got, spotQueueOwnerKey("cluster-new"))
	}
}

func TestClient_ClaimSpotTerminationQueueKeepsRecentReservation(t *testing.T) {
	reservation := spotQueueTestReservation(spotQueueOwnerKey("deleted-cluster"), time.Now())
	c, _ := newSpotQueueTestClient(reservation)
	created, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-new")
	if created || !IsAlreadyExists(err) {
		t.Fatalf("ClaimSpotTerminationQueue() = (%t, %v), want a recent-reservation conflict", created, err)
	}
}

func TestClient_ClaimSpotTerminationQueueReturnsClientErrors(t *testing.T) {
	wantErr := errors.New("storage failure")
	t.Run("create", func(t *testing.T) {
		c := NewClientFrom(fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return wantErr },
		}).Build(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if _, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); !errors.Is(err, wantErr) {
			t.Fatalf("ClaimSpotTerminationQueue() error = %v, want %v", err, wantErr)
		}
	})
	t.Run("get existing reservation", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("owner"), time.Now())
		c := NewClientFrom(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(reservation).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, types.NamespacedName, client.Object, ...client.GetOption) error {
				return wantErr
			},
		}).Build(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if _, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); !errors.Is(err, wantErr) {
			t.Fatalf("ClaimSpotTerminationQueue() error = %v, want %v", err, wantErr)
		}
	})
	t.Run("refresh existing reservation", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-1"), time.Now())
		c := NewClientFrom(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(reservation).WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return wantErr },
		}).Build(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if _, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); !errors.Is(err, wantErr) {
			t.Fatalf("ClaimSpotTerminationQueue() error = %v, want %v", err, wantErr)
		}
	})
	t.Run("release stale reservation", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("deleted-cluster"), time.Now().Add(-2*spotQueueClaimTimeout))
		c := NewClientFrom(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(reservation).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return wantErr },
		}).Build(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if _, err := c.ClaimSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); !errors.Is(err, wantErr) {
			t.Fatalf("ClaimSpotTerminationQueue() error = %v, want %v", err, wantErr)
		}
	})
}

func TestSpotQueueHelpers(t *testing.T) {
	if !spotQueueClaimExpired(spotQueueTestReservation("owner", time.Now().Add(-2*spotQueueClaimTimeout))) {
		t.Error("expected old claim to be expired")
	}
	missingTimestamp := spotQueueTestReservation("owner", time.Time{})
	missingTimestamp.Annotations = nil
	if spotQueueClaimExpired(missingTimestamp) {
		t.Error("claim without a timestamp must not be expired")
	}
	if clusterUsesSpotTerminationQueue(nil, spotQueueTestURL) {
		t.Error("nil cluster must not use the queue")
	}
	if clusterUsesSpotTerminationQueue(&seaplanev1alpha1.Cluster{}, spotQueueTestURL) {
		t.Error("cluster without AWS platform must not use the queue")
	}
	cluster := &seaplanev1alpha1.Cluster{Spec: seaplanev1alpha1.ClusterSpec{HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
		Platform: hypershiftPlatformWithQueue(spotQueueTestURL),
	}}}
	if !clusterUsesSpotTerminationQueue(cluster, spotQueueTestURL) || clusterUsesSpotTerminationQueue(cluster, "other") {
		t.Error("queue usage helper returned an unexpected result")
	}
}

func TestClient_ReleaseSpotTerminationQueue(t *testing.T) {
	t.Run("missing reservation", func(t *testing.T) {
		c, _ := newSpotQueueTestClient()
		if err := c.ReleaseSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); err != nil {
			t.Fatalf("ReleaseSpotTerminationQueue() error = %v, want nil", err)
		}
	})
	t.Run("wrong owner", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-other"), time.Now())
		c, _ := newSpotQueueTestClient(reservation)
		if err := c.ReleaseSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); err == nil {
			t.Fatal("ReleaseSpotTerminationQueue() error = nil, want ownership error")
		}
	})
	t.Run("cluster still uses queue", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-1"), time.Now())
		cluster := spotQueueTestCluster("cluster-1", spotQueueTestURL)
		c, fc := newSpotQueueTestClient(reservation, cluster)
		if err := c.ReleaseSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); err != nil {
			t.Fatalf("ReleaseSpotTerminationQueue() error = %v, want nil", err)
		}
		var found seaplanev1alpha1.Index
		if err := fc.Get(context.Background(), types.NamespacedName{Namespace: reservation.Namespace, Name: reservation.Name}, &found); err != nil {
			t.Errorf("reservation was deleted while cluster still uses it: %v", err)
		}
	})
	t.Run("cluster no longer uses queue", func(t *testing.T) {
		reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-1"), time.Now())
		cluster := spotQueueTestCluster("cluster-1", "")
		c, fc := newSpotQueueTestClient(reservation, cluster)
		if err := c.ReleaseSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); err != nil {
			t.Fatalf("ReleaseSpotTerminationQueue() error = %v, want nil", err)
		}
		var found seaplanev1alpha1.Index
		if err := fc.Get(context.Background(), types.NamespacedName{Namespace: reservation.Namespace, Name: reservation.Name}, &found); !IsNotFound(err) {
			t.Errorf("reservation still exists after release: %v", err)
		}
	})
	t.Run("delete failure", func(t *testing.T) {
		wantErr := errors.New("delete failure")
		reservation := spotQueueTestReservation(spotQueueOwnerKey("cluster-1"), time.Now())
		cluster := spotQueueTestCluster("cluster-1", "")
		c := NewClientFrom(fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(reservation, cluster).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return wantErr },
		}).Build(), slog.New(slog.NewTextHandler(os.Stdout, nil)))
		if err := c.ReleaseSpotTerminationQueue(ctxAccount(spotQueueTestAccountID), spotQueueTestURL, "cluster-1"); !errors.Is(err, wantErr) {
			t.Fatalf("ReleaseSpotTerminationQueue() error = %v, want %v", err, wantErr)
		}
	})
}

func spotQueueTestCluster(clusterName, queueURL string) *seaplanev1alpha1.Cluster {
	return &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: accountNamespace(spotQueueTestAccountID),
			Labels:    map[string]string{accountIDLabel: spotQueueTestAccountID},
		},
		Spec: seaplanev1alpha1.ClusterSpec{HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
			Platform: hypershiftPlatformWithQueue(queueURL),
		}},
	}
}

func hypershiftPlatformWithQueue(queueURL string) hypershiftv1beta1.PlatformSpec {
	return hypershiftv1beta1.PlatformSpec{
		AWS: &hypershiftv1beta1.AWSPlatformSpec{TerminationHandlerQueueURL: queueURL},
	}
}

// Creates reject a candidate whose identity differs from the caller, instead of rewriting it.
func TestClient_CreateRejectsForeignCandidate(t *testing.T) {
	ctx := ctxAccount("acct-1")
	foreign := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: "x", Namespace: accountNamespace("acct-2"), Labels: map[string]string{accountIDLabel: "acct-2"}}
	}
	relabeled := metav1.ObjectMeta{Name: "x", Namespace: accountNamespace("acct-1"), Labels: map[string]string{accountIDLabel: "acct-2"}}
	fc := newUIDIndexedClient(t)
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, create := range map[string]func() error{
		"cluster":       func() error { return c.CreateCluster(ctx, &seaplanev1alpha1.Cluster{ObjectMeta: foreign()}) },
		"cluster label": func() error { return c.CreateCluster(ctx, &seaplanev1alpha1.Cluster{ObjectMeta: relabeled}) },
		"cluster spec": func() error {
			cr := &seaplanev1alpha1.Cluster{ObjectMeta: *relabeled.DeepCopy()}
			cr.Labels[accountIDLabel] = "acct-1"
			cr.Spec.AccountID = "acct-2"
			return c.CreateCluster(ctx, cr)
		},
		"nodepool":    func() error { return c.CreateNodePool(ctx, &seaplanev1alpha1.NodePool{ObjectMeta: foreign()}) },
		"oidc config": func() error { return c.CreateOidcConfig(ctx, &seaplanev1alpha1.OidcConfig{ObjectMeta: foreign()}) },
		"dns reservation": func() error {
			return c.CreateDNSReservation(ctx, &seaplanev1alpha1.DNSReservation{ObjectMeta: foreign()})
		},
	} {
		if err := create(); err == nil {
			t.Errorf("%s: foreign candidate was written", name)
		}
	}
}

// Every update validates the caller owns the snapshot, as Cluster and NodePool updates do.
func TestClient_UpdatesValidateOwnedSnapshot(t *testing.T) {
	ctx := ctxAccount("acct-1")
	fc := newUIDIndexedClient(t)
	c := NewClientFrom(fc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	meta := metav1.ObjectMeta{Name: "x", Namespace: accountNamespace("acct-2"), ResourceVersion: "1"}
	if err := c.UpdateDNSReservation(ctx, &seaplanev1alpha1.DNSReservation{ObjectMeta: meta}); err == nil {
		t.Error("DNS reservation update accepted a foreign snapshot")
	}
	if err := c.UpdateOidcConfigObject(ctx, &seaplanev1alpha1.OidcConfig{ObjectMeta: meta}); err == nil {
		t.Error("OIDC config update accepted a foreign snapshot")
	}
	owned := metav1.ObjectMeta{Name: "x", Namespace: accountNamespace("acct-1")}
	if err := c.UpdateDNSReservation(ctx, &seaplanev1alpha1.DNSReservation{ObjectMeta: owned}); err == nil {
		t.Error("DNS reservation update accepted a snapshot without resource version")
	}
}
