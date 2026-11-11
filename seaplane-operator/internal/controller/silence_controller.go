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
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/metrics"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/silence"
)

const silenceAPIRetryDelay = time.Minute

// SilenceReconciler manages Alertmanager silences for cluster lifecycle phases.
// Delete cleanup relies on silence TTL rather than a CR finalizer; Ready-phase
// silences are expired explicitly when IntentForCluster returns nil.
type SilenceReconciler struct {
	client.Client
	SilenceClient           silence.Client
	Metrics                 *metrics.SilenceMetrics
	Clock                   func() time.Time
	MaxConcurrentReconciles int
}

func (r *SilenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if r.SilenceClient == nil {
		return ctrl.Result{}, nil
	}

	var cluster seaplanev1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := time.Now().UTC()
	if r.Clock != nil {
		now = r.Clock().UTC()
	}

	identity := silence.IdentityFromCluster(&cluster)
	intent := silence.IntentForCluster(&cluster)
	if intent == nil && cluster.DeletionTimestamp.IsZero() && cluster.Status.Phase != seaplanev1alpha1.ClusterPhaseReady {
		log.V(1).Info("no silence intent for cluster phase", "phase", cluster.Status.Phase)
	}

	existing, err := r.SilenceClient.List(ctx, identity)
	if err != nil {
		log.Error(nil, "failed to list silences")
		r.recordError()
		return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
	}

	if intent == nil {
		if err := r.expireAll(ctx, existing); err != nil {
			log.Error(nil, "failed to expire silences")
			r.recordError()
			return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
		}
		return ctrl.Result{}, nil
	}

	if len(existing) == 1 && silence.MatchesReason(existing[0], intent.Reason) {
		matched := existing[0]
		// Observatorium requires silence replacement when renewing; can't update in place like pure Alertmanager.
		// Create the replacement first so alerts stay suppressed if expire fails on the old silence.
		if silence.NeedsRenewal(matched, now) {
			if _, err := r.createSilence(ctx, identity, intent.Reason, now); err != nil {
				log.Error(nil, "failed to renew silence")
				r.recordError()
				return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
			}
			if err := r.SilenceClient.Expire(ctx, matched.ID); err != nil {
				log.Error(nil, "failed to expire old silence after renewal")
				r.recordError()
				return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
			}
		}
		return ctrl.Result{RequeueAfter: silence.RequeueInterval}, nil
	}

	if err := r.expireAll(ctx, existing); err != nil {
		log.Error(nil, "failed to expire silences while converging desired state")
		r.recordError()
		return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
	}
	if _, err := r.createSilence(ctx, identity, intent.Reason, now); err != nil {
		log.Error(nil, "failed to create silence")
		r.recordError()
		return ctrl.Result{RequeueAfter: silenceAPIRetryDelay}, nil
	}
	return ctrl.Result{RequeueAfter: silence.RequeueInterval}, nil
}

func (r *SilenceReconciler) recordError() {
	if r.Metrics != nil {
		r.Metrics.ReconcileErrorsTotal.Inc()
	}
}

func (r *SilenceReconciler) createSilence(ctx context.Context, identity silence.ClusterIdentity, reason silence.Reason, now time.Time) (string, error) {
	postable := silence.BuildPostableSilence(identity, reason, now, silence.DefaultTTL)
	return r.SilenceClient.Create(ctx, postable)
}

func (r *SilenceReconciler) expireAll(ctx context.Context, silences []silence.GettableSilence) error {
	for _, s := range silences {
		if err := r.SilenceClient.Expire(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *SilenceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seaplanev1alpha1.Cluster{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("cluster-silence").
		Complete(r)
}

var _ reconcile.Reconciler = &SilenceReconciler{}
