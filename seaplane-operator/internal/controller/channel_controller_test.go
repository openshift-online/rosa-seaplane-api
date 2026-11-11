package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/cincinnati"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type channelTestFeed struct {
	releases []v1alpha1.ChannelRelease
	err      error
	calls    int
}

func (f *channelTestFeed) ListReleases(_ context.Context, _ string) ([]v1alpha1.ChannelRelease, error) {
	f.calls++
	return f.releases, f.err
}

type channelCountingClient struct {
	client.Client
	statusWrites int
}

func (c *channelCountingClient) Status() client.SubResourceWriter {
	return &channelCountingStatusWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type channelCountingStatusWriter struct {
	client.SubResourceWriter
	client *channelCountingClient
}

func (w *channelCountingStatusWriter) Update(ctx context.Context, object client.Object, opts ...client.SubResourceUpdateOption) error {
	if err := w.SubResourceWriter.Update(ctx, object, opts...); err != nil {
		return err
	}
	w.client.statusWrites++
	return nil
}

func newChannelTestClient(t *testing.T, channel *v1alpha1.Channel) *channelCountingClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register Channel scheme: %v", err)
	}
	return &channelCountingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Channel{}).
		WithObjects(channel).
		Build()}
}

func loadTestChannel(t *testing.T, c client.Client, namespace, name string) v1alpha1.Channel {
	t.Helper()
	var channel v1alpha1.Channel
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &channel); err != nil {
		t.Fatalf("get Channel: %v", err)
	}
	return channel
}

func TestChannelReconcilerSnapshotsUpdatesAndSkipsUnchangedStatus(t *testing.T) {
	t.Parallel()
	oldDefault := &v1alpha1.ChannelRelease{Version: "4.20.1", Image: "quay.io/release:default"}
	channel := &v1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-4.20", Namespace: v1alpha1.ChannelNamespace, UID: "channel-one", Generation: 3},
		Status:     v1alpha1.ChannelStatus{Default: oldDefault},
	}
	c := newChannelTestClient(t, channel)
	var patch atomic.Int32
	patch.Store(2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		z := patch.Load()
		_, _ = fmt.Fprintf(w, `{"nodes":[{"version":"4.20.%d","payload":"quay.io/release:%d","metadata":{"io.openshift.upgrades.graph.release.channels":"stable-4.20"}}]}`, z, z)
	}))
	defer server.Close()
	feed, err := cincinnati.NewClient(server.URL, "amd64", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	r := &ChannelReconciler{Client: c, Feed: feed, RefreshInterval: 11 * time.Minute}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: channel.Namespace, Name: channel.Name}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil || result.RequeueAfter != 11*time.Minute {
		t.Fatalf("initial reconcile = (%+v, %v), want successful 11m requeue", result, err)
	}
	updated := loadTestChannel(t, c, channel.Namespace, channel.Name)
	if !reflect.DeepEqual(updated.Status.Releases, []v1alpha1.ChannelRelease{{Version: "4.20.2", Image: "quay.io/release:2"}}) || !reflect.DeepEqual(updated.Status.Default, oldDefault) {
		t.Fatalf("initial status = %+v, expected feed releases and unchanged default", updated.Status)
	}
	if updated.Status.ObservedGeneration != 3 {
		t.Errorf("observed generation = %d, want 3", updated.Status.ObservedGeneration)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, "Synced")
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "ReleasesSynced" || condition.ObservedGeneration != 3 {
		t.Errorf("Synced condition = %+v, want current true condition", condition)
	}
	if c.statusWrites != 1 {
		t.Fatalf("initial status writes = %d, want 1", c.statusWrites)
	}

	patch.Store(3)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile new snapshot: %v", err)
	}
	updated = loadTestChannel(t, c, channel.Namespace, channel.Name)
	if !reflect.DeepEqual(updated.Status.Releases, []v1alpha1.ChannelRelease{{Version: "4.20.3", Image: "quay.io/release:3"}}) || c.statusWrites != 2 {
		t.Fatalf("new snapshot = %+v, writes = %d; expected new releases and second write", updated.Status.Releases, c.statusWrites)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile identical snapshot: %v", err)
	}
	if c.statusWrites != 2 {
		t.Errorf("identical reconcile wrote status %d times, want 2", c.statusWrites)
	}
}

func TestChannelReconcilerKeepsSnapshotOnFeedFailure(t *testing.T) {
	t.Parallel()
	oldRelease := v1alpha1.ChannelRelease{Version: "4.20.1", Image: "quay.io/release:1"}
	channel := &v1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-4.20", Namespace: v1alpha1.ChannelNamespace, UID: "channel-two", Generation: 5},
		Status: v1alpha1.ChannelStatus{
			Releases: []v1alpha1.ChannelRelease{oldRelease},
			Default:  &oldRelease,
		},
	}
	c := newChannelTestClient(t, channel)
	feed := &channelTestFeed{err: errors.New("Cincinnati unavailable")}
	r := &ChannelReconciler{Client: c, Feed: feed, RefreshInterval: 15 * time.Minute}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: channel.Namespace, Name: channel.Name}}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil || result.RequeueAfter != time.Minute {
		t.Fatalf("failed feed reconcile = (%+v, %v), want retry after 1m", result, err)
	}
	updated := loadTestChannel(t, c, channel.Namespace, channel.Name)
	if !reflect.DeepEqual(updated.Status.Releases, channel.Status.Releases) || !reflect.DeepEqual(updated.Status.Default, channel.Status.Default) {
		t.Fatalf("fetch failure changed release snapshot or default: %+v", updated.Status)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, "Synced")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "FetchFailed" || condition.ObservedGeneration != 5 {
		t.Errorf("Synced condition = %+v, want false FetchFailed at generation 5", condition)
	}
	if updated.Status.ObservedGeneration != 5 {
		t.Errorf("observed generation = %d, want 5", updated.Status.ObservedGeneration)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile repeated failure: %v", err)
	}
	if c.statusWrites != 1 {
		t.Errorf("repeated failure wrote status %d times, want 1", c.statusWrites)
	}

	feed.err = nil
	feed.releases = []v1alpha1.ChannelRelease{{Version: "4.20.2", Image: "quay.io/release:2"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile recovered feed: %v", err)
	}
	updated = loadTestChannel(t, c, channel.Namespace, channel.Name)
	condition = meta.FindStatusCondition(updated.Status.Conditions, "Synced")
	if condition == nil || condition.Status != metav1.ConditionTrue || !reflect.DeepEqual(updated.Status.Releases, feed.releases) ||
		!reflect.DeepEqual(updated.Status.Default, channel.Status.Default) || c.statusWrites != 2 {
		t.Errorf("recovery status = %+v, writes = %d; expected refreshed releases, original default, true condition", updated.Status, c.statusWrites)
	}
}

func TestChannelReconcilerIgnoresOtherNamespacesAndMissingChannels(t *testing.T) {
	t.Parallel()
	channel := &v1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "stable-4.20", Namespace: "account-123456789012"}}
	c := newChannelTestClient(t, channel)
	feed := &channelTestFeed{}
	r := &ChannelReconciler{Client: c, Feed: feed}
	for _, req := range []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: channel.Namespace, Name: channel.Name}},
		{NamespacedName: types.NamespacedName{Namespace: v1alpha1.ChannelNamespace, Name: "not-created"}},
	} {
		result, err := r.Reconcile(context.Background(), req)
		if err != nil || result.RequeueAfter != 0 {
			t.Fatalf("ignored reconcile = (%+v, %v), want no requeue", result, err)
		}
	}
	if feed.calls != 0 || c.statusWrites != 0 {
		t.Errorf("unrelated reconciliation fetched %d times and wrote %d statuses", feed.calls, c.statusWrites)
	}
}
