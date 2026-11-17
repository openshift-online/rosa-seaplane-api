package validation

import (
	"reflect"
	"testing"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	configv1 "github.com/openshift/api/config/v1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
)

func TestPreserveServiceSet(t *testing.T) {
	t.Run("node pool keeps management and accepts replicas", func(t *testing.T) {
		replicas := int32(3)
		stored := v1.NodePoolSpec{AccountID: "123456789012"}
		stored.NodePool.Management = hypershiftv1beta1.NodePoolManagement{UpgradeType: hypershiftv1beta1.UpgradeTypeReplace, AutoRepair: true}
		candidate := *stored.DeepCopy()
		candidate.AccountID = ""
		candidate.NodePool.Management = hypershiftv1beta1.NodePoolManagement{}
		candidate.NodePool.Replicas = &replicas

		if err := NewFieldValidator("NodePool").PreserveServiceSet(&candidate, &stored, featuregate.Default); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(candidate.NodePool.Management, stored.NodePool.Management) || candidate.AccountID != stored.AccountID {
			t.Fatalf("service-set values not restored: %+v", candidate)
		}
		if candidate.NodePool.Replicas == nil || *candidate.NodePool.Replicas != 3 {
			t.Fatal("mutable change lost")
		}
	})

	t.Run("cluster keeps service-set proxy leaves and mutable proxy edits", func(t *testing.T) {
		stored := v1.ClusterSpec{AccountID: "123456789012"}
		stored.HostedCluster.Configuration = &hypershiftv1beta1.ClusterConfiguration{Proxy: &configv1.ProxySpec{HTTPProxy: "http://old", TrustedCA: configv1.ConfigMapNameReference{Name: "platform-ca"}}}
		candidate := *stored.DeepCopy()
		candidate.HostedCluster.Configuration.Proxy.HTTPProxy = "http://new"
		candidate.HostedCluster.Configuration.Proxy.TrustedCA = configv1.ConfigMapNameReference{}

		if err := NewFieldValidator("Cluster").PreserveServiceSet(&candidate, &stored, featuregate.Default); err != nil {
			t.Fatal(err)
		}
		proxy := candidate.HostedCluster.Configuration.Proxy
		if proxy.TrustedCA.Name != "platform-ca" || proxy.HTTPProxy != "http://new" {
			t.Fatalf("proxy = %+v, want trustedCA kept and httpProxy updated", proxy)
		}
	})

	t.Run("no service-set difference leaves candidate untouched", func(t *testing.T) {
		stored := v1.NodePoolSpec{AccountID: "123456789012", Labels: map[string]string{"team": "old"}}
		candidate := *stored.DeepCopy()
		candidate.Labels = map[string]string{"team": "new"}
		if err := NewFieldValidator("NodePool").PreserveServiceSet(&candidate, &stored, featuregate.Default); err != nil || candidate.Labels["team"] != "new" {
			t.Fatalf("candidate changed: %+v %v", candidate, err)
		}
	})
}
