package controller

import (
	"context"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// upsertRenderedHostedCluster persists the rendered HostedCluster as a
// desired-state snapshot: it records what the renderer produced and handed
// to the kube-applier, not confirmation that the management cluster applied
// it. Apply confirmation remains in DynamoDB via the DesireStatusEntry and
// the synced condition. Content-identical updates are suppressed by the
// database writer, so repeated reconciles are read-only no-ops.
func (r *ClusterReconciler) upsertRenderedHostedCluster(ctx context.Context, hostedCluster *hypershiftv1beta1.HostedCluster) error {
	return upsertRenderedResource(ctx, r.Client, hostedCluster, func() client.Object {
		return &hypershiftv1beta1.HostedCluster{}
	})
}

// deleteRenderedHostedCluster removes the persisted desired-state snapshot.
// When a management cluster exists, this is called only after MC deletion is
// confirmed; otherwise the snapshot is removed immediately. Normal cleanup is
// driven by the Cluster finalizer; the seaplane.io/cluster-id label supports
// any future orphan-discovery job.
func (r *ClusterReconciler) deleteRenderedHostedCluster(ctx context.Context, clusterUID, hostedClusterName string) error {
	hostedCluster := &hypershiftv1beta1.HostedCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "hypershift.openshift.io/v1beta1",
			Kind:       "HostedCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostedClusterName,
			Namespace: render.ManagementNamespace(clusterUID),
		},
	}
	return deleteRenderedResource(ctx, r.Client, hostedCluster)
}
