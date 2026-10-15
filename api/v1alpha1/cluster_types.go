/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterPhase represents the lifecycle phase of a Cluster.
// +kubebuilder:validation:Enum=WaitingForPlacement;Provisioning;Ready;Deleting
type ClusterPhase string

const (
	ClusterPhaseWaitingForPlacement ClusterPhase = "WaitingForPlacement"
	ClusterPhaseProvisioning        ClusterPhase = "Provisioning"
	ClusterPhaseReady               ClusterPhase = "Ready"
	ClusterPhaseDeleting            ClusterPhase = "Deleting"
)

// HostedClusterNamePending marks a Cluster whose internal HostedCluster name
// has not yet been initialized by the platform API or operator.
const HostedClusterNamePending = "uid-pending"

// ClusterSpec defines the desired state of a ROSA HCP cluster.
// metadata.Name is the client-chosen account-scoped name; metadata.Namespace
// is account-<accountID>.
type ClusterSpec struct {
	// DeleteProtection prevents accidental deletion when enabled.
	// +seaplane:write-mode=mutable
	// +optional
	DeleteProtection *bool `json:"deleteProtection,omitempty"`

	// ExpirationTimestamp marks when this cluster should be automatically deleted.
	// +seaplane:write-mode=mutable
	// +optional
	ExpirationTimestamp *metav1.Time `json:"expirationTimestamp,omitempty"`

	// Properties are arbitrary key-value pairs for customer metadata.
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxProperties=100
	// +optional
	Properties map[string]string `json:"properties,omitempty"`

	// AdditionalTrustBundle is a PEM-encoded CA bundle used by the cluster.
	// The Platform API accepts this value on writes and redacts it in its responses.
	// +seaplane:write-mode=mutable
	// +seaplane:response-redact
	// +kubebuilder:validation:MaxLength=1048576
	// +optional
	AdditionalTrustBundle *string `json:"additionalTrustBundle,omitempty"`

	// RegistryAdditionalTrustedCA is a map of registry hostname to PEM-encoded CA
	// certificates that should be trusted when pulling images from those registries.
	// The operator materializes this into a ConfigMap and references it in the
	// HostedCluster's ImageSpec.
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxProperties=256
	// +optional
	RegistryAdditionalTrustedCA map[string]string `json:"registryAdditionalTrustedCA,omitempty"`

	// Tags are customer-defined AWS tags applied to AWS resources for this
	// cluster and its NodePools. The operator propagates changes to the
	// HostedCluster and NodePool resources after creation.
	// HyperShift caps platform.aws.resourceTags at 25 entries. Reserved system
	// tags consume part of that budget; NodePool-specific tags share the same
	// rendered limit.
	// +seaplane:write-mode=mutable
	// +seaplane:resettable=true
	// +kubebuilder:validation:MaxProperties=23
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// AccountID is the AWS account that owns this cluster.
	// +k8s:openapi-gen=false
	// +seaplane:write-mode=service-set
	// +optional
	AccountID string `json:"accountId,omitempty"`

	// CreatorARN is the IAM ARN of the user who created this cluster.
	// +k8s:openapi-gen=false
	// +seaplane:write-mode=service-set
	// +optional
	// +kubebuilder:validation:Pattern=`^arn:aws:`
	CreatorARN string `json:"creatorARN,omitempty"`

	// HostedClusterName is the internal HostedCluster name used on the
	// management cluster. It is set by the platform/operator, not the customer.
	// When a DNSReservation is supplied, it copies that record's
	// status.internalHostedClusterName so networking and Cluster creation use
	// the same value.
	// Without a supplied reservation, it matches metadata.name.
	// It is optional for legacy Clusters that fall back to metadata.name; remove
	// +optional after those records are backfilled with an internal name.
	// +k8s:openapi-gen=false
	// +seaplane:write-mode=service-set
	// +optional
	HostedClusterName string `json:"hostedClusterName,omitempty"`

	// DNSReservationID is the UID of an account-scoped DNSReservation to bind to
	// this Cluster. If omitted, the operator allocates and binds one automatically.
	// Immutable after creation.
	// +seaplane:write-mode=immutable
	// +optional
	DNSReservationID string `json:"dnsReservationId,omitempty"`

	// OidcConfigID is the UID of the account-scoped OidcConfig to use for cluster
	// identity. Empty selects the legacy managed issuer flow. Immutable after creation.
	// +seaplane:write-mode=immutable
	// +optional
	OidcConfigID string `json:"oidcConfigId,omitempty"`

	// HostedCluster contains the upstream HyperShift fields, mirrored as
	// passthrough types with per-field visibility and write-mode markers.
	// +kubebuilder:validation:Required
	HostedCluster HostedClusterSpecPassthrough `json:"hostedCluster"`

	// ControlPlaneUpgradePolicy is the control plane upgrade policy defined by the user.
	// +optional
	ControlPlaneUpgradePolicy *ControlPlaneUpgradePolicySpec `json:"controlPlaneUpgradePolicy,omitempty"`
}

// ClusterStatus defines the observed state of a Cluster.
type ClusterStatus struct {
	// Conditions represent the latest observations of the cluster's state.
	// Known condition types: Synced, Available, Degraded, ControlPlaneUpgradeState.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Phase summarizes the cluster's lifecycle state.
	// +optional
	Phase ClusterPhase `json:"phase,omitempty"`

	// ControlPlaneEndpoint is the API server endpoint for the hosted cluster.
	// +optional
	ControlPlaneEndpoint hypershiftv1beta1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// Version is the running OpenShift version.
	// +optional
	Version string `json:"version,omitempty"`

	// PlacementRef references the Placement that assigned this cluster to a management cluster.
	// +optional
	PlacementRef *PlacementReference `json:"placementRef,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ControlPlaneUpgradePolicy summarizes the control plane upgrade policy status.
	// +optional
	ControlPlaneUpgradePolicy *ControlPlaneUpgradePolicyStatus `json:"controlPlaneUpgradePolicy,omitempty"`

	// BaseDomain is the DNS base domain assigned to this cluster via a DNSReservation
	// (e.g. "f7a3.0.openshiftapps.com", assembled as {prefix}.{shard}.{baseDomain},
	// where baseDomain is the operator's configured --base-domain).
	// Empty when not yet reserved. Set by the operator when the DNSReservation is created or claimed.
	// +optional
	BaseDomain string `json:"baseDomain,omitempty"`
}

// PlacementReference identifies the management cluster assignment.
type PlacementReference struct {
	// Name is the Placement CR name.
	Name string `json:"name"`

	// ManagementCluster is the target management cluster ID (e.g. mc01).
	ManagementCluster string `json:"managementCluster"`
}

// +genclient
// +genclient:nonNamespaced
// +bridge:watch=disabled
// +bridge:wait
// +seaplane:rest-response-projection=proxy
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=hfc
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS label of at most 63 characters"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="MC",type=string,JSONPath=".status.placementRef.managementCluster"
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=".status.controlPlaneEndpoint.host",priority=1
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=".spec.expirationTimestamp",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Cluster is the Schema for the clusters API.
// It represents a ROSA HCP cluster whose lifecycle is managed by the seaplane-operator.
// metadata.Name is a client-chosen DNS label of at most 63 characters;
// metadata.Namespace is account-<accountID>. The owning account is recorded in
// the seaplane.io/account-id label.
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ClusterSpec `json:"spec"`

	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster.
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Cluster{}, &ClusterList{})
		return nil
	})
}

// PlatformSpec mirrors PlatformSpec from upstream HyperShift, exposing only the AWS
// platform. Azure, GCP, OpenStack, and other platforms are intentionally omitted —
// Seaplane only supports AWS-backed clusters.
// +seaplane:upstream-reduced-object=hypershiftv1beta1.PlatformSpec
// +k8s:openapi-gen=true
type PlatformSpec struct {
	// type specifies the underlying infrastructure provider for the cluster.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=immutable
	// +kubebuilder:validation:Enum=AWS
	// +required
	Type hypershiftv1beta1.PlatformType `json:"type"`

	// aws specifies AWS-specific configuration for the cluster.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +optional
	AWS *AWSPlatformSpec `json:"aws,omitempty"`
}

// AWSPlatformSpec mirrors upstream AWSPlatformSpec so Seaplane can control
// visibility and write modes for individual AWS fields.
// +seaplane:upstream-reduced-object=hypershiftv1beta1.AWSPlatformSpec
// +k8s:openapi-gen=true
type AWSPlatformSpec struct {
	// region is the AWS region in which the cluster resides.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxLength=255
	// +required
	Region string `json:"region"`

	// cloudProviderConfig specifies AWS networking configuration for the control plane.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +optional
	CloudProviderConfig *hypershiftv1beta1.AWSCloudProviderConfig `json:"cloudProviderConfig,omitempty"`

	// serviceEndpoints specifies optional custom endpoints for AWS services.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxItems=50
	// +optional
	ServiceEndpoints []hypershiftv1beta1.AWSServiceEndpoint `json:"serviceEndpoints,omitempty"`

	// rolesRef contains references to the AWS IAM roles required by the cluster.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +required
	RolesRef hypershiftv1beta1.AWSRolesRef `json:"rolesRef"`

	// resourceTags is an internal HyperShift passthrough. Customer tags must use
	// ClusterSpec.Tags instead.
	// +k8s:openapi-gen=false
	// +seaplane:write-mode=service-set
	// +kubebuilder:validation:MaxItems=25
	// +optional
	ResourceTags []hypershiftv1beta1.AWSResourceTag `json:"resourceTags,omitempty"`

	// endpointAccess specifies the publishing scope of cluster endpoints.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:Enum=Public;PublicAndPrivate;Private
	// +kubebuilder:default=Public
	// +optional
	EndpointAccess hypershiftv1beta1.AWSEndpointAccessType `json:"endpointAccess,omitempty"`

	// additionalAllowedPrincipals specifies extra principals allowed to connect to the VPC endpoint service.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxItems=25
	// +kubebuilder:validation:items:MaxLength=255
	// +optional
	AdditionalAllowedPrincipals []string `json:"additionalAllowedPrincipals,omitempty"`

	// multiArch indicates whether this cluster supports NodePools with multiple CPU architectures.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:default=false
	// +optional
	MultiArch bool `json:"multiArch"`

	// sharedVPC contains configuration for a VPC shared from another AWS account.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +optional
	SharedVPC *hypershiftv1beta1.AWSSharedVPC `json:"sharedVPC,omitempty"`

	// terminationHandlerQueueURL specifies the SQS queue for EC2 Spot interruption events.
	// +k8s:openapi-gen=true
	// +seaplane:write-mode=mutable
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://sqs(-fips)?\.([a-z0-9-]+)\.amazonaws\.com/[0-9]{12}/[a-zA-Z0-9_-]+(\.fifo)?$`
	// +optional
	TerminationHandlerQueueURL string `json:"terminationHandlerQueueURL,omitempty"`
}
