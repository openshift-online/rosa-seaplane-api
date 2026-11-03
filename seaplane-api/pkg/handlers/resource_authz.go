package handlers

import (
	"context"
	"maps"
	"net/http"
	"reflect"
	"strings"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Creation is authorized before seaplane-db assigns a UID.
func candidateResource(resource authz.Resource, name string) authz.Resource {
	resource.ID = name
	return resource
}

func (h *ClusterHandler) clusterResource(cr *v1.Cluster) authz.Resource {
	// The seaplane-db UID identifies this object incarnation independently of its name.
	return authz.Resource{
		Kind:      authz.Cluster,
		ID:        string(cr.UID),
		AccountID: cr.Labels[v1.AccountIDLabel],
		Labels:    maps.Clone(cr.Annotations),
	}
}

func (h *NodePoolHandler) nodePoolResource(ctx context.Context, np *v1.NodePool) (authz.Resource, error) {
	owner := metav1.GetControllerOf(np)
	// A pool without its current owned parent reads as not found; this is the only parent load.
	notFound := &authz.Failure{Stage: authz.StageResourceLoading, Err: apierrors.NewNotFound(v1.GroupVersion.WithResource("nodepools").GroupResource(), np.Name)}
	if owner == nil || owner.Kind != "Cluster" || owner.UID == "" || np.Labels[clusterUIDLabel] != string(owner.UID) {
		return authz.Resource{}, notFound
	}
	parent, err := h.db.GetCluster(ctx, owner.Name)
	if seaplanedb.IsNotFound(err) || (err == nil && parent.UID != owner.UID) {
		return authz.Resource{}, notFound
	}
	if err != nil {
		return authz.Resource{}, &authz.Failure{Stage: authz.StageResourceLoading, Err: err}
	}
	return authz.Resource{Kind: authz.NodePool, ID: string(np.UID), AccountID: np.Labels[v1.AccountIDLabel], Labels: maps.Clone(np.Annotations),
		ParentCluster: &authz.ParentCluster{ID: string(parent.UID), AccountID: parent.Labels[v1.AccountIDLabel], Labels: maps.Clone(parent.Annotations)}}, nil
}

// nodePoolNotFound is each loaded-pool operation's response when the parent check hides the pool.
// A function, not a map: the error values are assigned in init.
func nodePoolNotFound(operation authz.Action) (APIError, bool) {
	switch operation {
	case authz.DescribeNodePool:
		return ErrNodePoolGetNotFound, true
	case authz.UpdateNodePool:
		return ErrNodePoolUpdateNotFound, true
	case authz.DeleteNodePool:
		return ErrNodePoolDeleteNotFound, true
	}
	return APIError{}, false
}

func (h *NodePoolHandler) authorizeNodePool(w http.ResponseWriter, r *http.Request, operation authz.Action, np *v1.NodePool, additional ...authz.Action) bool {
	resource, err := h.nodePoolResource(r.Context(), np)
	if notFound, isLoaded := nodePoolNotFound(operation); isLoaded && seaplanedb.IsNotFound(err) {
		writeAPIError(w, notFound, h.logger)
		return false
	}
	if err != nil {
		attempt := startResourceAuthz(r, operation)
		writeResourceAuthzError(w, r, attempt, operation, err, h.logger)
		return false
	}
	if operation == authz.CreateNodePool {
		resource = candidateResource(resource, np.Name)
	}
	return authorizeResource(w, r, h.authorizer, h.logger, operation, resource, additional...)
}

// namespaceAccount returns the account that owns an account-<id> namespace, or "".
func namespaceAccount(namespace string) string {
	account, isAccountNamespace := strings.CutPrefix(namespace, "account-")
	if !isAccountNamespace {
		return ""
	}
	return account
}

func oidcConfigResource(oc *v1.OidcConfig) authz.Resource {
	return authz.Resource{Kind: authz.OIDCConfig, ID: string(oc.UID), AccountID: namespaceAccount(oc.Namespace), Labels: maps.Clone(oc.Annotations)}
}

func dnsReservationResource(res *v1.DNSReservation) authz.Resource {
	return authz.Resource{Kind: authz.DNSReservation, ID: string(res.UID), AccountID: namespaceAccount(res.Namespace), Labels: maps.Clone(res.Annotations)}
}

// clusterUpdateActions returns the specialized actions an update needs beyond
// UpdateCluster. It compares the stored spec with the exact candidate to be written.
func clusterUpdateActions(old, candidate *v1.ClusterSpec) []authz.Action {
	var actions []authz.Action
	if !reflect.DeepEqual(old.HostedCluster.Configuration, candidate.HostedCluster.Configuration) {
		actions = append(actions, authz.UpdateClusterConfig)
	}
	if !reflect.DeepEqual(old.HostedCluster.Release, candidate.HostedCluster.Release) ||
		!reflect.DeepEqual(old.ControlPlaneUpgradePolicy, candidate.ControlPlaneUpgradePolicy) {
		actions = append(actions, authz.UpdateClusterVersion)
	}
	return actions
}

// nodePoolUpdateActions returns the specialized actions an update needs beyond
// UpdateNodePool. It compares the stored spec with the exact candidate to be written.
func nodePoolUpdateActions(old, candidate *v1.NodePoolSpec) []authz.Action {
	var actions []authz.Action
	if !reflect.DeepEqual(old.NodePool.Replicas, candidate.NodePool.Replicas) ||
		!reflect.DeepEqual(old.NodePool.AutoScaling, candidate.NodePool.AutoScaling) {
		actions = append(actions, authz.ScaleNodePool)
	}
	if !reflect.DeepEqual(old.NodePool.Release, candidate.NodePool.Release) {
		actions = append(actions, authz.UpdateNodePoolVersion)
	}
	return actions
}
