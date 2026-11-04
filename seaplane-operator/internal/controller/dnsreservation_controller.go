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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
)

const (
	dnsReservationFinalizer          = "seaplane.io/dnsreservation"
	dnsReservationAllocationAttempts = 5
	dnsReservationRetryInterval      = 30 * time.Second
)

// DNSReservationReconciler assigns a shard-unique base domain to a customer
// reservation and owns the corresponding Index by the reservation's FleetDB UID.
type DNSReservationReconciler struct {
	client.Client
	APIReader               client.Reader
	Scheme                  *runtime.Scheme
	RegionalConfig          render.RegionalConfig
	MaxConcurrentReconciles int
	generatePrefix          func() (string, error)
}

// +kubebuilder:rbac:groups=seaplane.io,resources=dnsreservations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=seaplane.io,resources=dnsreservations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=seaplane.io,resources=dnsreservations/finalizers,verbs=update
// +kubebuilder:rbac:groups=seaplane.io,resources=indices,verbs=get;list;watch;create;delete

func (r *DNSReservationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var reservation seaplanev1alpha1.DNSReservation
	if err := r.Get(ctx, req.NamespacedName, &reservation); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !reservation.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &reservation)
	}

	if !controllerutil.ContainsFinalizer(&reservation, dnsReservationFinalizer) {
		controllerutil.AddFinalizer(&reservation, dnsReservationFinalizer)
		if err := r.Update(ctx, &reservation); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("add DNSReservation finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if reservation.UID == "" {
		return ctrl.Result{}, fmt.Errorf("DNSReservation %s/%s has no database UID", reservation.Namespace, reservation.Name)
	}
	if r.RegionalConfig.BaseDomainSuffix == "" {
		return ctrl.Result{}, fmt.Errorf("DNS reservation base domain suffix is not configured")
	}

	index, err := r.recoverOwnedIndex(ctx, &reservation)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("recover DNS Index for reservation UID %s: %w", reservation.UID, err)
	}
	if index == nil {
		index, err = r.allocateIndex(ctx, &reservation)
		if err != nil {
			return ctrl.Result{}, err
		}
		if index == nil {
			message := fmt.Sprintf("failed to reserve a DNS prefix after %d attempts", dnsReservationAllocationAttempts)
			if err := r.updateReservationStatus(ctx, &reservation, seaplanev1alpha1.DNSReservationPhasePending, "", message); err != nil {
				return ctrl.Result{}, fmt.Errorf("record DNS allocation failure: %w", err)
			}
			return ctrl.Result{RequeueAfter: dnsReservationRetryInterval}, nil
		}
	}

	if err := r.deleteDuplicateOwnedIndexes(ctx, &reservation, index); err != nil {
		return ctrl.Result{}, fmt.Errorf("clean up duplicate DNS Indexes: %w", err)
	}

	baseDomain := fmt.Sprintf("%s.%s.%s", index.Name, defaultDNSShard, r.RegionalConfig.BaseDomainSuffix)
	if err := r.updateReservationStatus(ctx, &reservation, seaplanev1alpha1.DNSReservationPhaseReady, baseDomain, ""); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark DNSReservation Ready: %w", err)
	}
	return ctrl.Result{RequeueAfter: statusRefreshDelay}, nil
}

func (r *DNSReservationReconciler) reconcileDelete(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) (ctrl.Result, error) {
	indexes, err := r.listOwnedIndexes(ctx, string(reservation.UID))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list owned DNS Indexes for deletion: %w", err)
	}
	for i := range indexes.Items {
		idx := &indexes.Items[i]
		if idx.Labels[ownerUIDLabel] != string(reservation.UID) {
			continue
		}
		if err := r.Delete(ctx, idx); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete DNS Index %s/%s: %w", idx.Namespace, idx.Name, err)
		}
	}

	remaining, err := r.listOwnedIndexes(ctx, string(reservation.UID))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("confirm DNS Index cleanup: %w", err)
	}
	if len(remaining.Items) > 0 {
		return ctrl.Result{RequeueAfter: dnsReservationRetryInterval}, nil
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.DNSReservation
		if err := r.Get(ctx, client.ObjectKeyFromObject(reservation), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != reservation.UID || !controllerutil.ContainsFinalizer(&latest, dnsReservationFinalizer) {
			return nil
		}
		controllerutil.RemoveFinalizer(&latest, dnsReservationFinalizer)
		return r.Update(ctx, &latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove DNSReservation finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// recoverOwnedIndex finds an Index left by an earlier reconcile using the stable
// owner UID label. This recovers the crash window before reservation status was
// persisted and lets this controller remove same-owner duplicates.
func (r *DNSReservationReconciler) recoverOwnedIndex(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) (*seaplanev1alpha1.Index, error) {
	indexes, err := r.listOwnedIndexes(ctx, string(reservation.UID))
	if err != nil {
		return nil, err
	}
	var expected []*seaplanev1alpha1.Index
	for i := range indexes.Items {
		index := &indexes.Items[i]
		if index.Labels[ownerUIDLabel] != string(reservation.UID) {
			continue
		}
		if index.Namespace != dnsShardNamespace(defaultDNSShard) {
			if err := r.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete owned Index from unexpected shard %s: %w", index.Namespace, err)
			}
			continue
		}
		expected = append(expected, index)
	}
	if len(expected) == 0 {
		return nil, nil
	}
	sort.Slice(expected, func(i, j int) bool { return expected[i].Name < expected[j].Name })
	return expected[0], nil
}

func (r *DNSReservationReconciler) allocateIndex(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) (*seaplanev1alpha1.Index, error) {
	accountID := reservation.Labels[accountIDLabel]
	if accountID == "" {
		return nil, fmt.Errorf("DNSReservation %s/%s has no %s label", reservation.Namespace, reservation.Name, accountIDLabel)
	}
	generate := r.generatePrefix
	if generate == nil {
		generate = randomDNSPrefix
	}
	indexNamespace := dnsShardNamespace(defaultDNSShard)

	for range dnsReservationAllocationAttempts {
		prefix, err := generate()
		if err != nil {
			return nil, fmt.Errorf("generate DNS prefix: %w", err)
		}
		index := &seaplanev1alpha1.Index{
			ObjectMeta: metav1.ObjectMeta{
				Name:      prefix,
				Namespace: indexNamespace,
				Labels: map[string]string{
					accountIDLabel: accountID,
					ownerUIDLabel:  string(reservation.UID),
				},
			},
			Spec: seaplanev1alpha1.IndexSpec{},
		}
		if err := r.Create(ctx, index); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create DNS Index %s/%s: %w", index.Namespace, index.Name, err)
			}
			existing := &seaplanev1alpha1.Index{}
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(index), existing); err != nil {
				return nil, fmt.Errorf("get colliding DNS Index %s/%s: %w", index.Namespace, index.Name, err)
			}
			if existing.Labels[ownerUIDLabel] != string(reservation.UID) {
				continue
			}
		}

		owned, err := r.recoverOwnedIndex(ctx, reservation)
		if err != nil {
			return nil, fmt.Errorf("recover DNS Index after create: %w", err)
		}
		if owned != nil {
			return owned, nil
		}
		return nil, fmt.Errorf("DNS Index %s/%s was created but is not visible by owner UID %s", index.Namespace, index.Name, reservation.UID)
	}
	return nil, nil
}

func (r *DNSReservationReconciler) deleteDuplicateOwnedIndexes(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation, canonical *seaplanev1alpha1.Index) error {
	indexes, err := r.listOwnedIndexes(ctx, string(reservation.UID))
	if err != nil {
		return err
	}
	for i := range indexes.Items {
		index := &indexes.Items[i]
		if index.Labels[ownerUIDLabel] != string(reservation.UID) ||
			(index.Namespace == canonical.Namespace && index.Name == canonical.Name) {
			continue
		}
		if err := r.Delete(ctx, index); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete duplicate Index %s/%s: %w", index.Namespace, index.Name, err)
		}
	}
	return nil
}

// updateReservationStatus persists allocation state and the shared internal
// HostedCluster name. Customer-created reservations keep their UID-derived
// name through claims; automatic reservations follow the already-created Cluster.
func (r *DNSReservationReconciler) updateReservationStatus(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation, phase seaplanev1alpha1.DNSReservationPhase, baseDomain, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.DNSReservation
		if err := r.Get(ctx, client.ObjectKeyFromObject(reservation), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != reservation.UID || !latest.DeletionTimestamp.IsZero() {
			return nil
		}
		internalHostedClusterName, err := r.resolveInternalHostedClusterName(ctx, &latest)
		if err != nil {
			return err
		}
		before := latest.Status.DeepCopy()
		latest.Status.Phase = phase
		latest.Status.BaseDomain = baseDomain
		latest.Status.InternalHostedClusterName = internalHostedClusterName
		latest.Status.ObservedGeneration = latest.Generation
		if message != "" {
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "PrefixAllocationFailed",
				Message:            message,
				ObservedGeneration: latest.Generation,
				LastTransitionTime: metav1.Now(),
			})
		} else {
			meta.RemoveStatusCondition(&latest.Status.Conditions, "Ready")
		}
		if reflect.DeepEqual(*before, latest.Status) {
			return nil
		}
		return r.Status().Update(ctx, &latest)
	})
}

func (r *DNSReservationReconciler) resolveInternalHostedClusterName(ctx context.Context, reservation *seaplanev1alpha1.DNSReservation) (string, error) {
	if !isAutomaticDNSReservation(reservation) {
		name := seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)
		if name == "" {
			return "", fmt.Errorf("derive internal HostedCluster name from DNSReservation UID %q", reservation.UID)
		}
		return name, nil
	}

	if clusterUID := reservation.Labels[claimedByClusterUIDLabel]; clusterUID != "" {
		var clusters seaplanev1alpha1.ClusterList
		if err := listByMetadataUID(ctx, r, &clusters, reservation.Namespace, clusterUID); err != nil {
			return "", fmt.Errorf("find Cluster UID %s for DNSReservation %s/%s: %w", clusterUID, reservation.Namespace, reservation.Name, err)
		}
		for i := range clusters.Items {
			cluster := &clusters.Items[i]
			if string(cluster.UID) != clusterUID {
				continue
			}
			if name := cluster.Spec.HostedClusterName; name != "" && name != seaplanev1alpha1.HostedClusterNamePending {
				return name, nil
			}
			return cluster.Name, nil
		}
		return "", fmt.Errorf("cluster UID %s claiming DNSReservation %s/%s was not found", clusterUID, reservation.Namespace, reservation.Name)
	}

	return "", fmt.Errorf("automatic DNSReservation %s/%s has no claiming Cluster UID", reservation.Namespace, reservation.Name)
}

func isAutomaticDNSReservation(reservation *seaplanev1alpha1.DNSReservation) bool {
	if reservation == nil {
		return false
	}
	if reservation.Spec.ClusterDeletionPolicy == seaplanev1alpha1.DNSReservationClusterDeletionPolicyDelete {
		return true
	}
	clusterUID := reservation.Labels[claimedByClusterUIDLabel]
	if clusterUID == "" {
		return false
	}
	return reservation.Name == "auto-dns-"+clusterUID || strings.HasSuffix(reservation.Name, "-auto-dns-"+clusterUID)
}

func (r *DNSReservationReconciler) listOwnedIndexes(ctx context.Context, uid string) (*seaplanev1alpha1.IndexList, error) {
	var indexes seaplanev1alpha1.IndexList
	if err := r.reader().List(ctx, &indexes, client.MatchingLabels{ownerUIDLabel: uid}); err != nil {
		return nil, err
	}
	return &indexes, nil
}

func (r *DNSReservationReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func randomDNSPrefix() (string, error) {
	bytes := make([]byte, 2)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func (r *DNSReservationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.DNSReservation{}).
		Named("dnsreservation").
		Complete(r)
}
