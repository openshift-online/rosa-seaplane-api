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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
)

// PlacementReconciler watches Cluster CRs and ensures each has a Placement on
// a management cluster and an HCP slot.
type PlacementReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	MaxConcurrentReconciles int
	// HCPSlotCount is the number of HCP slots on every management cluster.
	HCPSlotCount int
}

func (r *PlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	placementName := fmt.Sprintf("%s.placement", req.Name)
	var placement seaplanev1alpha1.Placement
	err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: placementName}, &placement)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get placement: %w", err)
	}
	placementFound := err == nil
	if placementFound && !placement.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizePlacement(ctx, &placement)
	}

	var cluster seaplanev1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Don't create placements for clusters being deleted.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if !placementFound {
		mc, slot, err := r.claimSlot(ctx, &cluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("select management cluster: %w", err)
		}
		if slot == 0 {
			log.Info("No management cluster has a free HCP slot", "cluster", cluster.Name)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		created, err := r.createPlacement(ctx, &cluster, placementName, mc, slot)
		if err != nil {
			return ctrl.Result{}, err
		}
		placement = *created
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

// createPlacement writes the Cluster's Placement on (mc, slot).
func (r *PlacementReconciler) createPlacement(ctx context.Context, cluster *seaplanev1alpha1.Cluster,
	name, mc string, slot int32) (*seaplanev1alpha1.Placement, error) {
	log := logf.FromContext(ctx)
	log.Info("Creating Placement for Cluster", "cluster", cluster.Name, "mc", mc, "hcpSlot", slot)
	placement := &seaplanev1alpha1.Placement{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  cluster.Namespace,
			Labels:     map[string]string{clusterUIDLabel: string(cluster.UID)},
			Finalizers: []string{hcpSlotFinalizer},
		},
		Spec: seaplanev1alpha1.PlacementSpec{
			ClusterName:       cluster.Name,
			ManagementCluster: mc,
			HCPSlot:           slot,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, placement, r.Scheme); err != nil {
		return nil, fmt.Errorf("set Placement ownerReference: %w", err)
	}
	if err := r.Create(ctx, placement); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create placement: %w", err)
		}
		// A racing attempt placed the Cluster first; give back our slot if it differs.
		var existing seaplanev1alpha1.Placement
		if err := r.Get(ctx, client.ObjectKeyFromObject(placement), &existing); err != nil {
			return nil, fmt.Errorf("get existing placement: %w", err)
		}
		if existing.Spec.ManagementCluster != mc || existing.Spec.HCPSlot != slot {
			if err := r.releaseHCPSlot(ctx, mc, slot, cluster.UID); err != nil {
				return nil, err
			}
		}
		return &existing, nil
	}

	return placement, nil
}

// finalizePlacement releases a deleting Placement's HCP slot and removes its finalizer.
func (r *PlacementReconciler) finalizePlacement(ctx context.Context, placement *seaplanev1alpha1.Placement) error {
	if !controllerutil.ContainsFinalizer(placement, hcpSlotFinalizer) {
		return nil
	}
	if err := r.releaseHCPSlot(ctx, placement.Spec.ManagementCluster, placement.Spec.HCPSlot,
		types.UID(placement.Labels[clusterUIDLabel])); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(placement, hcpSlotFinalizer)
	if err := r.Update(ctx, placement); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("remove placement finalizer: %w", err)
	}
	return nil
}

func (r *PlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.Cluster{}).
		Owns(&seaplanev1alpha1.Placement{}).
		Named("placement").
		Complete(r)
}
