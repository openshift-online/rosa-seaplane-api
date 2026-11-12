package controller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"time"

	v1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ChannelConfigSync keeps region-local Channel specs in sync with the projected ConfigMap.
// Each replica may run it; database writes are idempotent and use optimistic concurrency.
type ChannelConfigSync struct {
	client.Client
	ConfigPath      string
	RefreshInterval time.Duration
}

const channelManagedLabel = "seaplane.io/channel-config-managed"

var channelNamePattern = regexp.MustCompile(`^[a-z0-9-]+-[0-9]+\.[0-9]+$`)

type channelConfig struct {
	Channels []struct {
		Name string `json:"name"`
		Spec struct {
			FeatureGates []string `json:"featureGates,omitempty"`
			MinVersion   string   `json:"minVersion,omitempty"`
			ZOffset      *int32   `json:"zOffset"`
			DayOffset    *int32   `json:"dayOffset"`
		} `json:"spec"`
	} `json:"channels"`
}

type configuredChannel struct {
	name string
	spec v1alpha1.ChannelSpec
}

func parseChannelConfig(data []byte) ([]configuredChannel, error) {
	var config channelConfig
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		return nil, fmt.Errorf("decode channels configuration: %w", err)
	}
	if config.Channels == nil {
		return nil, fmt.Errorf("channels list is required (use [] for no channels)")
	}
	channels := make([]configuredChannel, 0, len(config.Channels))
	seen := make(map[string]bool, len(config.Channels))
	for i, entry := range config.Channels {
		if len(validation.IsDNS1123Subdomain(entry.Name)) != 0 || !channelNamePattern.MatchString(entry.Name) {
			return nil, fmt.Errorf("channels[%d] has an invalid name", i)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("channels[%d] duplicates a channel name", i)
		}
		if entry.Spec.ZOffset == nil || entry.Spec.DayOffset == nil || *entry.Spec.ZOffset < 0 || *entry.Spec.DayOffset < 0 {
			return nil, fmt.Errorf("channels[%d] requires non-negative zOffset and dayOffset", i)
		}
		seen[entry.Name] = true
		channels = append(channels, configuredChannel{
			name: entry.Name,
			spec: v1alpha1.ChannelSpec{
				FeatureGates: entry.Spec.FeatureGates,
				MinVersion:   entry.Spec.MinVersion,
				ZOffset:      *entry.Spec.ZOffset,
				DayOffset:    *entry.Spec.DayOffset,
			},
		})
	}
	return channels, nil
}

// Sync reads and validates the entire configuration before changing any database rows.
func (s *ChannelConfigSync) Sync(ctx context.Context) error {
	data, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return fmt.Errorf("read channels configuration: %w", err)
	}
	desired, err := parseChannelConfig(data)
	if err != nil {
		return err
	}
	var rows v1alpha1.ChannelList
	if err := s.List(ctx, &rows, client.InNamespace(v1alpha1.ChannelNamespace)); err != nil {
		return fmt.Errorf("list Channels: %w", err)
	}
	existing := make(map[string]*v1alpha1.Channel, len(rows.Items))
	for i := range rows.Items {
		existing[rows.Items[i].Name] = &rows.Items[i]
	}
	for _, entry := range desired {
		if row, found := existing[entry.name]; found {
			delete(existing, entry.name)
			if err := s.update(ctx, row, entry.spec); err != nil {
				return fmt.Errorf("update Channel %s: %w", entry.name, err)
			}
			continue
		}
		row := &v1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: v1alpha1.ChannelNamespace,
				Name:      entry.name,
				Labels:    map[string]string{channelManagedLabel: "true"},
			},
			Spec: entry.spec,
		}
		if err := s.Create(ctx, row); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create Channel %s: %w", entry.name, err)
			}
			if err := s.Get(ctx, client.ObjectKeyFromObject(row), row); err != nil {
				return fmt.Errorf("get Channel %s after concurrent create: %w", entry.name, err)
			}
			if err := s.update(ctx, row, entry.spec); err != nil {
				return fmt.Errorf("update Channel %s after concurrent create: %w", entry.name, err)
			}
		}
	}
	// Never sweep pre-existing, unmanaged Channels on an upgrade with no configured channels.
	for name, row := range existing {
		if row.Labels[channelManagedLabel] != "true" {
			continue
		}
		if err := s.delete(ctx, row); err != nil {
			return fmt.Errorf("delete unconfigured Channel %s: %w", name, err)
		}
	}
	return nil
}

func (s *ChannelConfigSync) update(ctx context.Context, row *v1alpha1.Channel, desired v1alpha1.ChannelSpec) error {
	current := row.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !current.DeletionTimestamp.IsZero() {
			return nil
		}
		if reflect.DeepEqual(current.Spec, desired) && current.Labels[channelManagedLabel] == "true" {
			return nil
		}
		if current.Labels == nil {
			current.Labels = make(map[string]string, 1)
		}
		current.Labels[channelManagedLabel] = "true"
		current.Spec = desired
		if err := s.Update(ctx, current); err != nil {
			if apierrors.IsConflict(err) {
				return s.refreshAfterConflict(ctx, row, current, err)
			}
			return err
		}
		return nil
	})
}

func (s *ChannelConfigSync) delete(ctx context.Context, row *v1alpha1.Channel) error {
	current := row.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !current.DeletionTimestamp.IsZero() || current.Labels[channelManagedLabel] != "true" {
			return nil
		}
		if err := s.Delete(ctx, current); apierrors.IsConflict(err) {
			return s.refreshAfterConflict(ctx, row, current, err)
		} else {
			return client.IgnoreNotFound(err)
		}
	})
}

func (s *ChannelConfigSync) refreshAfterConflict(ctx context.Context, original, current *v1alpha1.Channel, conflict error) error {
	if err := s.Get(ctx, types.NamespacedName{Namespace: original.Namespace, Name: original.Name}, current); err != nil {
		return err
	}
	if current.UID != original.UID {
		return nil // A replaced Channel must not inherit an update or deletion for the old UID.
	}
	return conflict
}

// Start periodically reloads the projected file; invalid updates leave existing rows intact.
func (s *ChannelConfigSync) Start(ctx context.Context) error {
	interval := s.RefreshInterval
	if interval <= 0 {
		interval = channelRefreshInterval
	}
	logger := ctrl.Log.WithName("channel-config")
	sync := func() {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			logger.Error(err, "Failed to synchronize Channels")
		}
	}
	sync()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			sync()
		}
	}
}
