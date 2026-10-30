package seaplanedb

import (
	"encoding/json"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion"
	v1alpha1conv "github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/conversion/v1alpha1"
)

const (
	testClusterID    = "550e8400-e29b-41d4-a716-446655440000"
	testAccountID    = "account-123"
	testClusterName  = "test-cluster"
	testNodePoolName = "test-cluster.test-nodepool"
)

// --- Cluster conversion tests ---

func TestPublicToInternalCluster_SetsMetadata(t *testing.T) {
	pub := &public.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName,
		},
		Spec: public.ClusterSpec{DNSReservationID: "reservation-uid"},
	}

	result := PublicToInternalCluster(pub, testAccountID)

	require.NotNil(t, result)
	assert.Equal(t, testClusterName, result.Name)
	assert.Equal(t, accountNamespace(testAccountID), result.Namespace)
	assert.Empty(t, result.UID)
	assert.Equal(t, map[string]string{accountIDLabel: testAccountID}, result.Labels)
}

func TestPublicToInternalCluster_InjectsServiceSetFields(t *testing.T) {
	pub := &public.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
		Spec:       public.ClusterSpec{DNSReservationID: "reservation-uid"},
	}

	result := PublicToInternalCluster(pub, testAccountID)

	require.NotNil(t, result)
	assert.Equal(t, testAccountID, result.Spec.AccountID)
	assert.Empty(t, result.Spec.HostedClusterName)
}

func TestUnprojectCluster_PreservesCustomDNSAndEnrichesServiceSetFields(t *testing.T) {
	baseDomainPrefix := "my-cluster"
	pub := &public.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
		Spec: public.ClusterSpec{
			DNSReservationID: "reservation-uid",
			HostedCluster: public.HostedClusterSpecPassthrough{
				DNS: hypershiftv1beta1.DNSSpec{
					BaseDomain:       "customer.example",
					BaseDomainPrefix: &baseDomainPrefix,
					PrivateZoneID:    "ZCUSTOMERPRIVATE",
					PublicZoneID:     "ZCUSTOMERPUBLIC",
				},
			},
		},
	}

	enrichment := &conversion.ServiceSetFields{
		AccountID:         testAccountID,
		HostedClusterName: "hc-550e8400e29b41d",
		HostedCluster: &conversion.ServiceSetFieldsHostedCluster{
			DNS: &conversion.ServiceSetFieldsDNS{
				BaseDomain:    "example.com",
				PrivateZoneID: "Z1234PRIVATE",
				PublicZoneID:  "Z5678PUBLIC",
			},
			KubeAPIServerDNSName: "api.my-cluster.example.com",
		},
	}

	result := v1alpha1conv.UnprojectCluster(&pub.Spec, enrichment)

	require.NotNil(t, result)
	// The customer-selectable prefix survives while platform-managed fields win.
	assert.Equal(t, "example.com", result.HostedCluster.DNS.BaseDomain)
	assert.Equal(t, &baseDomainPrefix, result.HostedCluster.DNS.BaseDomainPrefix)
	assert.Equal(t, "Z1234PRIVATE", result.HostedCluster.DNS.PrivateZoneID)
	assert.Equal(t, "Z5678PUBLIC", result.HostedCluster.DNS.PublicZoneID)
	assert.Equal(t, "api.my-cluster.example.com", result.HostedCluster.KubeAPIServerDNSName)
	// Verify other service-set fields still work
	assert.Equal(t, testAccountID, result.AccountID)
	assert.Equal(t, "hc-550e8400e29b41d", result.HostedClusterName)
}

func TestPublicToInternalCluster_NilInput(t *testing.T) {
	result := PublicToInternalCluster(nil, testAccountID)
	assert.Nil(t, result)
}

func TestInternalToPublicCluster_FiltersServiceSetFields(t *testing.T) {
	cr := &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: accountNamespace(testAccountID),
			UID:       types.UID(testClusterID),
			Labels:    map[string]string{accountIDLabel: testAccountID},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			AccountID:         testAccountID,
			HostedClusterName: "hc-550e8400e29b41d",
			DNSReservationID:  "reservation-uid",
		},
	}

	result := InternalToPublicCluster(cr)

	require.NotNil(t, result)
	assert.Equal(t, testClusterName, result.Name)
	assert.Equal(t, types.UID(testClusterID), result.UID)
	assert.Equal(t, "reservation-uid", result.Spec.DNSReservationID)
	// Service-set fields absent from public type (filtered by JSON roundtrip)
}

// Customer tags are annotations end to end; labels stay system-only.
func TestCustomerTagsAreAnnotations(t *testing.T) {
	pub := &public.Cluster{ObjectMeta: metav1.ObjectMeta{Name: testClusterName,
		Labels: map[string]string{
			"team": "dropped", accountIDLabel: "forged",
		},
		Annotations: map[string]string{"team": "blue", "seaplane.io/forged": "dropped"},
	}}

	cr := PublicToInternalCluster(pub, testAccountID)
	assert.Equal(t, map[string]string{accountIDLabel: testAccountID}, cr.Labels)
	assert.Equal(t, map[string]string{"team": "blue"}, cr.Annotations)
	assert.Equal(t, cr.ObjectMeta, InternalToPublicCluster(cr).ObjectMeta)
}

func TestInternalToPublicCluster_NilInput(t *testing.T) {
	result := InternalToPublicCluster(nil)
	assert.Nil(t, result)
}

func TestInternalToPublicCluster_ProjectsProxy(t *testing.T) {
	tests := []struct {
		name   string
		config *hypershiftv1beta1.ClusterConfiguration
		want   *public.ClusterProxy
	}{
		{name: "no configuration"},
		{name: "no proxy", config: &hypershiftv1beta1.ClusterConfiguration{}},
		{
			name: "full proxy",
			config: &hypershiftv1beta1.ClusterConfiguration{
				Proxy: &configv1.ProxySpec{
					HTTPProxy:  "http://proxy.example.com:8080",
					HTTPSProxy: "https://proxy.example.com:8443",
					NoProxy:    "localhost,127.0.0.1,.example.com",
					TrustedCA:  configv1.ConfigMapNameReference{Name: "private-ca"},
				},
			},
			want: &public.ClusterProxy{
				HTTPProxy:  "http://proxy.example.com:8080",
				HTTPSProxy: "https://proxy.example.com:8443",
				NoProxy:    "localhost,127.0.0.1,.example.com",
			},
		},
		{
			name:   "no proxy exclusions only",
			config: &hypershiftv1beta1.ClusterConfiguration{Proxy: &configv1.ProxySpec{NoProxy: "localhost"}},
			want:   &public.ClusterProxy{NoProxy: "localhost"},
		},
		{
			name:   "empty proxy",
			config: &hypershiftv1beta1.ClusterConfiguration{Proxy: &configv1.ProxySpec{}},
			want:   &public.ClusterProxy{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &seaplanev1alpha1.Cluster{}
			cr.Spec.HostedCluster.Configuration = tt.config
			before := cr.DeepCopy()
			got := InternalToPublicCluster(cr)
			assert.Equal(t, tt.want, got.Proxy)
			assert.Equal(t, before, cr, "projection must not modify the stored cluster")

			data, err := json.Marshal(got)
			require.NoError(t, err)
			var response map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &response))
			if tt.want == nil {
				assert.NotContains(t, response, "proxy")
				return
			}
			wantProxy := map[string]string{}
			if tt.want.HTTPProxy != "" {
				wantProxy["http_proxy"] = tt.want.HTTPProxy
			}
			if tt.want.HTTPSProxy != "" {
				wantProxy["https_proxy"] = tt.want.HTTPSProxy
			}
			if tt.want.NoProxy != "" {
				wantProxy["no_proxy"] = tt.want.NoProxy
			}
			wantJSON, err := json.Marshal(wantProxy)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), string(response["proxy"]))
			assert.NotContains(t, string(data), "private-ca", "hidden fields must remain filtered")
			require.NotNil(t, got.Spec.HostedCluster.Configuration.Proxy)
			assert.Equal(t, tt.want.HTTPProxy, got.Spec.HostedCluster.Configuration.Proxy.HTTPProxy)
			assert.Equal(t, tt.want.HTTPSProxy, got.Spec.HostedCluster.Configuration.Proxy.HTTPSProxy)
			assert.Equal(t, tt.want.NoProxy, got.Spec.HostedCluster.Configuration.Proxy.NoProxy)

			// The response alias is independent of the canonical configuration.
			got.Proxy.HTTPProxy = "http://other.example.com"
			assert.Equal(t, tt.want.HTTPProxy, got.Spec.HostedCluster.Configuration.Proxy.HTTPProxy)
			copy := got.DeepCopy()
			copy.Proxy.NoProxy = "different.example.com"
			assert.Equal(t, tt.want.NoProxy, got.Proxy.NoProxy)
		})
	}
}

func TestPublicToInternalCluster_IgnoresProxyProjection(t *testing.T) {
	pub := &public.Cluster{
		Proxy: &public.ClusterProxy{HTTPProxy: "http://response-only.example.com"},
		Spec: public.ClusterSpec{
			HostedCluster: public.HostedClusterSpecPassthrough{
				Configuration: &public.ClusterConfiguration{
					Proxy: &public.ProxyConfiguration{HTTPProxy: "http://canonical.example.com"},
				},
			},
		},
	}
	cr := PublicToInternalCluster(pub, testAccountID)
	assert.Equal(t, "http://canonical.example.com", cr.Spec.HostedCluster.Configuration.Proxy.HTTPProxy)
	assert.Equal(t, "http://canonical.example.com", InternalToPublicCluster(cr).Proxy.HTTPProxy)
}

func TestClusterRoundTrip(t *testing.T) {
	original := &public.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName},
		Spec:       public.ClusterSpec{DNSReservationID: "reservation-uid"},
	}

	internal := PublicToInternalCluster(original, testAccountID)
	require.NotNil(t, internal)

	result := InternalToPublicCluster(internal)
	require.NotNil(t, result)

	assert.Equal(t, original.Spec.DNSReservationID, result.Spec.DNSReservationID)
	assert.Equal(t, original.Name, result.Name)
}

// --- NodePool conversion tests ---

func TestPublicToInternalNodePool_SetsMetadata(t *testing.T) {
	pub := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: testNodePoolName},
	}

	// NodePool internalPoolID is tied to its name (cr.Name used as both ID and Name)
	result := PublicToInternalNodePool(pub, testAccountID, testNodePoolName)

	require.NotNil(t, result)
	assert.Equal(t, testNodePoolName, result.Name)
	assert.Equal(t, accountNamespace(testAccountID), result.Namespace)
	assert.Equal(t, testAccountID, result.Labels[accountIDLabel])
}

func TestPublicToInternalNodePool_InjectsServiceSetFields(t *testing.T) {
	pub := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: testNodePoolName},
	}

	result := PublicToInternalNodePool(pub, testAccountID, testNodePoolName)

	require.NotNil(t, result)
	assert.Equal(t, testAccountID, result.Spec.AccountID)
	assert.Equal(t, testNodePoolName, result.Spec.InternalPoolID)
}

func TestPublicToInternalNodePool_SyncsAutoRepairToPassthrough(t *testing.T) {
	autoRepair := true
	pub := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: testNodePoolName},
		Spec: public.NodePoolSpec{
			AutoRepair: &autoRepair,
			Labels:     map[string]string{"env": "test"},
		},
	}

	result := PublicToInternalNodePool(pub, testAccountID, testNodePoolName)

	require.NotNil(t, result)
	assert.Equal(t, true, result.Spec.NodePool.Management.AutoRepair)
	assert.Equal(t, map[string]string{"env": "test"}, result.Spec.NodePool.NodeLabels)
}

func TestPublicToInternalNodePool_DefaultsAutoRepairToTrue(t *testing.T) {
	pub := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: testNodePoolName},
	}

	result := PublicToInternalNodePool(pub, testAccountID, testNodePoolName)

	require.NotNil(t, result)
	// Matches operator default behavior
	assert.Equal(t, true, result.Spec.NodePool.Management.AutoRepair)
}

func TestPublicToInternalNodePool_NilInput(t *testing.T) {
	result := PublicToInternalNodePool(nil, testAccountID, testNodePoolName)
	assert.Nil(t, result)
}

func TestInternalToPublicNodePool_FiltersServiceSetFields(t *testing.T) {
	cr := &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNodePoolName,
			Namespace: accountNamespace(testAccountID),
			Labels:    map[string]string{accountIDLabel: testAccountID},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			AccountID:      testAccountID,
			InternalPoolID: testNodePoolName,
			AutoRepair:     ptrBool(true),
			Labels:         map[string]string{"env": "test"},
			NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
				Management: hypershiftv1beta1.NodePoolManagement{AutoRepair: true},
				NodeLabels: map[string]string{"env": "test"},
			},
		},
	}

	result := InternalToPublicNodePool(cr)

	require.NotNil(t, result)
	assert.Equal(t, testNodePoolName, result.Name)
	// Service-set fields absent; user-visible fields present
	require.NotNil(t, result.Spec.AutoRepair)
	assert.Equal(t, true, *result.Spec.AutoRepair)
	assert.Equal(t, map[string]string{"env": "test"}, result.Spec.Labels)
}

func TestInternalToPublicNodePool_NilInput(t *testing.T) {
	result := InternalToPublicNodePool(nil)
	assert.Nil(t, result)
}

func TestNodePoolRoundTrip(t *testing.T) {
	autoRepair := true
	original := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: testNodePoolName},
		Spec: public.NodePoolSpec{
			AutoRepair: &autoRepair,
			Labels:     map[string]string{"env": "test"},
		},
	}

	internal := PublicToInternalNodePool(original, testAccountID, testNodePoolName)
	require.NotNil(t, internal)

	// Passthrough synced correctly
	assert.Equal(t, true, internal.Spec.NodePool.Management.AutoRepair)
	assert.Equal(t, original.Spec.Labels, internal.Spec.NodePool.NodeLabels)

	result := InternalToPublicNodePool(internal)
	require.NotNil(t, result)

	require.NotNil(t, result.Spec.AutoRepair)
	assert.Equal(t, *original.Spec.AutoRepair, *result.Spec.AutoRepair)
	assert.Equal(t, original.Spec.Labels, result.Spec.Labels)
}

// --- customer metadata tests ---

func TestCustomerObjectMeta_SetsAccountNamespaceAndProtectsMetadata(t *testing.T) {
	created := metav1.Time{Time: time.Unix(1, 0)}
	input := metav1.ObjectMeta{
		Name:              "res",
		Namespace:         "account-attacker",
		UID:               types.UID("client-uid"),
		ResourceVersion:   "client-rv",
		CreationTimestamp: created,
		Labels: map[string]string{
			"app":          "test",
			accountIDLabel: "attacker",
			seaplanev1alpha1.ClaimedByClusterUIDLabel: "attacker-cluster",
		},
		OwnerReferences: []metav1.OwnerReference{{Name: "attacker"}},
		Finalizers:      []string{"attacker"},
	}

	got := customerObjectMeta(input, testAccountID)

	assert.Equal(t, accountNamespace(testAccountID), got.Namespace)
	assert.Empty(t, got.UID)
	assert.Empty(t, got.ResourceVersion)
	assert.True(t, got.CreationTimestamp.IsZero())
	assert.Empty(t, got.OwnerReferences)
	assert.Empty(t, got.Finalizers)
	assert.NotContains(t, got.Labels, "app")
	assert.Equal(t, testAccountID, got.Labels[accountIDLabel])
	assert.NotContains(t, got.Labels, seaplanev1alpha1.ClaimedByClusterUIDLabel)
	assert.Equal(t, "account-attacker", input.Namespace, "input metadata must not be mutated")
}

// --- Input mutation regression tests ---

// TestPublicToInternalCluster_DoesNotMutateInput verifies that conversion leaves
// pub.ObjectMeta and its labels unchanged.
func TestPublicToInternalCluster_DoesNotMutateInput(t *testing.T) {
	pub := &public.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: "",
			Labels:    map[string]string{"user-key": "user-val"},
		},
	}

	cr := PublicToInternalCluster(pub, testAccountID)

	// CRD must carry the enriched metadata
	assert.Equal(t, accountNamespace(testAccountID), cr.Namespace)
	assert.Equal(t, testAccountID, cr.Labels[accountIDLabel])

	// pub.ObjectMeta must be unchanged
	assert.Equal(t, "", pub.Namespace)
	assert.Equal(t, types.UID(""), pub.UID)
	_, hasAccountLabel := pub.Labels[accountIDLabel]
	assert.False(t, hasAccountLabel, "pub.Labels must not be mutated by conversion")
	assert.Equal(t, "user-val", pub.Labels["user-key"])
}

// TestPublicToInternalNodePool_DoesNotMutateInput verifies that conversion leaves
// pub.ObjectMeta and its labels unchanged.
func TestPublicToInternalNodePool_DoesNotMutateInput(t *testing.T) {
	pub := &public.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNodePoolName,
			Namespace: "",
			Labels:    map[string]string{"user-key": "user-val"},
		},
	}

	np := PublicToInternalNodePool(pub, testAccountID, testNodePoolName)

	// CRD must carry the enriched metadata
	assert.Equal(t, accountNamespace(testAccountID), np.Namespace)
	assert.Equal(t, testAccountID, np.Labels[accountIDLabel])

	// pub.ObjectMeta must be unchanged
	assert.Equal(t, "", pub.Namespace)
	assert.Equal(t, types.UID(""), pub.UID)
	_, hasAccountLabel := pub.Labels[accountIDLabel]
	assert.False(t, hasAccountLabel, "pub.Labels must not be mutated by conversion")
	assert.Equal(t, "user-val", pub.Labels["user-key"])
}

// --- syncNodePoolPassthrough tests ---

func TestSyncNodePoolPassthrough_SyncsAutoRepairAndLabels(t *testing.T) {
	autoRepair := true
	spec := &seaplanev1alpha1.NodePoolSpec{
		AutoRepair: &autoRepair,
		Labels:     map[string]string{"env": "test"},
	}

	syncNodePoolPassthrough(spec, spec.AutoRepair, spec.Labels)

	assert.Equal(t, true, spec.NodePool.Management.AutoRepair)
	assert.Equal(t, spec.Labels, spec.NodePool.NodeLabels)
}

func TestMergeSpecJSON_ReplacesSuppliedNodePoolLabelMaps(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{
		Labels: map[string]string{"old-top-level": "value"},
		NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
			NodeLabels: map[string]string{"old-node-label": "value"},
		},
	}

	err := MergeSpecJSON(spec, []byte(`{"labels":{"new-top-level":"value"},"nodePool":{"nodeLabels":{"new-node-label":"value"}}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"new-top-level": "value"}, spec.Labels)
	assert.Equal(t, map[string]string{"new-node-label": "value"}, spec.NodePool.NodeLabels)
}

func TestMergeSpecJSON_EmptySuppliedNodePoolLabelMapsClearExisting(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{
		Labels: map[string]string{"old-top-level": "value"},
		NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
			NodeLabels: map[string]string{"old-node-label": "value"},
		},
	}

	err := MergeSpecJSON(spec, []byte(`{"labels":{},"nodePool":{"nodeLabels":{}}}`))
	require.NoError(t, err)
	assert.Empty(t, spec.Labels)
	assert.Empty(t, spec.NodePool.NodeLabels)
}

func TestMergeSpecJSON_PreservesOmittedNodePoolLabelMaps(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{
		Labels: map[string]string{"existing": "top-level"},
		NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
			NodeLabels: map[string]string{"existing": "node"},
		},
	}

	err := MergeSpecJSON(spec, []byte(`{"displayName":"updated"}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"existing": "top-level"}, spec.Labels)
	assert.Equal(t, map[string]string{"existing": "node"}, spec.NodePool.NodeLabels)
}

func TestMergeSpecJSON_TypedNilNodePoolSpecReturnsError(t *testing.T) {
	var spec *seaplanev1alpha1.NodePoolSpec

	err := MergeSpecJSON(spec, []byte(`{"labels":{"team":"api"}}`))
	require.Error(t, err)
}

func TestMergeSpecJSON_ReplacesSuppliedNodePoolTags(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{
		Tags: map[string]string{"old": "value", "keep": "old-value"},
	}

	err := MergeSpecJSON(spec, []byte(`{"tags":{"keep":"new-value","new":"value"}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"keep": "new-value", "new": "value"}, spec.Tags)
}

func TestMergeSpecJSON_ExplicitNodePoolTagsClearExisting(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{name: "empty map", json: `{"tags":{}}`},
		{name: "null", json: `{"tags":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &seaplanev1alpha1.NodePoolSpec{Tags: map[string]string{"old": "value"}}

			err := MergeSpecJSON(spec, []byte(tc.json))
			require.NoError(t, err)
			assert.Empty(t, spec.Tags)
		})
	}
}

func TestMergeSpecJSON_PreservesOmittedNodePoolTags(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{Tags: map[string]string{"existing": "value"}}

	err := MergeSpecJSON(spec, []byte(`{"displayName":"updated"}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"existing": "value"}, spec.Tags)
}

func TestMergeSpecJSON_ReplacesSuppliedClusterTags(t *testing.T) {
	spec := &seaplanev1alpha1.ClusterSpec{
		Tags: map[string]string{"old": "value", "keep": "old-value"},
	}

	err := MergeSpecJSON(spec, []byte(`{"tags":{"keep":"new-value","new":"value"}}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"keep": "new-value", "new": "value"}, spec.Tags)
}

func TestMergeSpecJSON_ExplicitClusterTagsClearExisting(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{name: "empty map", json: `{"tags":{}}`},
		{name: "null", json: `{"tags":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := &seaplanev1alpha1.ClusterSpec{Tags: map[string]string{"old": "value"}}

			err := MergeSpecJSON(spec, []byte(tc.json))
			require.NoError(t, err)
			assert.Empty(t, spec.Tags)
		})
	}
}

func TestMergeSpecJSON_PreservesOmittedClusterTags(t *testing.T) {
	spec := &seaplanev1alpha1.ClusterSpec{
		Tags: map[string]string{"existing": "value"},
	}

	err := MergeSpecJSON(spec, []byte(`{"displayName":"updated"}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"existing": "value"}, spec.Tags)
}

func TestSyncNodePoolPassthrough_DefaultsAutoRepairWhenNil(t *testing.T) {
	spec := &seaplanev1alpha1.NodePoolSpec{}

	syncNodePoolPassthrough(spec, nil, nil)

	assert.Equal(t, true, spec.NodePool.Management.AutoRepair)
}

func TestSyncNodePoolPassthrough_NilSpec(t *testing.T) {
	// Must not panic
	syncNodePoolPassthrough(nil, nil, nil)
}

func TestAccountNamespaceUsesAuthenticatedAccountID(t *testing.T) {
	assert.Equal(t, "account-account-123", accountNamespace(testAccountID))
}

func ptrBool(b bool) *bool { return &b }
