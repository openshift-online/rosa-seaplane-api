package seaplanedb

import (
	"encoding/json"
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion"
	v1alpha1conv "github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion/v1alpha1"
)

// MergeSpecJSON merges raw JSON into dst. Only fields present in the JSON
// overwrite dst; omitted fields are preserved. This avoids data loss from
// non-omitempty struct fields (e.g. HostedCluster, NodePool passthrough)
// that would serialize as empty objects if marshaled from a typed Go struct.
func MergeSpecJSON(dst any, specJSON []byte) error {
	if len(specJSON) == 0 {
		return nil
	}
	if err := resetSuppliedMaps(dst, specJSON); err != nil {
		return err
	}
	return json.Unmarshal(specJSON, dst)
}

// resetSuppliedMaps ensures that explicitly supplied maps replace their stored
// values. encoding/json otherwise merges object keys when decoding into a
// non-nil map, preventing callers from removing keys with a partial update.
func resetSuppliedMaps(dst any, specJSON []byte) error {
	switch spec := dst.(type) {
	case *seaplanev1alpha1.ClusterSpec:
		if spec == nil {
			return nil
		}
		var supplied struct {
			Tags json.RawMessage `json:"tags"`
		}
		if err := json.Unmarshal(specJSON, &supplied); err != nil {
			return err
		}
		if supplied.Tags != nil {
			spec.Tags = nil
		}
	case *seaplanev1alpha1.NodePoolSpec:
		if spec == nil {
			return nil
		}
		var supplied struct {
			Labels   json.RawMessage `json:"labels"`
			Tags     json.RawMessage `json:"tags"`
			NodePool json.RawMessage `json:"nodePool"`
		}
		if err := json.Unmarshal(specJSON, &supplied); err != nil {
			return err
		}
		if supplied.Labels != nil {
			spec.Labels = nil
		}
		if supplied.Tags != nil {
			spec.Tags = nil
		}
		if supplied.NodePool != nil {
			var nodePool struct {
				NodeLabels json.RawMessage `json:"nodeLabels"`
			}
			if err := json.Unmarshal(supplied.NodePool, &nodePool); err != nil {
				return err
			}
			if nodePool.NodeLabels != nil {
				spec.NodePool.NodeLabels = nil
			}
		}
	}
	return nil
}

// --- Cluster conversions ---

// PublicToInternalCluster converts public.Cluster to internal v1alpha1.Cluster
// for storage in FleetDB. Account identity is derived from the authenticated caller.
// The input pub is not modified; a copy of ObjectMeta is used for the returned CRD.
func PublicToInternalCluster(pub *public.Cluster, accountID string) *seaplanev1alpha1.Cluster {
	if pub == nil {
		return nil
	}

	meta := customerObjectMeta(pub.ObjectMeta, accountID)

	enrichment := &conversion.ServiceSetFields{
		AccountID: accountID,
	}
	crdSpec := v1alpha1conv.UnprojectCluster(&pub.Spec, enrichment)

	return &seaplanev1alpha1.Cluster{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: meta,
		Spec:       *crdSpec,
	}
}

// InternalToPublicCluster converts internal v1alpha1.Cluster to public.Cluster
// for REST API responses. FleetDB's UID is returned without substitution.
func InternalToPublicCluster(cr *seaplanev1alpha1.Cluster) *public.Cluster {
	if cr == nil {
		return nil
	}
	return v1alpha1conv.ProjectCluster(cr)
}

// --- NodePool conversions ---

// PublicToInternalNodePool converts public.NodePool to internal v1alpha1.NodePool
// for storage in FleetDB. Enriches with service-set fields and syncs the top-level
// autoRepair and labels fields into the HyperShift passthrough for internal consistency.
// The input pub is not modified; a copy of ObjectMeta is used for the returned CRD.
func PublicToInternalNodePool(pub *public.NodePool, accountID, internalPoolID string) *seaplanev1alpha1.NodePool {
	if pub == nil {
		return nil
	}

	meta := customerObjectMeta(pub.ObjectMeta, accountID)

	// Capture top-level user values before Unproject, which overlays ServiceSetFields
	// (some of which have no omitempty and can zero out fields like Labels).
	// Clone labels so crdSpec.Labels and NodePool.NodeLabels do not share a mutable
	// map with each other or with the original pub.Spec.Labels.
	userAutoRepair := pub.Spec.AutoRepair
	userLabels := maps.Clone(pub.Spec.Labels)

	enrichment := &conversion.ServiceSetFields{
		AccountID:      accountID,
		InternalPoolID: internalPoolID,
	}
	crdSpec := v1alpha1conv.UnprojectNodePool(&pub.Spec, enrichment)

	// ServiceSetFields.Labels has no omitempty so the overlay can zero spec.Labels.
	// Restore the user-supplied labels so they're preserved in the stored CRD and
	// remain readable by ProjectNodePool on the read path.
	crdSpec.Labels = maps.Clone(userLabels)

	// Sync top-level fields into the HyperShift passthrough using the pre-Unproject
	// values; the operator owns management.autoRepair and nodeLabels and reconciles them,
	// but we mirror them here so the stored CRD is internally consistent from day one.
	// Clone again so crdSpec.Labels and crdSpec.NodePool.NodeLabels are independent maps.
	syncNodePoolPassthrough(crdSpec, userAutoRepair, maps.Clone(userLabels))

	return &seaplanev1alpha1.NodePool{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: meta,
		Spec:       *crdSpec,
	}
}

// InternalToPublicNodePool converts internal v1alpha1.NodePool to public.NodePool
// for REST API responses. FleetDB's UID is returned without substitution.
func InternalToPublicNodePool(cr *seaplanev1alpha1.NodePool) *public.NodePool {
	if cr == nil {
		return nil
	}
	return v1alpha1conv.ProjectNodePool(cr)
}

// --- OidcConfig conversions ---

// PublicToInternalOidcConfig converts public.OidcConfig to internal v1alpha1.OidcConfig
// for storage in FleetDB. The name and namespace are set from validated request
// metadata and the authenticated account.
// The input pub is not modified; a copy of ObjectMeta is used for the returned CRD.
func PublicToInternalOidcConfig(pub *public.OidcConfig, accountID string) *seaplanev1alpha1.OidcConfig {
	if pub == nil {
		return nil
	}

	meta := customerObjectMeta(pub.ObjectMeta, accountID)

	enrichment := &conversion.ServiceSetFields{
		AccountID: accountID,
	}
	crdSpec := v1alpha1conv.UnprojectOidcConfig(&pub.Spec, enrichment)

	return &seaplanev1alpha1.OidcConfig{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: meta,
		Spec:       *crdSpec,
	}
}

// InternalToPublicOidcConfig converts internal v1alpha1.OidcConfig to public.OidcConfig
// for REST API responses. FleetDB's UID is returned without substitution.
func InternalToPublicOidcConfig(cr *seaplanev1alpha1.OidcConfig) *public.OidcConfig {
	if cr == nil {
		return nil
	}
	return v1alpha1conv.ProjectOidcConfig(cr)
}

// PublicToInternalDNSReservation converts the customer-facing reservation to its
// account-scoped FleetDB representation.
func PublicToInternalDNSReservation(pub *public.DNSReservation, accountID string) *seaplanev1alpha1.DNSReservation {
	if pub == nil {
		return nil
	}
	return &seaplanev1alpha1.DNSReservation{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: customerObjectMeta(pub.ObjectMeta, accountID),
		Spec:       *v1alpha1conv.UnprojectDNSReservation(&pub.Spec, &conversion.ServiceSetFields{AccountID: accountID}),
	}
}

// InternalToPublicDNSReservation projects a DNSReservation. The FleetDB UID is returned unchanged.
func InternalToPublicDNSReservation(res *seaplanev1alpha1.DNSReservation) *public.DNSReservation {
	if res == nil {
		return nil
	}
	return v1alpha1conv.ProjectDNSReservation(res)
}

// --- Helpers ---

// customerObjectMeta sets the account namespace and trusted account label while
// discarding client-supplied server-owned identity and ownership metadata.
// Customer tags are annotations (the Cedar tags); labels are system-owned, so
// client labels are dropped.
func customerObjectMeta(meta metav1.ObjectMeta, accountID string) metav1.ObjectMeta {
	meta.Annotations = maps.Clone(meta.Annotations)
	for key := range meta.Annotations {
		if strings.HasPrefix(key, "seaplane.io/") {
			delete(meta.Annotations, key)
		}
	}
	meta.Labels = map[string]string{seaplanev1alpha1.AccountIDLabel: accountID}
	meta.Namespace = accountNamespace(accountID)
	meta.UID = ""
	meta.ResourceVersion = ""
	meta.Generation = 0
	meta.CreationTimestamp = metav1.Time{}
	meta.DeletionTimestamp = nil
	meta.DeletionGracePeriodSeconds = nil
	meta.OwnerReferences = nil
	meta.Finalizers = nil
	meta.ManagedFields = nil
	return meta
}

// syncNodePoolPassthrough mirrors the top-level autoRepair and labels into the
// HyperShift passthrough for internal consistency. Called only during public→internal
// conversion; the operator reconciles these during its own sync loop.
// autoRepair and labels are passed explicitly because the ServiceSetFields overlay in
// UnprojectNodePool can zero out spec.Labels (no omitempty on ServiceSetFields.Labels).
func syncNodePoolPassthrough(spec *seaplanev1alpha1.NodePoolSpec, autoRepair *bool, labels map[string]string) {
	if spec == nil {
		return
	}

	// Default autoRepair to true when unset (matches operator behavior).
	if autoRepair != nil {
		spec.NodePool.Management.AutoRepair = *autoRepair
	} else {
		spec.NodePool.Management.AutoRepair = true
	}

	spec.NodePool.NodeLabels = labels
}
