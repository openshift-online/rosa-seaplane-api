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
	"strings"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

// PlacementReconciler watches Cluster CRs and ensures each has a Placement.
type PlacementReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=seaplane.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=seaplane.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=seaplane.io,resources=placements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=placements/status,verbs=get;update;patch

func (r *PlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster seaplanev1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Don't create placements for clusters being deleted.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	placementName := fmt.Sprintf("%s.placement", cluster.Name)

	var placement seaplanev1alpha1.Placement
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: placementName}, &placement)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get placement: %w", err)
	}

	if apierrors.IsNotFound(err) {
		mc, err := r.selectManagementCluster(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("select management cluster: %w", err)
		}
		log.Info("Creating Placement for Cluster", "cluster", cluster.Name, "mc", mc)
		placement = seaplanev1alpha1.Placement{
			ObjectMeta: metav1.ObjectMeta{
				Name:      placementName,
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					clusterUIDLabel: string(cluster.UID),
				},
			},
			Spec: seaplanev1alpha1.PlacementSpec{
				ClusterName:       cluster.Name,
				ManagementCluster: mc,
			},
		}
		if err := controllerutil.SetControllerReference(&cluster, &placement, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set Placement ownerReference: %w", err)
		}
		if err := r.Create(ctx, &placement); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The Create failed, so no watch event is emitted; requeue explicitly.
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("create placement: %w", err)
		}
	}

	// Ensure Placement status is Bound.
	if placement.Status.Phase != seaplanev1alpha1.PlacementPhaseBound {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest seaplanev1alpha1.Placement
			if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: placementName}, &latest); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if latest.Status.Phase == seaplanev1alpha1.PlacementPhaseBound {
				return nil
			}
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type:               "Bound",
				Status:             metav1.ConditionTrue,
				Reason:             "Assigned",
				Message:            fmt.Sprintf("Assigned to management cluster %s", latest.Spec.ManagementCluster),
				ObservedGeneration: latest.Generation,
			})
			latest.Status.Phase = seaplanev1alpha1.PlacementPhaseBound
			latest.Status.ObservedGeneration = latest.Generation
			return r.Status().Update(ctx, &latest)
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("update placement status: %w", err)
		}
		log.Info("Placement bound", "placement", placement.Name, "mc", placement.Spec.ManagementCluster)
	}

	// Update Cluster status with PlacementRef if not already set.
	if cluster.Status.PlacementRef == nil ||
		cluster.Status.PlacementRef.Name != placementName ||
		cluster.Status.PlacementRef.ManagementCluster != placement.Spec.ManagementCluster {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest seaplanev1alpha1.Cluster
			if err := r.Get(ctx, client.ObjectKeyFromObject(&cluster), &latest); err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			latest.Status.PlacementRef = &seaplanev1alpha1.PlacementReference{
				Name:              placementName,
				ManagementCluster: placement.Spec.ManagementCluster,
			}
			return r.Status().Update(ctx, &latest)
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("update cluster placement ref: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// TODO: Implement proper MC scheduling (least-loaded, capacity-aware, affinity).
func (r *PlacementReconciler) selectManagementCluster(ctx context.Context) (string, error) {
	var list seaplanev1alpha1.ManagementClusterList
	if err := r.List(ctx, &list); err != nil {
		return "", fmt.Errorf("list management clusters: %w", err)
	}
	for i := range list.Items {
		// Skip fake MCs created by e2e tests.
		if !strings.HasPrefix(list.Items[i].Name, "test-mc") {
			return list.Items[i].Name, nil
		}
	}
	return "", fmt.Errorf("no management clusters configured")
}

func (r *PlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.Cluster{}).
		Owns(&seaplanev1alpha1.Placement{}).
		Named("placement").
		Complete(r)
}
