package controller

import (
	"context"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

// upsertRenderedNodePool persists the rendered NodePool as a desired-state
// snapshot. Database-managed identity is kept out of the object used to build
// the DynamoDB desire for the management cluster.
func (r *NodePoolReconciler) upsertRenderedNodePool(ctx context.Context, nodePool *hypershiftv1beta1.NodePool) error {
	return upsertRenderedResource(ctx, r.Client, nodePool, func() client.Object {
		return &hypershiftv1beta1.NodePool{}
	})
}

// deleteRenderedNodePool removes the persisted desired-state snapshot.
func (r *NodePoolReconciler) deleteRenderedNodePool(ctx context.Context, clusterUID, nodePoolName string) error {
	nodePool := &hypershiftv1beta1.NodePool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "hypershift.openshift.io/v1beta1",
			Kind:       "NodePool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodePoolName,
			Namespace: render.ManagementNamespace(clusterUID),
		},
	}
	return deleteRenderedResource(ctx, r.Client, nodePool)
}

// renderedNodePoolIdentity returns the management-cluster identity from the
// Seaplane NodePool when its parent Cluster is no longer available.
func renderedNodePoolIdentity(nodePool *seaplanev1alpha1.NodePool) (string, string, bool) {
	owner := metav1.GetControllerOf(nodePool)
	if !isClusterControllerOwner(owner) {
		return "", "", false
	}
	_, name, ok := strings.Cut(nodePool.Name, ".")
	if !ok || name == "" || strings.Contains(name, ".") {
		return "", "", false
	}
	return string(owner.UID), name, true
}
