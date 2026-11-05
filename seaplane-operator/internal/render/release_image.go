package render

import (
	"fmt"
	"strings"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

// ValidateReleaseImage requires a non-empty OCP release image pullspec.
func ValidateReleaseImage(cluster *seaplanev1alpha1.Cluster) error {
	if strings.TrimSpace(cluster.Spec.HostedCluster.Release.Image) == "" {
		return fmt.Errorf(
			"cluster %s/%s has no release image; spec.hostedCluster.release.image is required",
			cluster.Namespace, cluster.Name,
		)
	}
	return nil
}

// ValidateNodePoolReleaseImage requires an explicit release image, either on the
// NodePool itself or inherited from its parent Cluster.
func ValidateNodePoolReleaseImage(nodePool *seaplanev1alpha1.NodePool, cluster *seaplanev1alpha1.Cluster) error {
	image := nodePool.Spec.NodePool.Release.Image
	if image == "" {
		return ValidateReleaseImage(cluster)
	}
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf(
			"nodepool %s/%s release image must not be whitespace-only",
			nodePool.Namespace, nodePool.Name,
		)
	}
	return nil
}
