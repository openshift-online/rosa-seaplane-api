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
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
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
	"sigs.k8s.io/controller-runtime/pkg/source"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
)

const (
	nodePoolFinalizer = "seaplane.io/nodepool"
)

var errNodePoolClusterOwnerMissing = errors.New("no Cluster controller ownerReference")

func (r *NodePoolReconciler) parentCluster(ctx context.Context, nodePool *seaplanev1alpha1.NodePool) (*seaplanev1alpha1.Cluster, error) {
	owner := metav1.GetControllerOf(nodePool)
	if !isClusterControllerOwner(owner) {
		return nil, fmt.Errorf("%w: NodePool %s/%s", errNodePoolClusterOwnerMissing, nodePool.Namespace, nodePool.Name)
	}
	var cluster seaplanev1alpha1.Cluster
	if err := r.Get(ctx, types.NamespacedName{Namespace: nodePool.Namespace, Name: owner.Name}, &cluster); err != nil {
		return nil, err
	}
	if cluster.UID != owner.UID {
		return nil, apierrors.NewNotFound(seaplanev1alpha1.GroupVersion.WithResource("clusters").GroupResource(), owner.Name)
	}
	return &cluster, nil
}

func isClusterControllerOwner(owner *metav1.OwnerReference) bool {
	return owner != nil && owner.Kind == reflect.TypeOf(seaplanev1alpha1.Cluster{}).Name() && owner.UID != ""
}

// NodePoolReconciler reconciles a NodePool object by creating DynamoDB desires
// that kube-applier-aws applies to the management cluster.
type NodePoolReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Dynamo                  dynamo.DesireClient
	StatusEvents            chan event.GenericEvent
	EventRouter             *EventRouter
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=seaplane.io,resources=nodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=seaplane.io,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=seaplane.io,resources=nodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=seaplane.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=seaplane.io,resources=placements,verbs=get;list;watch

func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var nodePool seaplanev1alpha1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &nodePool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !nodePool.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &nodePool)
	}

	if !controllerutil.ContainsFinalizer(&nodePool, nodePoolFinalizer) {
		controllerutil.AddFinalizer(&nodePool, nodePoolFinalizer)
		if err := r.Update(ctx, &nodePool); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// The finalizer update emits a watch event that re-enqueues this object.
		return ctrl.Result{}, nil
	}

	cluster, err := r.parentCluster(ctx, &nodePool)
	if err != nil {
		if apierrors.IsNotFound(err) {
			owner := metav1.GetControllerOf(&nodePool)
			log.Info("Waiting for the referenced Cluster", "cluster_name", owner.Name, "cluster_uid", owner.UID)
			r.setPhase(ctx, &nodePool, seaplanev1alpha1.NodePoolPhaseWaitingForCluster)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get parent Cluster: %w", err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		log.Info("Parent Cluster is deleting", "cluster", cluster.Name)
		r.setPhase(ctx, &nodePool, seaplanev1alpha1.NodePoolPhaseWaitingForCluster)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Cluster must have a Placement before we can target an MC.
	if cluster.Status.PlacementRef == nil {
		log.Info("Cluster has no Placement yet", "cluster", cluster.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	mc := cluster.Status.PlacementRef.ManagementCluster
	specsPrefix := dynamo.SpecsPrefix(mc)
	statusPrefix := dynamo.StatusPrefix(mc)

	// Generate NodePool resource and create ApplyDesire.
	m, err := render.NodePoolResource(&nodePool, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render nodepool resource: %w", err)
	}
	renderedNodePool, ok := m.Object.(*hypershiftv1beta1.NodePool)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("rendered NodePool has unexpected type %T", m.Object)
	}
	if err := r.upsertRenderedNodePool(ctx, renderedNodePool); err != nil {
		return ctrl.Result{}, err
	}
	ns := m.Namespace

	docID := dynamo.NewDocumentID(taskKey, m.Group, m.Version, m.Resource, ns, m.Name)
	readDocID := dynamo.NewDocumentID(taskKey+"-read", m.Group, m.Version, m.Resource, ns, m.Name)

	// Upsert ApplyDesire — no-op when content matches, no generation gate needed.
	content, err := json.Marshal(m.Object)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal nodepool resource: %w", err)
	}

	desire := &dynamo.ApplyDesire{
		DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: docID},
		Spec: dynamo.ApplyDesireSpec{
			Type:              dynamo.ApplyDesireTypeServerSideApply,
			ManagementCluster: mc,
			ClusterID:         string(cluster.UID),
			TargetItem: dynamo.ResourceReference{
				Group:     m.Group,
				Version:   m.Version,
				Resource:  m.Resource,
				Namespace: ns,
				Name:      m.Name,
			},
			ServerSideApply: &dynamo.ServerSideApplyConfig{
				KubeContent: &runtime.RawExtension{Raw: content},
			},
		},
	}
	readDesire := &dynamo.ReadDesire{
		DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: readDocID},
		Spec: dynamo.ReadDesireSpec{
			ManagementCluster: mc,
			ClusterID:         string(cluster.UID),
			TargetItem: dynamo.ResourceReference{
				Group:     m.Group,
				Version:   m.Version,
				Resource:  m.Resource,
				Namespace: ns,
				Name:      m.Name,
			},
		},
	}

	var upsertResult dynamo.UpsertResult
	var applyErr, readUpsertErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		upsertResult, applyErr = r.Dynamo.UpsertApplyDesire(ctx, specsPrefix, desire)
	}()
	go func() {
		defer wg.Done()
		_, readUpsertErr = r.Dynamo.UpsertReadDesire(ctx, specsPrefix, readDesire)
	}()
	wg.Wait()
	if applyErr != nil {
		return ctrl.Result{}, fmt.Errorf("upsert apply desire: %w", applyErr)
	}
	if readUpsertErr != nil {
		return ctrl.Result{}, fmt.Errorf("upsert read desire: %w", readUpsertErr)
	}
	applyEntry := DesireStatusEntry{DocID: docID, Resource: m.Resource, Name: m.Name, DesireUpdateTime: upsertResult.UpdateTime}

	if r.EventRouter != nil {
		r.EventRouter.Register(docID, EventTarget{Channel: r.StatusEvents, Key: req.NamespacedName})
		r.EventRouter.Register(readDocID, EventTarget{Channel: r.StatusEvents, Key: req.NamespacedName})
	}

	// Read status feedback from DynamoDB.
	r.updateStatusFromDynamo(ctx, &nodePool, statusPrefix, applyEntry, readDocID)

	// Re-read to see phase set by updateStatusFromDynamo.
	if err := r.Get(ctx, client.ObjectKeyFromObject(&nodePool), &nodePool); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-read nodepool after status update: %w", client.IgnoreNotFound(err))
	}
	if nodePool.Status.Phase == "" || nodePool.Status.Phase == seaplanev1alpha1.NodePoolPhaseWaitingForCluster {
		r.setPhase(ctx, &nodePool, seaplanev1alpha1.NodePoolPhaseProvisioning)
	}

	return ctrl.Result{RequeueAfter: statusRefreshDelay}, nil
}

func (r *NodePoolReconciler) reconcileDelete(ctx context.Context, nodePool *seaplanev1alpha1.NodePool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(nodePool, nodePoolFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("NodePool deleting", "nodePool", nodePool.Name)
	r.setPhase(ctx, nodePool, seaplanev1alpha1.NodePoolPhaseDeleting)

	cluster, err := r.parentCluster(ctx, nodePool)
	if err != nil {
		if !apierrors.IsNotFound(err) && !errors.Is(err, errNodePoolClusterOwnerMissing) {
			return ctrl.Result{}, fmt.Errorf("get parent Cluster for NodePool deletion: %w", err)
		}
		log.Info("Parent Cluster not resolvable; skipping management-cluster cleanup", "nodePool", nodePool.Name, "reason", err.Error())
		cluster = nil
	}
	if cluster != nil && cluster.Status.PlacementRef != nil {
		mc := cluster.Status.PlacementRef.ManagementCluster
		specsPrefix := dynamo.SpecsPrefix(mc)
		statusPrefix := dynamo.StatusPrefix(mc)

		m, err := render.NodePoolResource(nodePool, cluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("render nodepool resource for deletion: %w", err)
		}
		ns := m.Namespace

		// Switch the ApplyDesire to Type=Delete in-place, using the same
		// documentID as the original SSA desire. kube-applier sees a MODIFY
		// stream event and deletes the resource from the MC instead of
		// re-applying it.
		docID := dynamo.NewDocumentID(taskKey, m.Group, m.Version, m.Resource, ns, m.Name)
		deleteDesire := &dynamo.ApplyDesire{
			DynamoDBMetadata: dynamo.DynamoDBMetadata{DocumentID: docID},
			Spec: dynamo.ApplyDesireSpec{
				Type:              dynamo.ApplyDesireTypeDelete,
				ManagementCluster: mc,
				ClusterID:         string(cluster.UID),
				TargetItem: dynamo.ResourceReference{
					Group:     m.Group,
					Version:   m.Version,
					Resource:  m.Resource,
					Namespace: ns,
					Name:      m.Name,
				},
			},
		}
		ur, err := r.Dynamo.UpsertApplyDesire(ctx, specsPrefix, deleteDesire)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("upsert delete desire: %w", err)
		}

		deleteEntry := DesireStatusEntry{DocID: docID, Resource: m.Resource, Name: m.Name, DesireUpdateTime: ur.UpdateTime}
		deleteSynced := CheckApplyDesireStatuses(ctx, r.Dynamo, statusPrefix, []DesireStatusEntry{deleteEntry}, nodePool.Generation)
		r.setSyncedCondition(ctx, nodePool, deleteSynced)

		if deleteSynced.Status != metav1.ConditionTrue {
			log.Info("Waiting for NodePool to be deleted on management cluster", "nodePool", nodePool.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Resource confirmed deleted — remove the ApplyDesire and ReadDesire
		// specs from DynamoDB so kube-applier stops tracking them.
		var desireCleanupErr error
		if err := r.Dynamo.DeleteDesireSpec(ctx, specsPrefix, "-applydesires", docID); err != nil {
			log.Error(err, "Failed to clean up ApplyDesire spec", "nodepool", m.Name)
			desireCleanupErr = fmt.Errorf("delete ApplyDesire spec for NodePool %s: %w", m.Name, err)
		}
		readDocID := dynamo.NewDocumentID(taskKey+"-read", m.Group, m.Version, m.Resource, ns, m.Name)
		if err := r.Dynamo.DeleteDesireSpec(ctx, specsPrefix, "-readdesires", readDocID); err != nil {
			log.Error(err, "Failed to clean up ReadDesire spec", "nodepool", m.Name)
			if desireCleanupErr == nil {
				desireCleanupErr = fmt.Errorf("delete ReadDesire spec for NodePool %s: %w", m.Name, err)
			}
		}
		if desireCleanupErr != nil {
			return ctrl.Result{}, desireCleanupErr
		}
	}

	if clusterUID, renderedName, ok := renderedNodePoolIdentity(nodePool); ok {
		if err := r.deleteRenderedNodePool(ctx, clusterUID, renderedName); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove finalizer with RetryOnConflict to handle stale resourceVersion.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodePool), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !controllerutil.ContainsFinalizer(&latest, nodePoolFinalizer) {
			return nil
		}
		controllerutil.RemoveFinalizer(&latest, nodePoolFinalizer)
		return r.Update(ctx, &latest)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *NodePoolReconciler) updateStatusFromDynamo(ctx context.Context, nodePool *seaplanev1alpha1.NodePool, statusPrefix string, applyEntry DesireStatusEntry, readDocID string) {
	log := logf.FromContext(ctx)

	var syncedCond metav1.Condition
	var readStatus *dynamo.ReadDesireStatus
	var readErr error
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		syncedCond = CheckApplyDesireStatuses(ctx, r.Dynamo, statusPrefix, []DesireStatusEntry{applyEntry}, nodePool.Generation)
	}()
	go func() {
		defer wg.Done()
		readStatus, readErr = r.Dynamo.GetReadDesireStatus(ctx, statusPrefix, readDocID)
	}()
	wg.Wait()

	if readErr != nil {
		log.V(1).Info("ReadDesire status not yet available", "error", readErr)
	}

	var npConditions []metav1.Condition
	var npReplicas *int32
	var hasObservedStatus bool
	if readStatus != nil && readStatus.KubeContent != nil {
		var np struct {
			Status struct {
				Conditions []metav1.Condition `json:"conditions"`
				Replicas   *int32             `json:"replicas"`
			} `json:"status"`
		}
		if err := json.Unmarshal(readStatus.KubeContent.Raw, &np); err != nil {
			log.Error(err, "Failed to unmarshal NodePool status")
		} else {
			npConditions = np.Status.Conditions
			npReplicas = np.Status.Replicas
			hasObservedStatus = true
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodePool), &latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		meta.SetStatusCondition(&latest.Status.Conditions, syncedCond)
		for _, cond := range npConditions {
			if cond.Type == "Ready" {
				meta.SetStatusCondition(&latest.Status.Conditions, cond)
			}
		}
		if hasObservedStatus {
			latest.Status.Replicas = npReplicas
		}
		if meta.IsStatusConditionTrue(latest.Status.Conditions, "Ready") {
			latest.Status.Phase = seaplanev1alpha1.NodePoolPhaseReady
		}
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	}); err != nil {
		log.Error(err, "Failed to update nodepool status from DynamoDB feedback")
	}
}

func (r *NodePoolReconciler) setPhase(ctx context.Context, nodePool *seaplanev1alpha1.NodePool, phase seaplanev1alpha1.NodePoolPhase) {
	if nodePool.Status.Phase == phase {
		return
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodePool), &latest); err != nil {
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
		logf.FromContext(ctx).Error(err, "Failed to update nodepool phase", "phase", phase)
	}
}

func (r *NodePoolReconciler) setSyncedCondition(ctx context.Context, nodePool *seaplanev1alpha1.NodePool, cond metav1.Condition) {
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest seaplanev1alpha1.NodePool
		if err := r.Get(ctx, client.ObjectKeyFromObject(nodePool), &latest); err != nil {
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

func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&seaplanev1alpha1.NodePool{}).
		WatchesRawSource(source.Channel(r.StatusEvents, &handler.EnqueueRequestForObject{})).
		Named("nodepool").
		Complete(r)
}
