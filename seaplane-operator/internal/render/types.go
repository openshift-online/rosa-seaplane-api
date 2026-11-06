package render

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

const (
	clusterIDLabel    = "seaplane.io/cluster-id"
	resourceTypeLabel = "seaplane.io/resource-type"
	managedByLabel    = "seaplane.io/managed-by"
	managedByOperator = "seaplane-operator"
)

// HostedClusterName resolves the internal management-cluster name persisted
// by the platform API or operator. Legacy and automatic-DNS Clusters fall back
// to their existing customer name.
func HostedClusterName(cluster *seaplanev1alpha1.Cluster) string {
	if cluster == nil {
		return ""
	}
	if name := cluster.Spec.HostedClusterName; name != "" && name != seaplanev1alpha1.HostedClusterNamePending {
		return name
	}
	return cluster.Name
}

// ECRMirrorConfig holds the zero-egress ECR mirror configuration.
// When Enabled, the operator populates HostedCluster.spec.imageContentSources
// to redirect OCP image pulls from quay.io to the configured ECR registry.
// All four fields must be set together or none at all.
type ECRMirrorConfig struct {
	// RegistryAccount is the AWS account ID of the ECR registry.
	RegistryAccount string
	// RegistryRegion is the AWS region of the ECR registry.
	RegistryRegion string
	// Repository is the ECR repository name (e.g. "my-org/ocp-mirror").
	Repository string
	// Sources lists the source registry paths to redirect to the ECR mirror.
	// Parsed from ECR_MIRROR_SOURCES ("source-a,source-b,...").
	Sources []string
	// BaseSecretName is the target Secret name for the ESO ExternalSecret that
	// syncs /infra/pull-secret from SSM. When set, ESO writes to this Secret
	// instead of "pull-secret", leaving "pull-secret" exclusively for
	// ecr-creds-sync to write the merged credentials. When empty, the ESO
	// ExternalSecret targets "pull-secret" directly (default, non-zero-egress).
	BaseSecretName string
}

// Enabled returns true when all four fields are set.
func (c ECRMirrorConfig) Enabled() bool {
	return c.RegistryAccount != "" && c.RegistryRegion != "" && c.Repository != "" && len(c.Sources) > 0
}

// RegionalConfig holds per-region values injected by the operator at startup.
type RegionalConfig struct {
	// BaseDomainSuffix is the shared parent zone every cluster's assigned base
	// domain hangs off, e.g. "rosa.example.com". A cluster's own base domain is
	// assembled as "{prefix}.{shard}.{BaseDomainSuffix}".
	BaseDomainSuffix string
	AWSRegion        string
	// ControlPlaneOperatorImage, when non-empty, is stamped onto every
	// HostedCluster as the hypershift.openshift.io/control-plane-operator-image
	// annotation. Development-only override for testing custom
	// control-plane-operator builds; empty disables the override.
	ControlPlaneOperatorImage string
	// ECRMirror, when Enabled, causes the operator to populate
	// HostedCluster.spec.imageContentSources with mirror entries pointing
	// OCP image sources at the configured zero-egress ECR registry.
	ECRMirror ECRMirrorConfig
}

// Resource is a generated Kubernetes resource with its GVR for desire creation.
type Resource struct {
	Group     string
	Version   string
	Resource  string
	Name      string
	Namespace string
	Object    any
}

// ManagementNamespace returns the namespace used for a Cluster's resources on
// its management cluster. FleetDB namespaces are account-scoped and are not
// used to derive this namespace.
func ManagementNamespace(clusterUID string) string {
	return "cluster-" + clusterUID
}

func clusterLabels(clusterUID string) map[string]string {
	return map[string]string{clusterIDLabel: clusterUID}
}

func clusterResourceLabels(clusterUID, resourceType string) map[string]string {
	labels := clusterLabels(clusterUID)
	labels[resourceTypeLabel] = resourceType
	return labels
}

func managementClusterIdentity(cluster *seaplanev1alpha1.Cluster) (uid, namespace string, err error) {
	if cluster == nil {
		return "", "", fmt.Errorf("cluster is nil")
	}
	if cluster.UID == "" {
		return "", "", fmt.Errorf("cluster %s/%s has no database UID", cluster.Namespace, cluster.Name)
	}
	uid = string(cluster.UID)
	return uid, ManagementNamespace(uid), nil
}

// Minimal local types for CRDs that lack standalone API modules.
// These produce the correct JSON for kube-applier-aws without pulling
// in the full cert-manager or external-secrets dependency trees.

// Certificate is a minimal cert-manager.io/v1.Certificate for serialization.
type Certificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CertificateSpec `json:"spec"`
}

type CertificateSpec struct {
	SecretName string               `json:"secretName"`
	IssuerRef  CertificateIssuerRef `json:"issuerRef"`
	DNSNames   []string             `json:"dnsNames,omitempty"`
}

type CertificateIssuerRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// ExternalSecret is a minimal external-secrets.io/v1.ExternalSecret for serialization.
type ExternalSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExternalSecretSpec `json:"spec"`
}

type ExternalSecretSpec struct {
	RefreshInterval string                    `json:"refreshInterval"`
	SecretStoreRef  SecretStoreRef            `json:"secretStoreRef"`
	Target          ExternalSecretTarget      `json:"target"`
	Data            []ExternalSecretDataEntry `json:"data"`
}

type SecretStoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type ExternalSecretTarget struct {
	Name           string                       `json:"name"`
	CreationPolicy string                       `json:"creationPolicy"`
	Template       ExternalSecretTargetTemplate `json:"template,omitempty"`
}

type ExternalSecretTargetTemplate struct {
	Type string `json:"type"`
}

type ExternalSecretDataEntry struct {
	SecretKey string            `json:"secretKey"`
	RemoteRef ExternalRemoteRef `json:"remoteRef"`
}

type ExternalRemoteRef struct {
	Key string `json:"key"`
}
