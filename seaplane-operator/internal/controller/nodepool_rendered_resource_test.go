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

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

func TestRenderedNodePoolLifecycle(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := hypershiftv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HyperShift scheme: %v", err)
	}

	store := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &NodePoolReconciler{Client: store}
	nodePool := &hypershiftv1beta1.NodePool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "hypershift.openshift.io/v1beta1",
			Kind:       "NodePool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workers",
			Namespace: "cluster-cluster-uid",
			Labels:    map[string]string{"seaplane.io/cluster-id": "cluster-uid"},
		},
		Spec: hypershiftv1beta1.NodePoolSpec{
			ClusterName: "marcorosa",
		},
	}
	if err := reconciler.upsertRenderedNodePool(ctx, nodePool); err != nil {
		t.Fatalf("create rendered NodePool: %v", err)
	}
	if nodePool.UID != "" || nodePool.ResourceVersion != "" {
		t.Fatalf("create mutated rendered NodePool identity: UID=%q, resourceVersion=%q", nodePool.UID, nodePool.ResourceVersion)
	}

	var got hypershiftv1beta1.NodePool
	key := client.ObjectKeyFromObject(nodePool)
	if err := store.Get(ctx, key, &got); err != nil {
		t.Fatalf("get rendered NodePool: %v", err)
	}
	if got.Spec.ClusterName != "marcorosa" {
		t.Fatalf("ClusterName = %q, want marcorosa", got.Spec.ClusterName)
	}

	databaseSnapshot := got.DeepCopy()
	databaseSnapshot.UID = types.UID("database-uid")
	databaseSnapshot.ResourceVersion = "10"
	updateStore := fake.NewClientBuilder().WithScheme(scheme).WithObjects(databaseSnapshot).Build()
	updateReconciler := &NodePoolReconciler{Client: updateStore}
	updatedNodePool := nodePool.DeepCopy()
	updatedNodePool.Spec.ClusterName = "updated-marcorosa"
	updatedNodePool.UID = ""
	updatedNodePool.ResourceVersion = ""
	if err := updateReconciler.upsertRenderedNodePool(ctx, updatedNodePool); err != nil {
		t.Fatalf("update rendered NodePool: %v", err)
	}
	if updatedNodePool.UID != "" || updatedNodePool.ResourceVersion != "" {
		t.Fatalf("update mutated rendered NodePool identity: UID=%q, resourceVersion=%q", updatedNodePool.UID, updatedNodePool.ResourceVersion)
	}
	if err := updateStore.Get(ctx, key, &got); err != nil {
		t.Fatalf("get updated NodePool: %v", err)
	}
	if got.Spec.ClusterName != "updated-marcorosa" {
		t.Fatalf("updated ClusterName = %q, want updated-marcorosa", got.Spec.ClusterName)
	}
	if got.UID != types.UID("database-uid") || got.ResourceVersion == "" {
		t.Fatalf("database identity = UID=%q, resourceVersion=%q, want database UID and resource version", got.UID, got.ResourceVersion)
	}

	if err := updateReconciler.deleteRenderedNodePool(ctx, "cluster-uid", "workers"); err != nil {
		t.Fatalf("delete rendered NodePool: %v", err)
	}
	if err := updateStore.Get(ctx, key, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("get after delete error = %v, want NotFound", err)
	}
}

func TestRenderedNodePoolIdentity(t *testing.T) {
	nodePool := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "marcorosa.workers",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: seaplanev1alpha1.GroupVersion.String(),
				Kind:       "Cluster",
				Name:       "marcorosa",
				UID:        types.UID("cluster-uid"),
				Controller: func() *bool { value := true; return &value }(),
			}},
		},
	}

	clusterUID, renderedName, ok := renderedNodePoolIdentity(nodePool)
	if !ok || clusterUID != "cluster-uid" || renderedName != "workers" {
		t.Fatalf("identity = (%q, %q, %t), want (cluster-uid, workers, true)", clusterUID, renderedName, ok)
	}
}
