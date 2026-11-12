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
	"fmt"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/oidc"
)

const (
	oidcConfigFinalizer    = "seaplane.io/oidcconfig"
	thumbprintRefreshDelay = 24 * time.Hour

	// managedPendingRequeueInterval controls how often a Pending managed OidcConfig re-checks its issuer.
	managedPendingRequeueInterval = 30 * time.Second

	// issuerURLConflictRequeueInterval is the recheck interval for a config parked on IssuerURLConflict.
	issuerURLConflictRequeueInterval = 5 * time.Minute
)

// OidcConfigReconciler reconciles OidcConfig objects
type OidcConfigReconciler struct {
	client.Client
	APIReader               client.Reader
	Scheme                  *runtime.Scheme
	OIDC                    oidc.InfraClient
	MaxConcurrentReconciles int
}

func (r *OidcConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var oc seaplanev1alpha1.OidcConfig
	if err := r.Get(ctx, req.NamespacedName, &oc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !oc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &oc)
	}

	if !controllerutil.ContainsFinalizer(&oc, oidcConfigFinalizer) {
		controllerutil.AddFinalizer(&oc, oidcConfigFinalizer)
		if err := r.Update(ctx, &oc); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// The Update above triggers a new reconcile via the watch on this
		// object, so no explicit requeue is needed here.
		return ctrl.Result{}, nil
	}

	switch oc.Spec.Type {
	case seaplanev1alpha1.OidcConfigTypeManaged:
		return r.reconcileManaged(ctx, &oc)
	case seaplanev1alpha1.OidcConfigTypeUnmanaged:
		return r.reconcileUnmanaged(ctx, &oc)
	default:
		r.setReadyConditionAndPhase(ctx, &oc, "InvalidType", "unknown type: "+oc.Spec.Type, seaplanev1alpha1.OidcConfigPhaseError)
		return ctrl.Result{}, nil
	}
}

// reconcileManaged marks a managed OidcConfig Ready once a cluster references it and its issuer is TLS-reachable.
func (r *OidcConfigReconciler) reconcileManaged(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) (ctrl.Result, error) {
	if oc.Status.Phase == "" {
		r.setPhase(ctx, oc, seaplanev1alpha1.OidcConfigPhasePending)
	}

	if reserved, result, err := r.reserveIssuerURLIndex(ctx, oc); err != nil || !reserved {
		return result, err
	}

	referenced, err := r.isReferencedByCluster(ctx, oc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check cluster references: %w", err)
	}
	if !referenced {
		r.setReadyConditionAndPhase(ctx, oc, "AwaitingCluster",
			"Waiting for a cluster to be created that references this OIDC config", seaplanev1alpha1.OidcConfigPhasePending)
		return ctrl.Result{RequeueAfter: managedPendingRequeueInterval}, nil
	}

	return r.checkReadiness(ctx, oc, func(_, _ string) (ctrl.Result, error) {
		r.setReadyConditionAndPhase(ctx, oc, "IssuerNotReady",
			"Waiting for the referenced cluster's control plane to publish its OIDC configuration", seaplanev1alpha1.OidcConfigPhasePending)
		return ctrl.Result{RequeueAfter: managedPendingRequeueInterval}, nil
	})
}

// isReferencedByCluster reports whether any live (non-terminating) Cluster owned by oc's account
// currently sets this oidcConfigId
func (r *OidcConfigReconciler) isReferencedByCluster(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) (bool, error) {
	var clusters seaplanev1alpha1.ClusterList
	if err := r.List(ctx, &clusters, client.MatchingLabels{accountIDLabel: oc.Spec.AccountID}); err != nil {
		return false, fmt.Errorf("list clusters: %w", err)
	}
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if c.DeletionTimestamp.IsZero() && c.Spec.OidcConfigID == string(oc.UID) {
			return true, nil
		}
	}
	return false, nil
}

// reserveIssuerURLIndex creates/adopts oc's issuer-URL Index, owned by the OidcConfig UID.
// reserved=false means another OidcConfig holds it; the caller should return the given result/err as-is.
func (r *OidcConfigReconciler) reserveIssuerURLIndex(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) (bool, ctrl.Result, error) {
	indexNS := seaplanev1alpha1.OidcIssuerReservationsNamespace
	indexName := seaplanev1alpha1.IssuerURLIndexName(oc.Spec.IssuerUrl)

	idx := &seaplanev1alpha1.Index{
		ObjectMeta: metav1.ObjectMeta{
			Name:      indexName,
			Namespace: indexNS,
			Labels: map[string]string{
				accountIDLabel: oc.Spec.AccountID,
				ownerUIDLabel:  string(oc.UID),
			},
		},
		Spec: seaplanev1alpha1.IndexSpec{},
	}

	if err := r.Create(ctx, idx); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, ctrl.Result{}, fmt.Errorf("create issuer url index: %w", err)
		}
		var existing seaplanev1alpha1.Index
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(idx), &existing); err != nil {
			return false, ctrl.Result{}, fmt.Errorf("get issuer url index: %w", err)
		}
		if existing.Labels[ownerUIDLabel] != string(oc.UID) {
			r.setReadyConditionAndPhase(ctx, oc, "IssuerURLConflict",
				fmt.Sprintf("issuerUrl %q is already reserved by another OIDC config", oc.Spec.IssuerUrl),
				seaplanev1alpha1.OidcConfigPhaseError)
			return false, ctrl.Result{RequeueAfter: issuerURLConflictRequeueInterval}, nil
		}
		// We own it already (idempotent re-entry) — fall through to readiness logic.
	}

	return true, ctrl.Result{}, nil
}

// deleteIssuerURLIndex derives the deterministic key from the immutable issuer URL
// and deletes it only when the Index is still owned by this OidcConfig UID.
func (r *OidcConfigReconciler) deleteIssuerURLIndex(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) error {
	var index seaplanev1alpha1.Index
	key := client.ObjectKey{
		Namespace: seaplanev1alpha1.OidcIssuerReservationsNamespace,
		Name:      seaplanev1alpha1.IssuerURLIndexName(oc.Spec.IssuerUrl),
	}
	if err := r.reader().Get(ctx, key, &index); err != nil {
		return client.IgnoreNotFound(err)
	}
	if index.Labels[ownerUIDLabel] != string(oc.UID) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &index))
}

func (r *OidcConfigReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *OidcConfigReconciler) reconcileUnmanaged(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	configID := oc.Name

	if oc.Status.Phase == "" {
		r.setPhase(ctx, oc, seaplanev1alpha1.OidcConfigPhasePending)
	}

	if reserved, result, err := r.reserveIssuerURLIndex(ctx, oc); err != nil || !reserved {
		return result, err
	}

	exists, err := r.OIDC.PrivateKeyExists(ctx, oc.Spec.AccountID, configID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("check private key: %w", err)
	}

	if !exists {
		log.Info("Copying customer private key to local Secrets Manager", "config", configID)

		keyData, err := r.OIDC.ReadCrossAccountSecret(ctx, oc.Spec.SecretArn, oc.Spec.InstallerRoleArn)
		if err != nil {
			r.setReadyCondition(ctx, oc, "CrossAccountReadFailed", err.Error())
			return ctrl.Result{}, fmt.Errorf("read cross-account secret: %w", err)
		}

		if err := oidc.ValidateRSAPrivateKey(keyData); err != nil {
			r.setReadyConditionAndPhase(ctx, oc, "InvalidPrivateKey", err.Error(), seaplanev1alpha1.OidcConfigPhaseError)
			return ctrl.Result{}, nil
		}

		if err := r.OIDC.StorePrivateKey(ctx, oc.Spec.AccountID, configID, keyData); err != nil {
			r.setReadyCondition(ctx, oc, "SecretStoreFailed", err.Error())
			return ctrl.Result{}, fmt.Errorf("store private key: %w", err)
		}
	}

	return r.checkReadiness(ctx, oc, func(reason, message string) (ctrl.Result, error) {
		// Returning the error lets controller-runtime's rate limiter apply exponential backoff.
		r.setReadyConditionAndPhase(ctx, oc, reason, message, seaplanev1alpha1.OidcConfigPhaseError)
		return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
	})
}

// checkReadiness confirms oc's issuer is TLS-reachable and on success, sets the status to Ready.
func (r *OidcConfigReconciler) checkReadiness(ctx context.Context, oc *seaplanev1alpha1.OidcConfig, onFailure func(reason, message string) (ctrl.Result, error)) (ctrl.Result, error) {
	thumbprint, err := r.OIDC.ComputeThumbprint(ctx, oc.Spec.IssuerUrl)
	if err != nil {
		logf.FromContext(ctx).V(1).Info("issuer not ready", "issuerUrl", oc.Spec.IssuerUrl, "error", err.Error())
		return onFailure("IssuerNotReady", err.Error())
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.OidcConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(oc), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "OIDCConfigured",
			Message: "OIDC infrastructure is configured and ready",
		})
		latest.Status.Phase = seaplanev1alpha1.OidcConfigPhaseReady
		latest.Status.Thumbprint = thumbprint
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{RequeueAfter: thumbprintRefreshDelay}, nil
}

func (r *OidcConfigReconciler) reconcileDelete(ctx context.Context, oc *seaplanev1alpha1.OidcConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(oc, oidcConfigFinalizer) {
		return ctrl.Result{}, nil
	}

	configID := oc.Name
	log.Info("OidcConfig deleting", "config", configID, "type", oc.Spec.Type)

	// Only unmanaged configs ever store a private key
	if oc.Spec.Type == seaplanev1alpha1.OidcConfigTypeUnmanaged {
		if err := r.OIDC.DeletePrivateKey(ctx, oc.Spec.AccountID, configID); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete private key: %w", err)
		}
	}

	if err := r.deleteIssuerURLIndex(ctx, oc); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete issuer url index: %w", err)
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.OidcConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(oc), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(&latest, oidcConfigFinalizer) {
			return nil
		}
		controllerutil.RemoveFinalizer(&latest, oidcConfigFinalizer)
		return r.Update(ctx, &latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// --- status helpers ---

func (r *OidcConfigReconciler) setPhase(ctx context.Context, oc *seaplanev1alpha1.OidcConfig, phase seaplanev1alpha1.OidcConfigPhase) {
	if oc.Status.Phase == phase {
		return
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.OidcConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(oc), &latest); err != nil {
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
		logf.FromContext(ctx).Error(err, "Failed to update oidcconfig phase", "phase", phase)
	}
}

func (r *OidcConfigReconciler) setReadyCondition(ctx context.Context, oc *seaplanev1alpha1.OidcConfig, reason, message string) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.OidcConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(oc), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update Ready condition")
	}
}

// setReadyConditionAndPhase updates the Ready condition and the phase together in a single Get + Status().Update retry loop
func (r *OidcConfigReconciler) setReadyConditionAndPhase(ctx context.Context, oc *seaplanev1alpha1.OidcConfig, reason, message string, phase seaplanev1alpha1.OidcConfigPhase) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.OidcConfig
		if err := r.Get(ctx, client.ObjectKeyFromObject(oc), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		})
		latest.Status.Phase = phase
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update Ready condition and phase", "phase", phase)
	}
}

func (r *OidcConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.OidcConfig{}).
		Named("oidcconfig").
		Complete(r)
}
