package controller

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestChannelConfigSyncLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "channels.yaml")
	setConfig := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatalf("write channels config: %v", err)
		}
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("register Channel scheme: %v", err)
	}
	previousDefault := &v1alpha1.ChannelRelease{Version: "4.20.1", Image: "quay.io/release:4.20.1"}
	previous := &v1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-4.20", Namespace: v1alpha1.ChannelNamespace, UID: "previous-uid"},
		Spec:       v1alpha1.ChannelSpec{ZOffset: 1},
		Status:     v1alpha1.ChannelStatus{Default: previousDefault},
	}
	unmanaged := &v1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "stable-4.21", Namespace: v1alpha1.ChannelNamespace, UID: "unmanaged-uid"},
	}
	store := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Channel{}).WithObjects(previous, unmanaged).Build()
	syncer := &ChannelConfigSync{Client: store, ConfigPath: path}

	setConfig(`channels:
  - name: stable-4.20
    spec:
      zOffset: 2
      dayOffset: 3
      featureGates: [internal]
      minVersion: 4.20.1
  - name: fast-4.20
    spec:
      zOffset: 0
      dayOffset: 1
`)
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	stable := loadTestChannel(t, store, v1alpha1.ChannelNamespace, "stable-4.20")
	if stable.UID != previous.UID || stable.Labels[channelManagedLabel] != "true" ||
		!reflect.DeepEqual(stable.Status.Default, previousDefault) ||
		!reflect.DeepEqual(stable.Spec, v1alpha1.ChannelSpec{ZOffset: 2, DayOffset: 3, FeatureGates: []string{"internal"}, MinVersion: "4.20.1"}) {
		t.Fatalf("existing Channel not updated in place: %#v", stable)
	}
	fast := loadTestChannel(t, store, v1alpha1.ChannelNamespace, "fast-4.20")
	if fast.Spec.ZOffset != 0 || fast.Spec.DayOffset != 1 || fast.Namespace != v1alpha1.ChannelNamespace || fast.Labels[channelManagedLabel] != "true" {
		t.Fatalf("new Channel spec: %#v", fast)
	}
	version := stable.ResourceVersion
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("idempotent sync: %v", err)
	}
	if got := loadTestChannel(t, store, v1alpha1.ChannelNamespace, "stable-4.20"); got.ResourceVersion != version {
		t.Fatalf("unchanged Channel was rewritten: %s to %s", version, got.ResourceVersion)
	}

	setConfig(`channels:
  - name: stable-4.20
    spec:
      zOffset: 0
      dayOffset: 3
`)
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("updated sync: %v", err)
	}
	stable = loadTestChannel(t, store, v1alpha1.ChannelNamespace, "stable-4.20")
	if stable.Spec.ZOffset != 0 || stable.Spec.DayOffset != 3 || stable.Status.Default.Version != previousDefault.Version {
		t.Fatalf("updated Channel spec/status: %#v", stable)
	}
	var removed v1alpha1.Channel
	if err := store.Get(ctx, types.NamespacedName{Namespace: v1alpha1.ChannelNamespace, Name: "fast-4.20"}, &removed); !apierrors.IsNotFound(err) {
		t.Fatalf("removed Channel should be deleted, got %v", err)
	}

	setConfig("channels:\n  - name: stable-4.20\n    spec:\n      zOffset: -1\n      dayOffset: 0\n")
	if err := syncer.Sync(ctx); err == nil {
		t.Fatal("expected invalid configuration error")
	}
	if got := loadTestChannel(t, store, v1alpha1.ChannelNamespace, "stable-4.20"); !reflect.DeepEqual(got.Spec, stable.Spec) {
		t.Fatalf("invalid config changed Channel: %#v", got)
	}

	setConfig("channels: []\n")
	if err := syncer.Sync(ctx); err != nil {
		t.Fatalf("empty config sync: %v", err)
	}
	if err := store.Get(ctx, types.NamespacedName{Namespace: v1alpha1.ChannelNamespace, Name: "stable-4.20"}, &removed); !apierrors.IsNotFound(err) {
		t.Fatalf("empty config should remove existing Channels, got %v", err)
	}
	if got := loadTestChannel(t, store, v1alpha1.ChannelNamespace, "stable-4.21"); got.UID != unmanaged.UID {
		t.Fatalf("unmanaged Channel was removed: %#v", got)
	}
}

func TestChannelConfigRejectsInvalidDeclarations(t *testing.T) {
	t.Parallel()
	for name, input := range map[string]string{
		"missing list":           "{}",
		"unknown field":          "channels: []\nunknown: true\n",
		"missing offsets":        "channels:\n  - name: stable-4.20\n    spec: {}\n",
		"negative day offset":    "channels:\n  - name: stable-4.20\n    spec: {zOffset: 0, dayOffset: -1}\n",
		"invalid channel name":   "channels:\n  - name: BAD_NAME\n    spec: {zOffset: 0, dayOffset: 0}\n",
		"invalid channel format": "channels:\n  - name: stable\n    spec: {zOffset: 0, dayOffset: 0}\n",
		"duplicate channel":      "channels:\n  - name: stable-4.20\n    spec: {zOffset: 0, dayOffset: 0}\n  - name: stable-4.20\n    spec: {zOffset: 1, dayOffset: 0}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseChannelConfig([]byte(input)); err == nil {
				t.Fatalf("accepted invalid channels configuration: %s", input)
			}
		})
	}
	if parsed, err := parseChannelConfig([]byte("channels: []\n")); err != nil || len(parsed) != 0 {
		t.Fatalf("empty channel list should be allowed: %v, %#v", err, parsed)
	}
}

func TestChannelConfigSyncReloadsProjectedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "channels.yaml")
	writeConfig := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatalf("write channels config: %v", err)
		}
	}
	writeConfig("channels:\n  - name: stable-4.20\n    spec: {zOffset: 1, dayOffset: 0}\n")
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := fake.NewClientBuilder().WithScheme(scheme).Build()
	syncer := &ChannelConfigSync{Client: store, ConfigPath: path, RefreshInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- syncer.Start(ctx) }()

	awaitChannel := func(want bool) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var channel v1alpha1.Channel
			err := store.Get(ctx, types.NamespacedName{Namespace: v1alpha1.ChannelNamespace, Name: "stable-4.20"}, &channel)
			if (want && err == nil) || (!want && apierrors.IsNotFound(err)) {
				return
			}
			if err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("get Channel: %v", err)
			}
			select {
			case <-deadline.C:
				t.Fatalf("timed out waiting for Channel presence=%t", want)
			case <-ticker.C:
			}
		}
	}
	awaitChannel(true)
	writeConfig("channels: []\n")
	awaitChannel(false)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Channel sync stopped with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Channel sync did not stop")
	}
}
