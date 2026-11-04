package controller

import (
	"context"
	"testing"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDNSReservationStatusPublishesInternalHostedClusterName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register API types: %v", err)
	}
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dns-name-status-test",
			Namespace: "account-123456789012",
			UID:       types.UID("550e8400-e29b-41d4-a716-446655440000"),
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&seaplanev1alpha1.DNSReservation{}).
		WithObjects(reservation).
		Build()
	reconciler := &DNSReservationReconciler{Client: k8sClient}

	if err := reconciler.updateReservationStatus(context.Background(), reservation, seaplanev1alpha1.DNSReservationPhaseReady, "f7a3.0.example.com", ""); err != nil {
		t.Fatalf("updateReservationStatus returned error: %v", err)
	}

	var updated seaplanev1alpha1.DNSReservation
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(reservation), &updated); err != nil {
		t.Fatalf("get updated DNSReservation: %v", err)
	}
	wantName := seaplanev1alpha1.HostedClusterNameFromDNSReservation(&updated)
	if updated.Status.InternalHostedClusterName != wantName {
		t.Errorf("InternalHostedClusterName = %q, want %q", updated.Status.InternalHostedClusterName, wantName)
	}
}

func TestAutomaticDNSReservationStatusUsesClaimedClustersPersistedName(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register API types: %v", err)
	}
	cluster := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "automatic-cluster",
			Namespace: "account-123456789012",
			UID:       types.UID("cluster-uid"),
		},
		Spec: seaplanev1alpha1.ClusterSpec{HostedClusterName: "automatic-cluster"},
	}
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "auto-dns-cluster-uid",
			Namespace: cluster.Namespace,
			UID:       types.UID("550e8400-e29b-41d4-a716-446655440000"),
			Labels:    map[string]string{claimedByClusterUIDLabel: string(cluster.UID)},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&seaplanev1alpha1.DNSReservation{}).
		WithIndex(&seaplanev1alpha1.Cluster{}, metadataUIDField, func(obj client.Object) []string { return []string{string(obj.GetUID())} }).
		WithObjects(cluster, reservation).
		Build()
	reconciler := &DNSReservationReconciler{Client: k8sClient}

	if err := reconciler.updateReservationStatus(context.Background(), reservation, seaplanev1alpha1.DNSReservationPhaseReady, "f7a3.0.example.com", ""); err != nil {
		t.Fatalf("updateReservationStatus returned error: %v", err)
	}

	var updated seaplanev1alpha1.DNSReservation
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(reservation), &updated); err != nil {
		t.Fatalf("get updated DNSReservation: %v", err)
	}
	if updated.Status.InternalHostedClusterName != cluster.Spec.HostedClusterName {
		t.Errorf("InternalHostedClusterName = %q, want claimed Cluster name %q", updated.Status.InternalHostedClusterName, cluster.Spec.HostedClusterName)
	}
}

func TestClaimedCustomerDNSReservationKeepsUIDDerivedName(t *testing.T) {
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "customer-dns-reservation",
			Namespace: "account-123456789012",
			UID:       types.UID("550e8400-e29b-41d4-a716-446655440000"),
			Labels:    map[string]string{claimedByClusterUIDLabel: "cluster-uid"},
		},
	}
	reconciler := &DNSReservationReconciler{}

	got, err := reconciler.resolveInternalHostedClusterName(context.Background(), reservation)
	if err != nil {
		t.Fatalf("resolveInternalHostedClusterName returned error: %v", err)
	}
	if want := seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation); got != want {
		t.Errorf("internal HostedCluster name = %q, want UID-derived reservation name %q", got, want)
	}
}
