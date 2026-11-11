package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	channelRefreshInterval = 10 * time.Minute
	channelFailureRetry    = time.Minute
)

// ChannelReconciler refreshes the Cincinnati release snapshot of existing Channels.
type ChannelReconciler struct {
	client.Client
	Feed interface {
		ListReleases(ctx context.Context, channel string) ([]v1alpha1.ChannelRelease, error)
	}
	RefreshInterval         time.Duration
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=seaplane.io,resources=channels,verbs=get;list;watch
// +kubebuilder:rbac:groups=seaplane.io,resources=channels/status,verbs=get;update

func (r *ChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != v1alpha1.ChannelNamespace {
		return ctrl.Result{}, nil
	}
	var channel v1alpha1.Channel
	if err := r.Get(ctx, req.NamespacedName, &channel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !channel.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	releases, fetchErr := r.Feed.ListReleases(ctx, channel.Name)
	if err := r.updateChannelStatus(ctx, &channel, releases, fetchErr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update Channel %s status: %w", channel.Name, err)
	}
	if fetchErr != nil {
		return ctrl.Result{RequeueAfter: channelFailureRetry}, nil
	}
	interval := r.RefreshInterval
	if interval <= 0 {
		interval = channelRefreshInterval
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *ChannelReconciler) updateChannelStatus(ctx context.Context, channel *v1alpha1.Channel, releases []v1alpha1.ChannelRelease, fetchErr error) error {
	firstAttempt := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha1.Channel
		if firstAttempt {
			latest = *channel.DeepCopy()
			firstAttempt = false
		} else if err := r.Get(ctx, client.ObjectKeyFromObject(channel), &latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		// A deleted and recreated Channel must never inherit an old instance's fetch.
		if latest.UID != channel.UID || !latest.DeletionTimestamp.IsZero() {
			return nil
		}

		condition := metav1.Condition{
			Type:               "Synced",
			Status:             metav1.ConditionTrue,
			Reason:             "ReleasesSynced",
			Message:            "Cincinnati release snapshot is current",
			ObservedGeneration: latest.Generation,
		}
		releasesChanged := fetchErr == nil && !slices.Equal(latest.Status.Releases, releases)
		if fetchErr != nil {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "FetchFailed"
			condition.Message = fetchErr.Error()
		} else if releasesChanged {
			latest.Status.Releases = releases
		}

		oldCondition := meta.FindStatusCondition(latest.Status.Conditions, "Synced")
		conditionChanged := oldCondition == nil || oldCondition.Status != condition.Status ||
			oldCondition.Reason != condition.Reason || oldCondition.Message != condition.Message ||
			oldCondition.ObservedGeneration != condition.ObservedGeneration
		if !conditionChanged && !releasesChanged && latest.Status.ObservedGeneration == latest.Generation {
			return nil
		}
		meta.SetStatusCondition(&latest.Status.Conditions, condition)
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, &latest)
	})
}

// SetupWithManager watches only Channel objects; Reconcile limits status writes to channels.
func (r *ChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&v1alpha1.Channel{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("channel").
		Complete(r)
}
