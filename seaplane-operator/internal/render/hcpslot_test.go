package render

import (
	"maps"
	"testing"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func TestHostedClusterPlacedOnHCPSlot(t *testing.T) {
	cluster := testCluster()
	// A passthrough node selector must not survive: the operator owns the field.
	cluster.Spec.HostedCluster.NodeSelector = map[string]string{"customer": "value"}

	resources, err := ClusterResources(cluster, false, "f7a3.0.example.com", "", ECRMirrorConfig{}, 7)
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	hc := hostedClusterFrom(t, resources)

	hcpNamespace := "cluster-abc12345-my-cluster"
	wantSelector := map[string]string{
		"hypershift.openshift.io/control-plane": "true",
		"hypershift.openshift.io/cluster":       hcpNamespace,
		"seaplane.io/hcp-slot":                  "07",
	}
	if !maps.Equal(hc.Spec.NodeSelector, wantSelector) {
		t.Errorf("nodeSelector = %v, want %v", hc.Spec.NodeSelector, wantSelector)
	}
	if got := hc.Labels[HCPSlotLabel]; got != "07" {
		t.Errorf("label %s = %q, want 07", HCPSlotLabel, got)
	}
	if got, want := hc.Annotations[hypershiftv1beta1.AWSLoadBalancerTargetNodesAnnotation], "hypershift.openshift.io/cluster="+hcpNamespace; got != want {
		t.Errorf("annotation %s = %q, want %q", hypershiftv1beta1.AWSLoadBalancerTargetNodesAnnotation, got, want)
	}
	if _, ok := hc.Annotations[hypershiftv1beta1.TopologyAnnotation]; ok {
		t.Errorf("annotation %s must never be set", hypershiftv1beta1.TopologyAnnotation)
	}
}

func TestHostedClusterWithoutHCPSlot(t *testing.T) {
	hc := hostedClusterFrom(t, mustClusterResources(t, 0))
	if len(hc.Spec.NodeSelector) != 0 {
		t.Errorf("nodeSelector = %v, want none", hc.Spec.NodeSelector)
	}
	if _, ok := hc.Labels[HCPSlotLabel]; ok {
		t.Errorf("label %s set without a slot", HCPSlotLabel)
	}
	if _, ok := hc.Annotations[hypershiftv1beta1.AWSLoadBalancerTargetNodesAnnotation]; ok {
		t.Errorf("annotation %s set without a slot", hypershiftv1beta1.AWSLoadBalancerTargetNodesAnnotation)
	}
}

func TestHCPSlotName(t *testing.T) {
	for slot, want := range map[int32]string{1: "01", 7: "07", 42: "42", 99: "99"} {
		if got := HCPSlotName(slot); got != want {
			t.Errorf("HCPSlotName(%d) = %q, want %q", slot, got, want)
		}
	}
}

func TestHostedControlPlaneNamespace(t *testing.T) {
	if got, want := hostedControlPlaneNamespace("cluster-abc", "my.cluster"), "cluster-abc-my-cluster"; got != want {
		t.Errorf("hostedControlPlaneNamespace = %q, want %q", got, want)
	}
}

func mustClusterResources(t *testing.T, hcpSlot int32) []Resource {
	t.Helper()
	resources, err := ClusterResources(testCluster(), false, "f7a3.0.example.com", "", ECRMirrorConfig{}, hcpSlot)
	if err != nil {
		t.Fatalf("ClusterResources: %v", err)
	}
	return resources
}
