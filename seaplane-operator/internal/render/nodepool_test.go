package render

import (
	"testing"
	"time"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func testNodePool() *seaplanev1alpha1.NodePool {
	return &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster.workers",
			Namespace: "account-123456789012",
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Replicas: ptr.To(int32(3)),
				Management: hypershiftv1beta1.NodePoolManagement{
					AutoRepair:  true,
					UpgradeType: hypershiftv1beta1.UpgradeTypeReplace,
				},
				Release: hypershiftv1beta1.Release{Image: "quay.io/ocp:4.17"},
				Platform: hypershiftv1beta1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						InstanceType:    "m6a.xlarge",
						RootVolume:      &hypershiftv1beta1.Volume{Size: 120, Type: "gp3"},
						InstanceProfile: "worker-profile",
						Subnet: hypershiftv1beta1.AWSResourceReference{
							ID: ptr.To("subnet-1"),
						},
						SecurityGroups: []hypershiftv1beta1.AWSResourceReference{
							{ID: ptr.To("sg-abc")},
							{ID: ptr.To("sg-def")},
						},
					},
				},
			},
		},
	}
}

func TestNodePoolResourceGVR(t *testing.T) {
	r, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}

	if r.Group != "hypershift.openshift.io" {
		t.Errorf("Group = %q, want %q", r.Group, "hypershift.openshift.io")
	}
	if r.Version != "v1beta1" {
		t.Errorf("Version = %q, want %q", r.Version, "v1beta1")
	}
	if r.Resource != "nodepools" {
		t.Errorf("Resource = %q, want %q", r.Resource, "nodepools")
	}
}

func TestNodePoolResourceNaming(t *testing.T) {
	r, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}

	wantName := "workers"
	if r.Name != wantName {
		t.Errorf("Name = %q, want %q", r.Name, wantName)
	}
	wantNS := "cluster-abc12345"
	if r.Namespace != wantNS {
		t.Errorf("Namespace = %q, want %q", r.Namespace, wantNS)
	}
}

func TestNodePoolResourceRejectsMismatchedParentName(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Name = "other-cluster.workers"
	if _, err := NodePoolResource(nodePool, testCluster()); err == nil {
		t.Fatal("expected error for NodePool name whose parent prefix does not match the Cluster")
	}
}

func TestNodePoolResourceObject(t *testing.T) {
	cluster := testCluster()
	r, err := NodePoolResource(testNodePool(), cluster)
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np, ok := r.Object.(*hypershiftv1beta1.NodePool)
	if !ok {
		t.Fatalf("Object is %T, want *NodePool", r.Object)
	}

	if np.Spec.ClusterName != HostedClusterName(cluster) {
		t.Errorf("ClusterName = %q, want %q", np.Spec.ClusterName, HostedClusterName(cluster))
	}
	if np.Spec.Replicas == nil || *np.Spec.Replicas != 3 {
		t.Errorf("Replicas = %v, want 3", np.Spec.Replicas)
	}
	if np.Spec.Platform.Type != hypershiftv1beta1.AWSPlatform {
		t.Errorf("Platform.Type = %q, want AWS", np.Spec.Platform.Type)
	}
	if np.Spec.Platform.AWS == nil {
		t.Fatal("Platform.AWS is nil")
	}
	if np.Spec.Platform.AWS.InstanceType != "m6a.xlarge" {
		t.Errorf("InstanceType = %q, want %q", np.Spec.Platform.AWS.InstanceType, "m6a.xlarge")
	}
	if got := len(np.Spec.Platform.AWS.SecurityGroups); got != 2 {
		t.Errorf("SecurityGroups count = %d, want 2", got)
	}
	if np.Spec.Release.Image != "quay.io/ocp:4.17" {
		t.Errorf("Release.Image = %q, want %q", np.Spec.Release.Image, "quay.io/ocp:4.17")
	}
}

func TestNodePoolResourceUsesReservationDerivedHostedClusterName(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.HostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(&seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{UID: "550e8400-e29b-41d4-a716-446655440000"},
	})
	r, err := NodePoolResource(testNodePool(), cluster)
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := r.Object.(*hypershiftv1beta1.NodePool)
	if want := HostedClusterName(cluster); np.Spec.ClusterName != want {
		t.Errorf("ClusterName = %q, want %q", np.Spec.ClusterName, want)
	}
}

func TestNodePoolResourceDefaults(t *testing.T) {
	minimalNP := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster.workers",
			Namespace: "account-123456789012",
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						InstanceProfile: "worker-profile",
						Subnet:          hypershiftv1beta1.AWSResourceReference{ID: ptr.To("subnet-1")},
						SecurityGroups:  []hypershiftv1beta1.AWSResourceReference{{ID: ptr.To("sg-abc")}},
					},
				},
			},
		},
	}

	r, err := NodePoolResource(minimalNP, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := r.Object.(*hypershiftv1beta1.NodePool)

	tests := []struct {
		name  string
		check func(*testing.T)
	}{
		{"UpgradeType", func(t *testing.T) {
			if np.Spec.Management.UpgradeType != hypershiftv1beta1.UpgradeTypeReplace {
				t.Errorf("got %q, want %q", np.Spec.Management.UpgradeType, hypershiftv1beta1.UpgradeTypeReplace)
			}
		}},
		{"AutoRepair", func(t *testing.T) {
			if !np.Spec.Management.AutoRepair {
				t.Error("got false, want true")
			}
		}},
		{"Replicas", func(t *testing.T) {
			if np.Spec.Replicas == nil || *np.Spec.Replicas != 2 {
				t.Errorf("got %v, want 2", np.Spec.Replicas)
			}
		}},
		{"InstanceType", func(t *testing.T) {
			if np.Spec.Platform.AWS.InstanceType != "t3a.xlarge" {
				t.Errorf("got %q, want %q", np.Spec.Platform.AWS.InstanceType, "t3a.xlarge")
			}
		}},
		{"RootVolume.Size", func(t *testing.T) {
			if np.Spec.Platform.AWS.RootVolume == nil || np.Spec.Platform.AWS.RootVolume.Size != 300 {
				t.Errorf("got %v, want 300", np.Spec.Platform.AWS.RootVolume)
			}
		}},
		{"RootVolume.Type", func(t *testing.T) {
			if np.Spec.Platform.AWS.RootVolume == nil || np.Spec.Platform.AWS.RootVolume.Type != "gp3" {
				t.Errorf("got %v, want gp3", np.Spec.Platform.AWS.RootVolume)
			}
		}},
		{"RootVolume.Encrypted", func(t *testing.T) {
			if np.Spec.Platform.AWS.RootVolume == nil || np.Spec.Platform.AWS.RootVolume.Encrypted == nil || !*np.Spec.Platform.AWS.RootVolume.Encrypted {
				t.Errorf("got %v, want true", np.Spec.Platform.AWS.RootVolume)
			}
		}},
		{"RootVolume.IOPS", func(t *testing.T) {
			// IOPS is service-set (enforced by FieldValidator), so it stays at the Go zero value,
			// matching v1's always-zero behavior for gp2/gp3 volumes.
			if np.Spec.Platform.AWS.RootVolume == nil || np.Spec.Platform.AWS.RootVolume.IOPS != 0 {
				t.Errorf("got %v, want 0", np.Spec.Platform.AWS.RootVolume)
			}
		}},
		{"RootVolume.EncryptionKey", func(t *testing.T) {
			// EncryptionKey is service-set, so it stays empty, matching v1 (AWS's default key is used).
			if np.Spec.Platform.AWS.RootVolume == nil || np.Spec.Platform.AWS.RootVolume.EncryptionKey != "" {
				t.Errorf("got %v, want empty", np.Spec.Platform.AWS.RootVolume)
			}
		}},
		{"Release.Image", func(t *testing.T) {
			if np.Spec.Release.Image != "quay.io/ocp:4.17" {
				t.Errorf("got %q, want the cluster's %q", np.Spec.Release.Image, "quay.io/ocp:4.17")
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.check)
	}
}

func TestNodePoolResourceRootVolumePartialDefaults(t *testing.T) {
	np := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-cluster.workers",
			Namespace: "account-123456789012",
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Platform: hypershiftv1beta1.NodePoolPlatform{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
						InstanceProfile: "worker-profile",
						Subnet:          hypershiftv1beta1.AWSResourceReference{ID: ptr.To("subnet-1")},
						// Only Size is set — every other sub-field is service-set and rejected by
						// FieldValidator before reaching here.
						RootVolume: &hypershiftv1beta1.Volume{Size: 500},
					},
				},
			},
		},
	}

	r, err := NodePoolResource(np, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	rv := r.Object.(*hypershiftv1beta1.NodePool).Spec.Platform.AWS.RootVolume
	if rv == nil {
		t.Fatal("RootVolume is nil, want the supplied volume with defaults filled in")
	}
	if rv.Size != 500 {
		t.Errorf("Size = %d, want 500 (customer-supplied value preserved)", rv.Size)
	}
	if rv.Type != "gp3" {
		t.Errorf("Type = %q, want %q", rv.Type, "gp3")
	}
	if rv.Encrypted == nil || !*rv.Encrypted {
		t.Errorf("Encrypted = %v, want true", rv.Encrypted)
	}
	if rv.IOPS != 0 {
		t.Errorf("IOPS = %d, want 0", rv.IOPS)
	}
	if rv.EncryptionKey != "" {
		t.Errorf("EncryptionKey = %q, want empty", rv.EncryptionKey)
	}
}

func TestNodePoolResourceAutoScalingOmitsDefaultReplicas(t *testing.T) {
	np := testNodePool()
	np.Spec.NodePool.Replicas = nil
	min := int32(1)
	np.Spec.NodePool.AutoScaling = &hypershiftv1beta1.NodePoolAutoScaling{
		Min: &min,
		Max: 3,
	}

	r, err := NodePoolResource(np, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	rendered := r.Object.(*hypershiftv1beta1.NodePool)
	if rendered.Spec.Replicas != nil {
		t.Errorf("Replicas = %v with autoscaling enabled, want nil", *rendered.Spec.Replicas)
	}
	if rendered.Spec.AutoScaling == nil {
		t.Fatal("AutoScaling is nil, want it preserved")
	}
	if rendered.Spec.AutoScaling.Min == nil || *rendered.Spec.AutoScaling.Min != min {
		t.Errorf("AutoScaling.Min = %v, want %d", rendered.Spec.AutoScaling.Min, min)
	}
	if rendered.Spec.AutoScaling.Max != 3 {
		t.Errorf("AutoScaling.Max = %d, want 3", rendered.Spec.AutoScaling.Max)
	}
}

func TestNodePoolResourceLabels(t *testing.T) {
	r, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := r.Object.(*hypershiftv1beta1.NodePool)

	if np.Labels[clusterIDLabel] != "abc12345" {
		t.Errorf("cluster-id label = %q, want %q", np.Labels[clusterIDLabel], "abc12345")
	}
}

func TestNodePoolResourceAutoRepair(t *testing.T) {
	tests := []struct {
		name       string
		autoRepair *bool
		want       bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", ptr.To(true), true},
		{"explicit false", ptr.To(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			np := testNodePool()
			np.Spec.AutoRepair = tt.autoRepair
			r, err := NodePoolResource(np, testCluster())
			if err != nil {
				t.Fatalf("NodePoolResource: %v", err)
			}
			got := r.Object.(*hypershiftv1beta1.NodePool).Spec.Management.AutoRepair
			if got != tt.want {
				t.Errorf("AutoRepair = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodePoolResourceEC2InstanceMetadataHttpTokensAnnotation(t *testing.T) {
	tests := []struct {
		name  string
		value seaplanev1alpha1.EC2InstanceMetadataHttpTokens
		want  string
		set   bool
	}{
		{name: "unset preserves HyperShift default"},
		{
			name:  "required",
			value: seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired,
			want:  "required",
			set:   true,
		},
		{
			name:  "optional",
			value: seaplanev1alpha1.EC2InstanceMetadataHttpTokensOptional,
			want:  "optional",
			set:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodePool := testNodePool()
			nodePool.Spec.EC2InstanceMetadataHttpTokens = tt.value

			resource, err := NodePoolResource(nodePool, testCluster())
			if err != nil {
				t.Fatalf("NodePoolResource: %v", err)
			}
			rendered := resource.Object.(*hypershiftv1beta1.NodePool)
			got, found := rendered.Annotations[ec2InstanceMetadataHttpTokensAnnotation]
			if found != tt.set {
				t.Fatalf("annotation present = %t, want %t (annotations: %v)", found, tt.set, rendered.Annotations)
			}
			if got != tt.want {
				t.Errorf("annotation value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNodePoolResourceNodeLabels(t *testing.T) {
	np := testNodePool()
	np.Spec.Labels = map[string]string{"env": "staging", "team": "platform"}

	r, err := NodePoolResource(np, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	got := r.Object.(*hypershiftv1beta1.NodePool).Spec.NodeLabels

	for k, v := range np.Spec.Labels {
		if got[k] != v {
			t.Errorf("NodeLabels[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(np.Spec.Labels) {
		t.Errorf("NodeLabels len = %d, want %d", len(got), len(np.Spec.Labels))
	}
}

func TestNodePoolResourceNodeLabelsEmpty(t *testing.T) {
	np := testNodePool()
	np.Spec.Labels = nil

	r, err := NodePoolResource(np, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	got := r.Object.(*hypershiftv1beta1.NodePool).Spec.NodeLabels
	if len(got) != 0 {
		t.Errorf("NodeLabels = %v, want empty", got)
	}
}

// Worker EC2 instances are created by the NodePool, so the cluster's customer
// tags have to reach the NodePool spec for day-1 tagging to cover them.
func TestNodePoolInheritsClusterCustomerTags(t *testing.T) {
	cluster := testCluster()
	cluster.Spec.Tags = map[string]string{
		"cost-center": "cc-1234",
		"environment": "production",
	}
	nodePool := testNodePool()
	nodePool.Spec.Tags = map[string]string{
		"environment": "staging",
		"team":        "workers",
	}
	nodePool.Spec.NodePool.Platform.AWS.ResourceTags = []hypershiftv1beta1.AWSResourceTag{
		{Key: "legacy", Value: "ignored"},
	}

	resource, err := NodePoolResource(nodePool, cluster)
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	got := tagPairs(np.Spec.Platform.AWS.ResourceTags)
	if got["cost-center"] != "cc-1234" {
		t.Errorf("cost-center = %q, want %q", got["cost-center"], "cc-1234")
	}
	if got["environment"] != "staging" {
		t.Errorf("environment = %q, want NodePool override %q", got["environment"], "staging")
	}
	if got["team"] != "workers" {
		t.Errorf("team = %q, want %q", got["team"], "workers")
	}
	if len(got) != 4 {
		t.Errorf("resourceTags = %v, want inherited, NodePool, and system tags only", got)
	}
	if _, exists := got["legacy"]; exists {
		t.Errorf("direct resourceTags passthrough should not be rendered: %v", got)
	}
	if got["red-hat-managed"] != "true" {
		t.Errorf("red-hat-managed = %q, want %q", got["red-hat-managed"], "true")
	}
	// NodePoolResource passes an empty cluster ID, so no ownership tag is added.
	if _, ok := got["kubernetes.io/cluster/abc12345"]; ok {
		t.Errorf("resourceTags = %v, want no cluster ownership tag", got)
	}
}

func TestNodePoolWithoutClusterTags(t *testing.T) {
	resource, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	tags := np.Spec.Platform.AWS.ResourceTags
	if len(tags) != 1 || tags[0].Key != "red-hat-managed" {
		t.Errorf("resourceTags = %v, want only the red-hat-managed system tag", tags)
	}
}

func TestNodePoolUpgradeBudgetUnset(t *testing.T) {
	resource, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	// Neither field was set, so the rendered spec must stay on HyperShift's
	// own defaulting rather than pinning a rolling update block of our own.
	if np.Spec.Management.Replace != nil {
		t.Errorf("Replace = %+v, want nil", np.Spec.Management.Replace)
	}
	if np.Spec.Management.InPlace != nil {
		t.Errorf("InPlace = %+v, want nil", np.Spec.Management.InPlace)
	}
}

func TestNodePoolUpgradeBudgetReplace(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	nodePool.Spec.MaxSurge = ptr.To(intstr.FromString("30%"))

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	replace := np.Spec.Management.Replace
	if replace == nil {
		t.Fatal("Replace = nil, want a rolling update configuration")
	}
	if replace.Strategy != hypershiftv1beta1.UpgradeStrategyRollingUpdate {
		t.Errorf("Strategy = %q, want %q", replace.Strategy, hypershiftv1beta1.UpgradeStrategyRollingUpdate)
	}
	if replace.RollingUpdate == nil {
		t.Fatal("RollingUpdate = nil, want maxUnavailable and maxSurge")
	}
	if got := replace.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 2 {
		t.Errorf("MaxUnavailable = %v, want 2", got)
	}
	if got := replace.RollingUpdate.MaxSurge; got == nil || got.String() != "30%" {
		t.Errorf("MaxSurge = %v, want 30%%", got)
	}
	if np.Spec.Management.InPlace != nil {
		t.Errorf("InPlace = %+v, want nil", np.Spec.Management.InPlace)
	}
}

// An unset field must not clobber the counterpart HyperShift already defaulted.
func TestNodePoolUpgradeBudgetPreservesExistingRollingUpdate(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.NodePool.Management.Replace = &hypershiftv1beta1.ReplaceUpgrade{
		Strategy: hypershiftv1beta1.UpgradeStrategyRollingUpdate,
		RollingUpdate: &hypershiftv1beta1.RollingUpdate{
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
		},
	}
	nodePool.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(1))

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	rollingUpdate := np.Spec.Management.Replace.RollingUpdate
	if got := rollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 1 {
		t.Errorf("MaxUnavailable = %v, want 1", got)
	}
	if got := rollingUpdate.MaxSurge; got == nil || got.IntValue() != 1 {
		t.Errorf("MaxSurge = %v, want the pre-existing 1", got)
	}
}

// OnDelete replaces nodes only as they are manually deleted, and upstream
// rejects a rollingUpdate block alongside it.
func TestNodePoolUpgradeBudgetSkippedForOnDelete(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.NodePool.Management.Replace = &hypershiftv1beta1.ReplaceUpgrade{
		Strategy: hypershiftv1beta1.UpgradeStrategyOnDelete,
	}
	nodePool.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	if got := np.Spec.Management.Replace.RollingUpdate; got != nil {
		t.Errorf("RollingUpdate = %+v, want nil", got)
	}
}

func TestNodePoolUpgradeBudgetInPlace(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.NodePool.Management.UpgradeType = hypershiftv1beta1.UpgradeTypeInPlace
	nodePool.Spec.MaxUnavailable = ptr.To(intstr.FromString("50%"))

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	if np.Spec.Management.InPlace == nil {
		t.Fatal("InPlace = nil, want maxUnavailable")
	}
	if got := np.Spec.Management.InPlace.MaxUnavailable; got == nil || got.String() != "50%" {
		t.Errorf("MaxUnavailable = %v, want 50%%", got)
	}
	if np.Spec.Management.Replace != nil {
		t.Errorf("Replace = %+v, want nil", np.Spec.Management.Replace)
	}
}

// The rendered spec is handed to the client, so it must not alias values owned
// by the cached NodePool.
func TestNodePoolUpgradeBudgetDoesNotAliasSource(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	nodePool.Spec.MaxSurge = ptr.To(intstr.FromInt32(3))

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	if np.Spec.Management.Replace.RollingUpdate.MaxUnavailable == nodePool.Spec.MaxUnavailable {
		t.Error("MaxUnavailable aliases the source NodePool")
	}
	if np.Spec.Management.Replace.RollingUpdate.MaxSurge == nodePool.Spec.MaxSurge {
		t.Error("MaxSurge aliases the source NodePool")
	}
}

// nodeVolumeDetachTimeout is a passthrough field, so enabling it in the public
// API is only useful if the value actually reaches the HyperShift NodePool.
func TestNodePoolPassesThroughNodeVolumeDetachTimeout(t *testing.T) {
	nodePool := testNodePool()
	nodePool.Spec.NodePool.NodeVolumeDetachTimeout = &metav1.Duration{Duration: 25 * time.Minute}
	nodePool.Spec.NodePool.NodeDrainTimeout = &metav1.Duration{Duration: 10 * time.Minute}

	resource, err := NodePoolResource(nodePool, testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	if got := np.Spec.NodeVolumeDetachTimeout; got == nil || got.Duration != 25*time.Minute {
		t.Errorf("NodeVolumeDetachTimeout = %v, want 25m", got)
	}
	if got := np.Spec.NodeDrainTimeout; got == nil || got.Duration != 10*time.Minute {
		t.Errorf("NodeDrainTimeout = %v, want 10m", got)
	}
}

func TestNodePoolOmitsUnsetDrainTimeouts(t *testing.T) {
	resource, err := NodePoolResource(testNodePool(), testCluster())
	if err != nil {
		t.Fatalf("NodePoolResource: %v", err)
	}
	np := resource.Object.(*hypershiftv1beta1.NodePool)

	if np.Spec.NodeVolumeDetachTimeout != nil {
		t.Errorf("NodeVolumeDetachTimeout = %v, want nil", np.Spec.NodeVolumeDetachTimeout)
	}
	if np.Spec.NodeDrainTimeout != nil {
		t.Errorf("NodeDrainTimeout = %v, want nil", np.Spec.NodeDrainTimeout)
	}
}
