package controller

import (
	"context"
	"testing"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRenderedHostedClusterLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := hypershiftv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HyperShift scheme: %v", err)
	}

	store := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &ClusterReconciler{Client: store}
	hostedCluster := &hypershiftv1beta1.HostedCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "hypershift.openshift.io/v1beta1",
			Kind:       "HostedCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "marcorosa",
			Namespace: "cluster-cluster-uid",
			Labels:    map[string]string{"seaplane.io/cluster-id": "cluster-uid"},
		},
		Spec: hypershiftv1beta1.HostedClusterSpec{
			DNS: hypershiftv1beta1.DNSSpec{BaseDomain: "example.com"},
		},
	}
	if err := reconciler.upsertRenderedHostedCluster(ctx, hostedCluster); err != nil {
		t.Fatalf("create rendered HostedCluster: %v", err)
	}
	if hostedCluster.UID != "" || hostedCluster.ResourceVersion != "" {
		t.Fatalf("create mutated rendered HostedCluster identity: UID=%q, resourceVersion=%q", hostedCluster.UID, hostedCluster.ResourceVersion)
	}

	var got hypershiftv1beta1.HostedCluster
	key := client.ObjectKeyFromObject(hostedCluster)
	if err := store.Get(ctx, key, &got); err != nil {
		t.Fatalf("get rendered HostedCluster: %v", err)
	}
	if got.Spec.DNS.BaseDomain != "example.com" {
		t.Fatalf("BaseDomain = %q, want example.com", got.Spec.DNS.BaseDomain)
	}

	databaseSnapshot := got.DeepCopy()
	databaseSnapshot.UID = types.UID("database-uid")
	databaseSnapshot.ResourceVersion = "10"
	updateStore := fake.NewClientBuilder().WithScheme(scheme).WithObjects(databaseSnapshot).Build()
	updateReconciler := &ClusterReconciler{Client: updateStore}
	updatedHostedCluster := hostedCluster.DeepCopy()
	updatedHostedCluster.Spec.DNS.BaseDomain = "updated.example.com"
	updatedHostedCluster.UID = ""
	updatedHostedCluster.ResourceVersion = ""
	if err := updateReconciler.upsertRenderedHostedCluster(ctx, updatedHostedCluster); err != nil {
		t.Fatalf("update rendered HostedCluster: %v", err)
	}
	if updatedHostedCluster.UID != "" || updatedHostedCluster.ResourceVersion != "" {
		t.Fatalf("update mutated rendered HostedCluster identity: UID=%q, resourceVersion=%q", updatedHostedCluster.UID, updatedHostedCluster.ResourceVersion)
	}
	if err := updateStore.Get(ctx, key, &got); err != nil {
		t.Fatalf("get updated HostedCluster: %v", err)
	}
	if got.Spec.DNS.BaseDomain != "updated.example.com" {
		t.Fatalf("updated BaseDomain = %q, want updated.example.com", got.Spec.DNS.BaseDomain)
	}
	if got.UID != types.UID("database-uid") || got.ResourceVersion == "" {
		t.Fatalf("database identity = UID=%q, resourceVersion=%q, want database UID and resource version", got.UID, got.ResourceVersion)
	}

	if err := updateReconciler.deleteRenderedHostedCluster(ctx, "cluster-uid", "marcorosa"); err != nil {
		t.Fatalf("delete rendered HostedCluster: %v", err)
	}
	if err := updateStore.Get(ctx, key, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("get after delete error = %v, want NotFound", err)
	}
}
