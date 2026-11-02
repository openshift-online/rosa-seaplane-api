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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

const (
	clusterFinalizer         = "seaplane.io/cluster"
	statusRefreshDelay       = 5 * time.Minute
	claimConfirmationTimeout = 10 * time.Minute
	taskKey                  = "seaplane-operator"

	// accountIDLabel records the AWS account that owns a resource.
	accountIDLabel = "seaplane.io/account-id"
	// clusterUIDLabel records the database UID of the owning Cluster.
	clusterUIDLabel = "seaplane.io/cluster-uid"
	// claimedByClusterUIDLabel records the Cluster UID holding a claim.
	claimedByClusterUIDLabel = "seaplane.io/claimed-by-cluster-uid"
	// ownerUIDLabel records the UID that owns an internal Index.
	ownerUIDLabel    = "seaplane.io/owner-uid"
	metadataUIDField = "metadata.uid"
	// accountNSPrefix prefixes the per-account namespace name.
	accountNSPrefix = "account-"
)

// ClusterReconciler reconciles a Cluster object by creating DynamoDB desires
// that kube-applier-aws applies to the management cluster.
type ClusterReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Dynamo                  dynamo.DesireClient
	RegionalConfig          render.RegionalConfig
	StatusEvents            chan event.GenericEvent
	EventRouter             *EventRouter
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=seaplane.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=seaplane.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=seaplane.io,resources=nodepools,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=placements,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=oidcconfigs,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=seaplane.io,resources=dnsreservations,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=indices,verbs=get;list;watch;create;delete

// oidcSigningKeyExternal reports whether cluster's referenced OidcConfig is unmanaged
func (r *ClusterReconciler) oidcSigningKeyExternal(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (bool, error) {
	if cluster.Spec.OidcConfigID == "" {
		return false, nil
	}
	accountID := cluster.Labels[accountIDLabel]
	if accountID == "" {
		return false, nil
	}
	oc, err := r.getOidcConfigByUID(ctx, accountID, cluster.Spec.OidcConfigID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get oidcconfig %s: %w", cluster.Spec.OidcConfigID, err)
	}
	return oc.Spec.Type == seaplanev1alpha1.OidcConfigTypeUnmanaged, nil
}

// listByMetadataUID uses the pgruntime UID field index when available. Some
// Kubernetes-backed clients do not support metadata.uid field selectors, so
// fall back to a namespace list for those clients.
func (r *ClusterReconciler) listByMetadataUID(ctx context.Context, list client.ObjectList, namespace, uid string) error {
	err := r.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingFields{metadataUIDField: uid},
	)
	if err == nil || !apierrors.IsBadRequest(err) {
		return err
	}
	return r.List(ctx, list, client.InNamespace(namespace))
}

func (r *ClusterReconciler) getOidcConfigByUID(ctx context.Context, accountID, uid string) (*seaplanev1alpha1.OidcConfig, error) {
	var list seaplanev1alpha1.OidcConfigList
	if err := r.listByMetadataUID(ctx, &list, accountNamespace(accountID), uid); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if string(list.Items[i].UID) == uid {
			return &list.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(seaplanev1alpha1.GroupVersion.WithResource("oidcconfigs").GroupResource(), uid)
}

func (r *ClusterReconciler) getDNSReservationByUID(ctx context.Context, accountID, uid string) (*seaplanev1alpha1.DNSReservation, error) {
	var list seaplanev1alpha1.DNSReservationList
	if err := r.listByMetadataUID(ctx, &list, accountNamespace(accountID), uid); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if string(list.Items[i].UID) == uid {
			return &list.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(seaplanev1alpha1.GroupVersion.WithResource("dnsreservations").GroupResource(), uid)
}

// releaseOidcConfigClaim releases only a claim still held by this Cluster UID.
func (r *ClusterReconciler) releaseOidcConfigClaim(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	accountID := cluster.Labels[accountIDLabel]
	if accountID == "" {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		oc, err := r.getOidcConfigByUID(ctx, accountID, cluster.Spec.OidcConfigID)
		if err != nil {
			return client.IgnoreNotFound(err)
		}
		if oc.Labels[claimedByClusterUIDLabel] != string(cluster.UID) {
			return nil
		}
		delete(oc.Labels, claimedByClusterUIDLabel)
		return r.Update(ctx, oc)
	})
}

// confirmedDNSReservation returns the assigned domain only after every required
// claim is bound to this database-minted Cluster UID.
func (r *ClusterReconciler) confirmedDNSReservation(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (string, bool, error) {
	accountID := cluster.Spec.AccountID
	if accountID == "" {
		accountID = cluster.Labels[accountIDLabel]
	}
	if accountID == "" || cluster.Spec.DNSReservationID == "" || cluster.UID == "" {
		return "", false, nil
	}

	reservation, err := r.getDNSReservationByUID(ctx, accountID, cluster.Spec.DNSReservationID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get DNSReservation by UID: %w", err)
	}
	if reservation == nil {
		return "", false, nil
	}
	if !reservation.DeletionTimestamp.IsZero() ||
		reservation.Labels[claimedByClusterUIDLabel] != string(cluster.UID) ||
		reservation.Status.Phase != seaplanev1alpha1.DNSReservationPhaseReady ||
		reservation.Status.BaseDomain == "" ||
		reservation.Status.InternalHostedClusterName == "" {
		return "", false, nil
	}

	if cluster.Spec.OidcConfigID != "" {
		oidcConfig, err := r.getOidcConfigByUID(ctx, accountID, cluster.Spec.OidcConfigID)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("get referenced OidcConfig: %w", err)
		}
		if oidcConfig.Labels[claimedByClusterUIDLabel] != string(cluster.UID) {
			return "", false, nil
		}
	}

	return reservation.Status.BaseDomain, true, nil
}

// ensureAutomaticDNSReservation preserves the legacy Cluster-create flow when a
// client does not supply a pre-created DNSReservation. The UID-based name and
// Cluster UID claim make retries recover the same reservation.
func (r *ClusterReconciler) ensureAutomaticDNSReservation(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	accountID := cluster.Spec.AccountID
	if accountID == "" {
		accountID = cluster.Labels[accountIDLabel]
	}
	clusterUID := string(cluster.UID)
	if accountID == "" || clusterUID == "" {
		return fmt.Errorf("cluster %s/%s needs account ID and database UID for automatic DNS reservation", cluster.Namespace, cluster.Name)
	}

	key := types.NamespacedName{
		Namespace: accountNamespace(accountID),
		Name:      automaticDNSReservationName(clusterUID),
	}
	var reservation seaplanev1alpha1.DNSReservation
	if err := r.Get(ctx, key, &reservation); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get automatic DNSReservation %s/%s: %w", key.Namespace, key.Name, err)
		}
		legacyKey := types.NamespacedName{
			Namespace: key.Namespace,
			Name:      legacyAutomaticDNSReservationName(cluster.Name, clusterUID),
		}
		if err := r.Get(ctx, legacyKey, &reservation); err == nil {
			key = legacyKey
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get legacy automatic DNSReservation %s/%s: %w", legacyKey.Namespace, legacyKey.Name, err)
		} else {

			reservation = seaplanev1alpha1.DNSReservation{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: key.Namespace,
					Name:      key.Name,
					Labels: map[string]string{
						accountIDLabel:           accountID,
						claimedByClusterUIDLabel: clusterUID,
					},
				},
				Spec: seaplanev1alpha1.DNSReservationSpec{
					ClusterDeletionPolicy: seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete,
				},
			}
			if err := r.Create(ctx, &reservation); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("create automatic DNSReservation %s/%s: %w", key.Namespace, key.Name, err)
				}
				if err := r.Get(ctx, key, &reservation); err != nil {
					return fmt.Errorf("recover automatic DNSReservation %s/%s after create collision: %w", key.Namespace, key.Name, err)
				}
			}
		}
	}
	if reservation.Labels[accountIDLabel] != accountID || reservation.Labels[claimedByClusterUIDLabel] != clusterUID {
		return fmt.Errorf("automatic DNSReservation %s/%s is not claimed by Cluster UID %s", key.Namespace, key.Name, clusterUID)
	}
	if reservation.UID == "" {
		return fmt.Errorf("automatic DNSReservation %s/%s has no database UID", key.Namespace, key.Name)
	}

	cluster.Spec.DNSReservationID = string(reservation.UID)
	if err := r.Update(ctx, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("set automatic DNSReservation UID on Cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

func automaticDNSReservationName(clusterUID string) string {
	return fmt.Sprintf("auto-dns-%s", clusterUID)
}

func legacyAutomaticDNSReservationName(clusterName, clusterUID string) string {
	return fmt.Sprintf("%s-auto-dns-%s", clusterName, clusterUID)
}

func (r *ClusterReconciler) reconcileDNSReservation(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (string, bool, ctrl.Result, error) {
	if cluster.Spec.DNSReservationID == "" {
		if err := r.ensureAutomaticDNSReservation(ctx, cluster); err != nil {
			return "", false, ctrl.Result{}, fmt.Errorf("ensure automatic DNS reservation: %w", err)
		}
		return "", false, ctrl.Result{}, nil
	}

	baseDomain, claimsConfirmed, err := r.confirmedDNSReservation(ctx, cluster)
	if err != nil {
		return "", false, ctrl.Result{}, fmt.Errorf("verify Cluster claims: %w", err)
	}
	if !claimsConfirmed {
		if cluster.Status.Phase == "" && !cluster.CreationTimestamp.IsZero() &&
			time.Since(cluster.CreationTimestamp.Time) >= claimConfirmationTimeout {
			if err := r.deleteClusterWithUnconfirmedClaims(ctx, cluster); err != nil {
				return "", false, ctrl.Result{}, fmt.Errorf("delete Cluster with unconfirmed claims: %w", err)
			}
			return "", false, ctrl.Result{}, nil
		}
		logf.FromContext(ctx).Info("Waiting for DNS and OIDC claims to be confirmed", "cluster", cluster.Name)
		return "", false, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if cluster.Status.BaseDomain != baseDomain {
		if err := r.persistBaseDomain(ctx, cluster, baseDomain); err != nil {
			return "", false, ctrl.Result{}, fmt.Errorf("persist reservation base domain: %w", err)
		}
		return "", false, ctrl.Result{}, nil
	}
	return baseDomain, true, ctrl.Result{}, nil
}

func (r *ClusterReconciler) deleteClusterWithUnconfirmedClaims(ctx context.Context, cluster *seaplanev1alpha1.Cluster) error {
	key := client.ObjectKeyFromObject(cluster)
	var latest seaplanev1alpha1.Cluster
	found := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest = seaplanev1alpha1.Cluster{}
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}
		found = true
		if !latest.DeletionTimestamp.IsZero() {
			return nil
		}

		latest.Status.ObservedGeneration = latest.Generation
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Degraded",
			Status:             metav1.ConditionTrue,
			Reason:             "ClaimConfirmationTimedOut",
			Message:            fmt.Sprintf("DNS and OIDC claims were not confirmed within %s; deleting the Cluster", claimConfirmationTimeout),
			ObservedGeneration: latest.Generation,
			LastTransitionTime: metav1.Now(),
		})
		return r.Status().Update(ctx, &latest)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record claim confirmation timeout: %w", err)
	}
	if !found || !latest.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, &latest); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Cluster after claim confirmation timeout: %w", err)
	}
	return nil
}

func (r *ClusterReconciler) ensureHostedClusterName(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (bool, error) {
	if cluster.Spec.HostedClusterName != "" && cluster.Spec.HostedClusterName != seaplanev1alpha1.HostedClusterNamePending {
		return false, nil
	}
	hostedClusterName := cluster.Name
	if cluster.Spec.DNSReservationID != "" {
		accountID := cluster.Spec.AccountID
		if accountID == "" {
			accountID = cluster.Labels[accountIDLabel]
		}
		if accountID == "" {
			return false, fmt.Errorf("resolve DNSReservation for Cluster %s/%s without an account ID", cluster.Namespace, cluster.Name)
		}
		reservation, err := r.getDNSReservationByUID(ctx, accountID, cluster.Spec.DNSReservationID)
		if err != nil {
			return false, fmt.Errorf("get DNSReservation %s for Cluster %s/%s: %w", cluster.Spec.DNSReservationID, cluster.Namespace, cluster.Name, err)
		}
		hostedClusterName = reservation.Status.InternalHostedClusterName
	}
	if hostedClusterName == "" {
		return false, fmt.Errorf("resolve HostedCluster name for Cluster %s/%s", cluster.Namespace, cluster.Name)
	}
	cluster.Spec.HostedClusterName = hostedClusterName
	if err := r.Update(ctx, cluster); err != nil {
		return false, fmt.Errorf("persist HostedCluster name for Cluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return true, nil
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster seaplanev1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion via standard Kubernetes DeletionTimestamp.
	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cluster)
	}

	// Ensure finalizer.
	if !controllerutil.ContainsFinalizer(&cluster, clusterFinalizer) {
		controllerutil.AddFinalizer(&cluster, clusterFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// The finalizer update emits a watch event that re-enqueues this object.
		return ctrl.Result{}, nil
	}
	hostedClusterNameUpdated, err := r.ensureHostedClusterName(ctx, &cluster)
	if hostedClusterNameUpdated || err != nil {
		return ctrl.Result{}, err
	}

	baseDomain, dnsReady, dnsResult, err := r.reconcileDNSReservation(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !dnsReady {
		return dnsResult, nil
	}

	if expired, err := r.deleteIfExpired(ctx, &cluster); expired {
		return ctrl.Result{}, err
	}

	// Look up Placement — if none or not Bound, wait.
	placementName := fmt.Sprintf("%s.placement", cluster.Name)
	var placement seaplanev1alpha1.Placement
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: placementName}, &placement); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Waiting for Placement", "cluster", cluster.Name)
			r.setPhase(ctx, &cluster, seaplanev1alpha1.ClusterPhaseWaitingForPlacement)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get placement: %w", err)
	}
	if placement.Status.Phase != seaplanev1alpha1.PlacementPhaseBound {
		log.Info("Placement not yet Bound", "placement", placementName)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return r.reconcilePlacedCluster(ctx, req, &cluster, &placement, baseDomain)
}

func (r *ClusterReconciler) reconcilePlacedCluster(ctx context.Context, req ctrl.Request, cluster *seaplanev1alpha1.Cluster, placement *seaplanev1alpha1.Placement, baseDomain string) (ctrl.Result, error) {
	mc := placement.Spec.ManagementCluster
	specsPrefix := dynamo.SpecsPrefix(mc)
	statusPrefix := dynamo.StatusPrefix(mc)

	// Render resources and build common structures used by both paths.
	oidcSigningKeyExternal, err := r.oidcSigningKeyExternal(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve oidc signing key mode: %w", err)
	}
	resources, err := render.ClusterResources(cluster, oidcSigningKeyExternal, baseDomain, r.RegionalConfig.ControlPlaneOperatorImage)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render cluster resources: %w", err)
	}
	for _, resource := range resources {
		if resource.Resource == "hostedclusters" {
			hostedCluster, ok := resource.Object.(*hypershiftv1beta1.HostedCluster)
			if !ok {
				return ctrl.Result{}, fmt.Errorf("rendered HostedCluster has unexpected type %T", resource.Object)
			}
			if err := r.upsertRenderedHostedCluster(ctx, hostedCluster); err != nil {
				return ctrl.Result{}, err
			}
			break
		}
	}

	clusterID := string(cluster.UID)
	hcName := render.HostedClusterName(cluster)
	hcNs := render.ManagementNamespace(clusterID)
	readDocID := dynamo.NewDocumentID(taskKey+"-read", "hypershift.openshift.io", "v1beta1", "hostedclusters", hcNs, hcName)

	// Upsert ApplyDesires in parallel — no-op when content matches.
	type upsertResult struct {
		entry DesireStatusEntry
		err   error
	}
	upsertResults := make([]upsertResult, len(resources))
	var upsertWg sync.WaitGroup
	for i, m := range resources {
		upsertWg.Add(1)
		go func(idx int, m render.Resource) {
			defer upsertWg.Done()
			docID := dynamo.NewDocumentID(taskKey, m.Group, m.Version, m.Resource, m.Namespace, m.Name)
			content, marshalErr := json.Marshal(m.Object)
			if marshalErr != nil {
				upsertResults[idx] = upsertResult{err: fmt.Errorf("marshal resource %s: %w", m.Name, marshalErr)}
				return
			}
			desire := &dynamo.ApplyDesire{
				DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: docID},
				Spec: dynamo.ApplyDesireSpec{
					Type:              dynamo.ApplyDesireTypeServerSideApply,
					ManagementCluster: mc,
					ClusterID:         clusterID,
					TargetItem: dynamo.ResourceReference{
						Group:     m.Group,
						Version:   m.Version,
						Resource:  m.Resource,
						Namespace: m.Namespace,
						Name:      m.Name,
					},
					ServerSideApply: &dynamo.ServerSideApplyConfig{
						KubeContent: &runtime.RawExtension{Raw: content},
					},
				},
			}
			res, upsertErr := r.Dynamo.UpsertApplyDesire(ctx, specsPrefix, desire)
			if upsertErr != nil {
				upsertResults[idx] = upsertResult{err: fmt.Errorf("upsert apply desire %s: %w", m.Name, upsertErr)}
				return
			}
			upsertResults[idx] = upsertResult{entry: DesireStatusEntry{DocID: docID, Resource: m.Resource, Name: m.Name, DesireUpdateTime: res.UpdateTime}}
			if r.EventRouter != nil {
				r.EventRouter.Register(docID, EventTarget{Channel: r.StatusEvents, Key: req.NamespacedName})
			}
		}(i, m)
	}

	// Upsert ReadDesire concurrently with ApplyDesires.
	var readErr error
	upsertWg.Go(func() {
		readDesire := &dynamo.ReadDesire{
			DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: readDocID},
			Spec: dynamo.ReadDesireSpec{
				ManagementCluster: mc,
				ClusterID:         clusterID,
				TargetItem: dynamo.ResourceReference{
					Group:     "hypershift.openshift.io",
					Version:   "v1beta1",
					Resource:  "hostedclusters",
					Namespace: hcNs,
					Name:      hcName,
				},
			},
		}
		if _, err := r.Dynamo.UpsertReadDesire(ctx, specsPrefix, readDesire); err != nil {
			readErr = fmt.Errorf("upsert read desire: %w", err)
			return
		}
		if r.EventRouter != nil {
			r.EventRouter.Register(readDocID, EventTarget{Channel: r.StatusEvents, Key: req.NamespacedName})
		}
	})
	upsertWg.Wait()

	if readErr != nil {
		return ctrl.Result{}, readErr
	}
	var applyEntries []DesireStatusEntry
	for _, ur := range upsertResults {
		if ur.err != nil {
			return ctrl.Result{}, ur.err
		}
		applyEntries = append(applyEntries, ur.entry)
	}

	// Read status feedback from DynamoDB and update Cluster status.
	// Phase transitions (Provisioning, Ready) are handled inside updateStatusFromDynamo
	// to avoid clobbering Ready with a stale in-memory phase check.
	r.updateStatusFromDynamo(ctx, cluster, statusPrefix, readDocID, applyEntries)

	requeueAfter := statusRefreshDelay
	if cluster.Spec.ExpirationTimestamp != nil && !cluster.Spec.ExpirationTimestamp.IsZero() {
		if remaining := time.Until(cluster.Spec.ExpirationTimestamp.Time); remaining > 0 && remaining < requeueAfter {
			requeueAfter = remaining
		}
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *ClusterReconciler) reconcileDelete(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("Cluster deleting", "cluster", cluster.Name)
	r.setPhase(ctx, cluster, seaplanev1alpha1.ClusterPhaseDeleting)

	// Resolve the management cluster. If none is set, no resources were ever
	// placed, so skip straight to Placement/finalizer cleanup.
	mc := ""
	if cluster.Status.PlacementRef != nil {
		mc = cluster.Status.PlacementRef.ManagementCluster
	} else {
		placementName := fmt.Sprintf("%s.placement", cluster.Name)
		var placement seaplanev1alpha1.Placement
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: placementName}, &placement); err == nil {
			mc = placement.Spec.ManagementCluster
		}
	}
	if mc == "" {
		if err := r.deleteRenderedHostedCluster(ctx, string(cluster.UID), render.HostedClusterName(cluster)); err != nil {
			return ctrl.Result{}, err
		}
		return r.cleanupAndRemoveFinalizer(ctx, cluster)
	}

	// Delete NodePool CRs so HyperShift tears down worker nodes.
	var nodePools seaplanev1alpha1.NodePoolList
	if err := r.List(ctx, &nodePools,
		client.MatchingLabels{clusterUIDLabel: string(cluster.UID)},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("list nodepools: %w", err)
	}
	pendingNodePools := 0
	for i := range nodePools.Items {
		np := &nodePools.Items[i]
		if np.DeletionTimestamp.IsZero() {
			log.Info("Deleting NodePool", "nodePool", np.Name)
			if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete nodepool %s: %w", np.Name, err)
			}
		}
		pendingNodePools++
	}

	specsPrefix := dynamo.SpecsPrefix(mc)
	statusPrefix := dynamo.StatusPrefix(mc)
	clusterID := string(cluster.UID)
	ns := render.ManagementNamespace(clusterID)
	hcName := render.HostedClusterName(cluster)

	baseDomain := cluster.Status.BaseDomain

	// Render the cluster resources — must match the original resource set so
	// every ApplyDesire (including the OIDC signing key ExternalSecret, if
	// any) gets flipped to Delete below instead of left dangling.
	oidcSigningKeyExternal, err := r.oidcSigningKeyExternal(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve oidc signing key mode: %w", err)
	}
	resources, err := render.ClusterResources(cluster, oidcSigningKeyExternal, baseDomain, r.RegionalConfig.ControlPlaneOperatorImage)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render cluster resources: %w", err)
	}

	// Switch all 7 ApplyDesires to Type=Delete in-place, using the same
	// taskKey and documentID as the original SSA desires. kube-applier
	// sees a MODIFY stream event per resource and deletes each one from
	// the MC instead of re-applying it.
	type upsertResult struct {
		entry DesireStatusEntry
		err   error
	}
	upsertResults := make([]upsertResult, len(resources))
	var wg sync.WaitGroup
	for i, m := range resources {
		wg.Add(1)
		go func(idx int, m render.Resource) {
			defer wg.Done()
			docID := dynamo.NewDocumentID(taskKey, m.Group, m.Version, m.Resource, m.Namespace, m.Name)
			desire := &dynamo.ApplyDesire{
				DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: docID},
				Spec: dynamo.ApplyDesireSpec{
					Type:              dynamo.ApplyDesireTypeDelete,
					ManagementCluster: mc,
					ClusterID:         clusterID,
					TargetItem: dynamo.ResourceReference{
						Group:     m.Group,
						Version:   m.Version,
						Resource:  m.Resource,
						Namespace: m.Namespace,
						Name:      m.Name,
					},
				},
			}
			res, upsertErr := r.Dynamo.UpsertApplyDesire(ctx, specsPrefix, desire)
			upsertResults[idx] = upsertResult{
				entry: DesireStatusEntry{
					DocID:            docID,
					Resource:         m.Resource,
					Name:             m.Name,
					DesireUpdateTime: res.UpdateTime,
				},
				err: upsertErr,
			}
		}(i, m)
	}
	wg.Wait()

	var deleteEntries []DesireStatusEntry
	for _, ur := range upsertResults {
		if ur.err != nil {
			return ctrl.Result{}, fmt.Errorf("upsert delete desire for %s: %w", ur.entry.Name, ur.err)
		}
		deleteEntries = append(deleteEntries, ur.entry)
	}

	// Wait for all 7 resources to be confirmed deleted by kube-applier.
	syncedCond := CheckApplyDesireStatuses(ctx, r.Dynamo, statusPrefix, deleteEntries, cluster.Generation)
	r.setSyncedCondition(ctx, cluster, syncedCond)
	if syncedCond.Status != metav1.ConditionTrue {
		log.Info("Waiting for cluster resources to be deleted on management cluster")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Wait for NodePool CRs to be fully removed.
	if pendingNodePools > 0 {
		log.Info("Waiting for NodePools to be deleted", "count", pendingNodePools)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// All MC resources deleted — clean up desire specs from DynamoDB.
	var desireCleanupErr error
	readDocID := dynamo.NewDocumentID(taskKey+"-read", "hypershift.openshift.io", "v1beta1", "hostedclusters", ns, hcName)
	if err := r.Dynamo.DeleteDesireSpec(ctx, specsPrefix, "-readdesires", readDocID); err != nil {
		log.Error(err, "failed to clean up ReadDesire spec", "hostedcluster", hcName)
		desireCleanupErr = fmt.Errorf("delete ReadDesire spec for HostedCluster %s: %w", hcName, err)
	}
	for _, m := range resources {
		docID := dynamo.NewDocumentID(taskKey, m.Group, m.Version, m.Resource, m.Namespace, m.Name)
		if err := r.Dynamo.DeleteDesireSpec(ctx, specsPrefix, "-applydesires", docID); err != nil {
			log.Error(err, "failed to clean up ApplyDesire spec", "resource", m.Name)
			if desireCleanupErr == nil {
				desireCleanupErr = fmt.Errorf("delete ApplyDesire spec for %s: %w", m.Name, err)
			}
		}
	}
	if desireCleanupErr != nil {
		return ctrl.Result{}, desireCleanupErr
	}

	if err := r.deleteRenderedHostedCluster(ctx, clusterID, hcName); err != nil {
		return ctrl.Result{}, err
	}

	return r.cleanupAndRemoveFinalizer(ctx, cluster)
}

func (r *ClusterReconciler) cleanupAndRemoveFinalizer(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (ctrl.Result, error) {
	clusterOwned := client.MatchingLabels{clusterUIDLabel: string(cluster.UID)}

	var nodePools seaplanev1alpha1.NodePoolList
	if err := r.List(ctx, &nodePools, clusterOwned); err != nil {
		return ctrl.Result{}, fmt.Errorf("list owned NodePools for cleanup: %w", err)
	}
	if len(nodePools.Items) > 0 {
		for i := range nodePools.Items {
			nodePool := &nodePools.Items[i]
			if nodePool.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, nodePool); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("delete owned NodePool %s: %w", nodePool.Name, err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	var placements seaplanev1alpha1.PlacementList
	if err := r.List(ctx, &placements, clusterOwned); err != nil {
		return ctrl.Result{}, fmt.Errorf("list owned Placements for cleanup: %w", err)
	}
	if len(placements.Items) > 0 {
		for i := range placements.Items {
			if err := r.Delete(ctx, &placements.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete owned Placement %s: %w", placements.Items[i].Name, err)
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Release the 1:1 binding claim this cluster took on its referenced OidcConfig, freeing it for reuse; retries on conflict since a missed release permanently orphans the config.
	if cluster.Spec.OidcConfigID != "" {
		if err := r.releaseOidcConfigClaim(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("release oidc config claim: %w", err)
		}
	}

	// Delete only DNSReservations still claimed by this Cluster UID. A reference
	// can be stale after a claim release or point to a reservation claimed by a
	// different Cluster; the claim label also finds automatic reservations
	// created just before the Cluster reference was persisted.
	if cluster.UID != "" {
		accountID := cluster.Spec.AccountID
		if accountID == "" {
			accountID = cluster.Labels[accountIDLabel]
		}
		var reservations seaplanev1alpha1.DNSReservationList
		if err := r.List(ctx, &reservations,
			client.InNamespace(accountNamespace(accountID)),
			client.MatchingLabels{claimedByClusterUIDLabel: string(cluster.UID)},
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("find claimed DNSReservations for Cluster UID %s: %w", cluster.UID, err)
		}
		for i := range reservations.Items {
			reservation := &reservations.Items[i]
			if !reservation.DeletionTimestamp.IsZero() {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			deletionPolicy := reservation.Spec.ClusterDeletionPolicy
			if deletionPolicy == "" && reservation.Name == legacyAutomaticDNSReservationName(cluster.Name, string(cluster.UID)) {
				// Older automatic reservations predate ClusterDeletionPolicy.
				deletionPolicy = seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete
			}
			if deletionPolicy != seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete {
				if err := r.releaseDNSReservationClaim(ctx, reservation, string(cluster.UID)); err != nil {
					return ctrl.Result{}, fmt.Errorf("release DNSReservation claim: %w", err)
				}
				continue
			}

			if reservation.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, reservation); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("delete claimed DNSReservation: %w", err)
				}
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.Cluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(&latest, clusterFinalizer) {
			return nil
		}
		controllerutil.RemoveFinalizer(&latest, clusterFinalizer)
		return r.Update(ctx, &latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *ClusterReconciler) releaseDNSReservationClaim(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation, clusterUID string) error {
	if reservation == nil || clusterUID == "" {
		return nil
	}

	key := client.ObjectKeyFromObject(reservation)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.DNSReservation
		if err := r.Get(ctx, key, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != reservation.UID || latest.Labels[claimedByClusterUIDLabel] != clusterUID {
			return nil
		}
		delete(latest.Labels, claimedByClusterUIDLabel)
		return r.Update(ctx, &latest)
	})
}

func (r *ClusterReconciler) updateStatusFromDynamo(ctx context.Context, cluster *seaplanev1alpha1.Cluster, statusPrefix, readDocID string, applyEntries []DesireStatusEntry) {
	log := logf.FromContext(ctx)

	// Read HC status and check apply desire statuses in parallel.
	var readStatus *dynamo.ReadDesireStatus
	var readErr error
	var syncedCond metav1.Condition
	var wg sync.WaitGroup

	wg.Go(func() {
		readStatus, readErr = r.Dynamo.GetReadDesireStatus(ctx, statusPrefix, readDocID)
	})

	if len(applyEntries) > 0 {
		wg.Go(func() {
			syncedCond = CheckApplyDesireStatuses(ctx, r.Dynamo, statusPrefix, applyEntries, cluster.Generation)
		})
	}
	wg.Wait()

	if readErr != nil {
		log.V(1).Info("ReadDesire status not yet available", "error", readErr)
	}

	var hc struct {
		Status struct {
			Conditions []metav1.Condition `json:"conditions"`
			Version    struct {
				History []struct {
					Version string `json:"version"`
				} `json:"history"`
			} `json:"version"`
			ControlPlaneEndpoint hypershiftv1beta1.APIEndpoint `json:"controlPlaneEndpoint"`
		} `json:"status"`
	}
	if readStatus != nil && readStatus.KubeContent != nil {
		if err := json.Unmarshal(readStatus.KubeContent.Raw, &hc); err != nil {
			log.Error(err, "Failed to unmarshal HostedCluster status")
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.Cluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}

		if len(applyEntries) > 0 {
			meta.SetStatusCondition(&latest.Status.Conditions, syncedCond)
		}

		if readStatus != nil && readStatus.KubeContent != nil {
			for _, cond := range hc.Status.Conditions {
				if cond.Type == "Available" || cond.Type == "Degraded" {
					meta.SetStatusCondition(&latest.Status.Conditions, cond)
				}
			}
			if hc.Status.ControlPlaneEndpoint.Host != "" {
				latest.Status.ControlPlaneEndpoint = hc.Status.ControlPlaneEndpoint
			}
			if len(hc.Status.Version.History) > 0 {
				latest.Status.Version = hc.Status.Version.History[0].Version
			}
		}

		if meta.IsStatusConditionTrue(latest.Status.Conditions, "Available") &&
			!meta.IsStatusConditionTrue(latest.Status.Conditions, "Degraded") {
			latest.Status.Phase = seaplanev1alpha1.ClusterPhaseReady
		} else if latest.Status.Phase == "" || latest.Status.Phase == seaplanev1alpha1.ClusterPhaseWaitingForPlacement {
			latest.Status.Phase = seaplanev1alpha1.ClusterPhaseProvisioning
		}
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		log.Error(err, "Failed to update cluster status from DynamoDB feedback")
	}
}

func (r *ClusterReconciler) setSyncedCondition(ctx context.Context, cluster *seaplanev1alpha1.Cluster, cond metav1.Condition) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.Cluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, cond)
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update Synced condition")
	}
}

func (r *ClusterReconciler) deleteIfExpired(ctx context.Context, cluster *seaplanev1alpha1.Cluster) (bool, error) {
	if cluster.Spec.ExpirationTimestamp == nil || cluster.Spec.ExpirationTimestamp.IsZero() ||
		!time.Now().After(cluster.Spec.ExpirationTimestamp.Time) {
		return false, nil
	}
	logf.FromContext(ctx).Info("Cluster expired, triggering deletion", "expirationTimestamp", cluster.Spec.ExpirationTimestamp.Time)
	if err := r.Delete(ctx, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, fmt.Errorf("delete expired cluster: %w", err)
	}
	return true, nil
}

func (r *ClusterReconciler) setPhase(ctx context.Context, cluster *seaplanev1alpha1.Cluster, phase seaplanev1alpha1.ClusterPhase) {
	if cluster.Status.Phase == phase {
		return
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.Cluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if latest.Status.Phase == phase {
			return nil
		}
		latest.Status.Phase = phase
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update cluster phase", "phase", phase)
	}
}

// defaultDNSShard is the single DNS zone shard used today. When multi-shard
// routing is needed, this will be replaced by dynamic shard selection.
const defaultDNSShard = "0"

func accountNamespace(accountID string) string {
	return accountNSPrefix + accountID
}

func dnsShardNamespace(shard string) string {
	return "dns-shard-" + shard + "-reservations"
}

func (r *ClusterReconciler) persistBaseDomain(ctx context.Context, cluster *seaplanev1alpha1.Cluster, baseDomain string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.Cluster
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &latest); err != nil {
			return err
		}
		latest.Status.BaseDomain = baseDomain
		return r.Status().Update(ctx, &latest)
	})
}
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.Cluster{}).
		Owns(&seaplanev1alpha1.Placement{}).
		Named("cluster")

	if r.StatusEvents != nil {
		b = b.WatchesRawSource(source.Channel(
			r.StatusEvents,
			handler.EnqueueRequestsFromMapFunc(
				func(_ context.Context, obj client.Object) []reconcile.Request {
					return []reconcile.Request{{
						NamespacedName: types.NamespacedName{
							Namespace: obj.GetNamespace(),
							Name:      obj.GetName(),
						},
					}}
				},
			),
		))
	}

	return b.Complete(r)
}
