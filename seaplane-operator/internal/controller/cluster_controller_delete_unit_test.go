package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
)

// Clusters created before the release image was required must still be deletable.
func TestReconcileDeleteWithoutReleaseImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cluster := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "legacy-cluster",
			Namespace:  "account-123456789012",
			UID:        types.UID("legacy-cluster-uid"),
			Finalizers: []string{clusterFinalizer},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			AccountID:         "123456789012",
			HostedClusterName: "legacy-cluster",
			HostedCluster:     seaplanev1alpha1.HostedClusterSpecPassthrough{},
		},
		Status: seaplanev1alpha1.ClusterStatus{
			PlacementRef: &seaplanev1alpha1.PlacementReference{
				Name:              "legacy-cluster.placement",
				ManagementCluster: "mc01",
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).WithStatusSubresource(cluster).Build()
	fd := &fakeDynamo{}
	reconciler := &ClusterReconciler{
		Client:         fc,
		Scheme:         scheme,
		Dynamo:         fd,
		RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "example.com", AWSRegion: "us-east-1"},
	}

	result, err := reconciler.reconcileDelete(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcileDelete() error = %v, want nil", err)
	}
	if got := len(filterDeleteDesires(fd.applies)); got != 6 {
		t.Fatalf("delete desires = %d, want 6", got)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting for deletion confirmation")
	}
}
