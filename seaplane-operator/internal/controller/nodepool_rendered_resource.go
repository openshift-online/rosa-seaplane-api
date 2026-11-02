package controller

import (
	"context"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// upsertRenderedNodePool persists the rendered NodePool as a desired-state
// snapshot. Database-managed identity is kept out of the object used to build
// the DynamoDB desire for the management cluster.
func (r *NodePoolReconciler) upsertRenderedNodePool(ctx context.Context, source *seaplanev1alpha1.NodePool, nodePool *hypershiftv1beta1.NodePool) error {
	return upsertRenderedResource(ctx, r.Client, nodePool, func() client.Object {
		return &hypershiftv1beta1.NodePool{}
	}, source.Namespace, source.Name, map[string]string{
		"seaplane.io/rendered-namespace": nodePool.Namespace,
		"seaplane.io/rendered-name":      nodePool.Name,
	})
}

// deleteRenderedNodePool removes the persisted desired-state snapshot.
func (r *NodePoolReconciler) deleteRenderedNodePool(ctx context.Context, accountNamespace, nodePoolName string) error {
	nodePool := &hypershiftv1beta1.NodePool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "hypershift.openshift.io/v1beta1",
			Kind:       "NodePool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodePoolName,
			Namespace: accountNamespace,
		},
	}
	return deleteRenderedResource(ctx, r.Client, nodePool)
}
