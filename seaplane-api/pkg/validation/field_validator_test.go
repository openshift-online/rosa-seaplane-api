package validation

import (
	"testing"
	"time"

	rest "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestValidator(entries map[string]registry.FieldMeta) *FieldValidator {
	return &FieldValidator{
		typedRegistry: registry.TypedFieldRegistry{
			"Test": entries,
		},
		resourceType: "Test",
	}
}

func TestValidateCreate_RejectsServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.accountId": {FieldPath: "spec.accountId", WriteMode: registry.ServiceSet},
		"spec.name":      {FieldPath: "spec.name", WriteMode: registry.Mutable},
	})

	spec := map[string]any{
		"accountId": "123",
		"name":      "my-cluster",
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation errors, got nil")
	}

	found := false
	for _, e := range errs {
		if e.Field == "spec.accountId" {
			found = true
		}
		if e.Field == "spec.name" {
			t.Error("mutable field 'spec.name' should not be rejected")
		}
	}
	if !found {
		t.Error("expected error for service-set field 'spec.accountId'")
	}
}

func TestValidateCreate_SkipsServiceSetFieldsAtZeroValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.networking": {FieldPath: "spec.networking", WriteMode: registry.ServiceSet},
		"spec.etcd":       {FieldPath: "spec.etcd", WriteMode: registry.ServiceSet},
		"spec.fips":       {FieldPath: "spec.fips", WriteMode: registry.ServiceSet},
		"spec.services":   {FieldPath: "spec.services", WriteMode: registry.ServiceSet},
		"spec.pullSecret": {FieldPath: "spec.pullSecret", WriteMode: registry.ServiceSet},
	})

	spec := map[string]any{
		"networking": map[string]any{},
		"etcd":       map[string]any{"managementType": "", "managed": map[string]any{}},
		"fips":       false,
		"services":   nil,
		"pullSecret": map[string]any{"name": ""},
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors for zero-value service-set fields, got %v", errs)
	}
}

func TestValidateCreate_RejectsNonZeroServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.networking": {FieldPath: "spec.networking", WriteMode: registry.ServiceSet},
		"spec.fips":       {FieldPath: "spec.fips", WriteMode: registry.ServiceSet},
	})

	spec := map[string]any{
		"networking": map[string]any{"clusterNetwork": []any{map[string]any{"cidr": "10.0.0.0/8"}}},
		"fips":       true,
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation errors for non-zero service-set fields, got nil")
	}
	if len(errs) != 2 {
		t.Errorf("expected 2 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateCreate_AllowsMutableFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.displayName": {FieldPath: "spec.displayName", WriteMode: registry.Mutable},
	})

	spec := map[string]any{
		"displayName": "test",
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateCreate_AllowsMutableFieldsInsideServiceSetContainer(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.proxy": {
			FieldPath: "spec.proxy",
			WriteMode: registry.ServiceSet,
		},
		"spec.proxy.httpProxy": {
			FieldPath: "spec.proxy.httpProxy",
			WriteMode: registry.Mutable,
		},
		"spec.proxy.httpsProxy": {
			FieldPath: "spec.proxy.httpsProxy",
			WriteMode: registry.Mutable,
		},
	})

	errs := v.ValidateCreate(map[string]any{
		"proxy": map[string]any{
			"httpProxy": "http://proxy.example.com:8080",
		},
	}, featuregate.Default)
	if errs != nil {
		t.Errorf("expected mutable child field to be accepted, got %v", errs)
	}
}

func TestValidateResourceTagsPassthroughIsServiceSet(t *testing.T) {
	tests := []struct {
		name         string
		resourceType string
		parentField  string
		fieldPath    string
	}{
		{
			name:         "cluster",
			resourceType: "Cluster",
			parentField:  "hostedCluster",
			fieldPath:    "spec.hostedCluster.platform.aws.resourceTags",
		},
		{
			name:         "nodepool",
			resourceType: "NodePool",
			parentField:  "nodePool",
			fieldPath:    "spec.nodePool.platform.aws.resourceTags",
		},
	}
	passthroughSpec := func(parentField, value string) map[string]any {
		return map[string]any{
			"tags": map[string]any{"owner": "root-level"},
			parentField: map[string]any{
				"platform": map[string]any{
					"aws": map[string]any{
						"resourceTags": []any{map[string]any{"key": "legacy", "value": value}},
					},
				},
			},
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			validator := NewFieldValidator(test.resourceType)
			createErrs := validator.ValidateCreate(passthroughSpec(test.parentField, "value"), featuregate.Default)
			if !hasFieldError(createErrs, test.fieldPath) {
				t.Fatalf("expected create error for %s, got %v", test.fieldPath, createErrs)
			}
			if hasFieldError(createErrs, "spec.tags") {
				t.Fatal("root-level tags should remain writable")
			}

			oldSpec := passthroughSpec(test.parentField, "old-value")
			if errs := validator.ValidateUpdate(oldSpec, oldSpec, featuregate.Default); len(errs) != 0 {
				t.Fatalf("unchanged service-set field should be allowed during update, got %v", errs)
			}
			updateErrs := validator.ValidateUpdate(passthroughSpec(test.parentField, "new-value"), oldSpec, featuregate.Default)
			if !hasFieldError(updateErrs, test.fieldPath) {
				t.Fatalf("expected update error for %s, got %v", test.fieldPath, updateErrs)
			}
			if hasFieldError(updateErrs, "spec.tags") {
				t.Fatal("root-level tags should remain writable")
			}
		})
	}
}

func hasFieldError(errs ValidationErrors, field string) bool {
	for _, err := range errs {
		if err.Field == field {
			return true
		}
	}
	return false
}

func TestValidateCreate_AllowsProxyConfiguration(t *testing.T) {
	v := NewFieldValidator("Cluster")
	errs := v.ValidateCreate(rest.ClusterSpec{
		HostedCluster: rest.HostedClusterSpecPassthrough{
			Configuration: &rest.ClusterConfiguration{
				Proxy: &rest.ProxyConfiguration{
					HTTPProxy:  "http://proxy.example.com:8080",
					HTTPSProxy: "https://proxy.example.com:8443",
					NoProxy:    "localhost,127.0.0.1",
				},
			},
		},
	}, featuregate.Default)
	if errs != nil {
		t.Errorf("expected proxy configuration to be accepted, got %v", errs)
	}
}

// TestValidateCreate_FIPSZeroValueStillAllowed confirms the pointer-aware ServiceSet
// check still falls back correctly for FIPS, a plain (non-pointer) bool field.
func TestValidateCreate_FIPSZeroValueStillAllowed(t *testing.T) {
	v := NewFieldValidator("Cluster")

	errs := v.ValidateCreate(rest.ClusterSpec{
		HostedCluster: rest.HostedClusterSpecPassthrough{FIPS: false},
	}, featuregate.Default)
	if errs != nil {
		t.Errorf("expected FIPS zero value to be allowed, got %v", errs)
	}

	errs = v.ValidateCreate(rest.ClusterSpec{
		HostedCluster: rest.HostedClusterSpecPassthrough{FIPS: true},
	}, featuregate.Default)
	found := false
	for _, e := range errs {
		if e.Field == "spec.hostedCluster.fips" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected explicit fips:true to still be rejected, got %v", errs)
	}
}

func TestValidateCreate_ValidatesAdditionalTrustBundle(t *testing.T) {
	v := NewFieldValidator("Cluster")
	errs := v.ValidateCreate(map[string]any{
		"additionalTrustBundle": "not a certificate",
	}, featuregate.Default)
	if len(errs) != 1 || errs[0].Field != "spec.additionalTrustBundle" {
		t.Fatalf("expected additionalTrustBundle validation error, got %v", errs)
	}
}

func TestValidateUpdate_ValidatesAdditionalTrustBundleAndAllowsClear(t *testing.T) {
	v := NewFieldValidator("Cluster")
	existing := map[string]any{"additionalTrustBundle": "previous bundle"}
	if errs := v.ValidateUpdate(map[string]any{"additionalTrustBundle": "not a certificate"}, existing, featuregate.Default); len(errs) != 1 || errs[0].Field != "spec.additionalTrustBundle" {
		t.Fatalf("expected additionalTrustBundle validation error, got %v", errs)
	}
	if errs := v.ValidateUpdate(map[string]any{"additionalTrustBundle": ""}, existing, featuregate.Default); errs != nil {
		t.Fatalf("expected empty string to clear the bundle, got %v", errs)
	}
}

func TestValidateCreate_FeatureGateIgnoresEmptyValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.autoscaling": {
			FieldPath:   "spec.autoscaling",
			WriteMode:   registry.Mutable,
			FeatureGate: "SeaplaneAutoScaling",
		},
	})

	err := v.ValidateCreate(map[string]any{
		"autoscaling": map[string]any{},
	}, featuregate.Default)
	if err != nil {
		t.Fatalf("expected empty feature-gated field to be treated as unset, got %v", err)
	}
}

func TestValidateCreate_FeatureGateRejectsConfiguredValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.autoscaling": {
			FieldPath:   "spec.autoscaling",
			WriteMode:   registry.Mutable,
			FeatureGate: "SeaplaneAutoScaling",
		},
	})

	err := v.ValidateCreate(map[string]any{
		"autoscaling": map[string]any{"maxNodesTotal": 10},
	}, featuregate.Default)
	if err == nil {
		t.Fatal("expected configured feature-gated field to be rejected")
	}
}

func TestValidateCreate_FeatureGateRejectsScalarZeroValues(t *testing.T) {
	for name, value := range map[string]any{
		"false":        false,
		"zero":         0,
		"empty string": "",
	} {
		t.Run(name, func(t *testing.T) {
			v := newTestValidator(map[string]registry.FieldMeta{
				"spec.setting": {
					FieldPath:   "spec.setting",
					WriteMode:   registry.Mutable,
					FeatureGate: "SeaplaneKubeletAdvanced",
				},
			})

			errs := v.ValidateCreate(map[string]any{"setting": value}, featuregate.Default)
			if errs == nil {
				t.Fatalf("expected feature-gated scalar zero value %#v to be rejected", value)
			}
		})
	}
}

func TestValidateCreate_FeatureGateRejectsObjectContainingZeroValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.kubelet": {
			FieldPath:   "spec.kubelet",
			WriteMode:   registry.Mutable,
			FeatureGate: "SeaplaneKubeletAdvanced",
		},
	})

	errs := v.ValidateCreate(map[string]any{
		"kubelet": map[string]any{"serializeImagePulls": false},
	}, featuregate.Default)
	if errs == nil {
		t.Fatal("expected feature-gated object containing an explicit zero value to be rejected")
	}
}

func TestValidateCreate_AllowsImmutableFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.fips": {FieldPath: "spec.fips", WriteMode: registry.Immutable},
	})

	spec := map[string]any{
		"fips": true,
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors on create for immutable field, got %v", errs)
	}
}

func TestValidateUpdate_RejectsImmutableFieldChange(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.fips": {FieldPath: "spec.fips", WriteMode: registry.Immutable},
	})

	existing := map[string]any{"fips": true}
	updated := map[string]any{"fips": false}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation error for immutable field change")
	}

	if errs[0].Field != "spec.fips" {
		t.Errorf("expected error on spec.fips, got %s", errs[0].Field)
	}
}

func TestClusterCapabilities_AreCreateOnly(t *testing.T) {
	v := NewFieldValidator("Cluster")

	initial := map[string]any{
		"hostedCluster": map[string]any{
			"capabilities": map[string]any{"enabled": []string{"Console"}},
		},
	}
	if errs := v.ValidateCreate(initial, featuregate.Default); errs != nil {
		t.Fatalf("expected capabilities to be allowed on create, got %v", errs)
	}

	updated := map[string]any{
		"hostedCluster": map[string]any{
			"capabilities": map[string]any{"enabled": []string{"Console", "Ingress"}},
		},
	}
	errs := v.ValidateUpdate(updated, initial, featuregate.Default)
	if len(errs) != 1 || errs[0].Field != "spec.hostedCluster.capabilities.enabled" {
		t.Fatalf("expected capability change to be rejected, got %v", errs)
	}

	updatedDisabled := map[string]any{
		"hostedCluster": map[string]any{
			"capabilities": map[string]any{
				"enabled":  []string{"Console"},
				"disabled": []string{"baremetal"},
			},
		},
	}
	errs = v.ValidateUpdate(updatedDisabled, initial, featuregate.Default)
	if len(errs) != 1 || errs[0].Field != "spec.hostedCluster.capabilities.disabled" {
		t.Fatalf("expected newly-set disabled capability to be rejected, got %v", errs)
	}

	// Resubmitting the identical value (e.g. Terraform sending the full object)
	// must not be treated as a change.
	same := map[string]any{
		"hostedCluster": map[string]any{
			"capabilities": map[string]any{"enabled": []string{"Console"}},
		},
	}
	if errs := v.ValidateUpdate(same, initial, featuregate.Default); len(errs) != 0 {
		t.Fatalf("expected identical capabilities to be accepted on update, got %v", errs)
	}
}

func TestValidateUpdate_AllowsImmutableFieldSameValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.fips": {FieldPath: "spec.fips", WriteMode: registry.Immutable},
	})

	existing := map[string]any{"fips": true}
	updated := map[string]any{"fips": true}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors when immutable field unchanged, got %v", errs)
	}
}

func TestValidateUpdateWithRawSpec_UsesExplicitImmutableFieldValues(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.value": {FieldPath: "spec.value", WriteMode: registry.Immutable},
	})
	type testSpec struct {
		Value string `json:"value,omitempty"`
	}

	tests := []struct {
		name       string
		newValue   string
		rawValue   any
		sendValue  bool
		wantErrors int
	}{
		{name: "omitted", wantErrors: 0},
		{name: "unchanged", newValue: "configured", rawValue: "configured", sendValue: true, wantErrors: 0},
		{name: "changed", newValue: "other", rawValue: "other", sendValue: true, wantErrors: 1},
		{name: "cleared with empty string", rawValue: "", sendValue: true, wantErrors: 1},
		{name: "cleared with null", rawValue: nil, sendValue: true, wantErrors: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawSpec := map[string]any{"unrelated": true}
			if tt.sendValue {
				rawSpec["value"] = tt.rawValue
			}

			errs := v.ValidateUpdateWithRawSpec(testSpec{Value: tt.newValue}, rawSpec, testSpec{Value: "configured"}, featuregate.Default)
			if len(errs) != tt.wantErrors {
				t.Fatalf("ValidateUpdateWithRawSpec() returned %v, want %d error(s)", errs, tt.wantErrors)
			}
			if tt.wantErrors > 0 && errs[0].Field != "spec.value" {
				t.Errorf("error field = %q, want spec.value", errs[0].Field)
			}
		})
	}
}

func TestValidateUpdateWithRawSpec_RequiresRawSpec(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{})
	type testSpec struct {
		Value string `json:"value,omitempty"`
	}

	tests := []struct {
		name    string
		rawSpec map[string]any
	}{
		{name: "nil raw spec"},
		{name: "empty raw spec", rawSpec: map[string]any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := v.ValidateUpdateWithRawSpec(testSpec{}, tt.rawSpec, testSpec{}, featuregate.Default)
			if len(errs) != 1 || errs[0].Field != "spec" {
				t.Fatalf("ValidateUpdateWithRawSpec() returned %v, want one raw-spec error", errs)
			}
		})
	}
}

func TestValidateUpdateWithRawSpec_RejectsNullParentOfImmutableField(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.parent.child": {FieldPath: "spec.parent.child", WriteMode: registry.Immutable},
	})
	type testSpec struct {
		Parent *struct {
			Child string `json:"child,omitempty"`
		} `json:"parent,omitempty"`
	}

	tests := []struct {
		name       string
		oldValue   any
		wantErrors int
	}{
		{name: "non-empty immutable descendant", oldValue: "configured", wantErrors: 1},
		{name: "zero-valued immutable descendant", oldValue: "", wantErrors: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := map[string]any{
				"parent": map[string]any{"child": tt.oldValue},
			}
			errs := v.ValidateUpdateWithRawSpec(testSpec{}, map[string]any{"parent": nil}, existing, featuregate.Default)
			if len(errs) != tt.wantErrors {
				t.Fatalf("ValidateUpdateWithRawSpec() returned %v, want %d error(s)", errs, tt.wantErrors)
			}
			if tt.wantErrors > 0 && errs[0].Field != "spec.parent.child" {
				t.Errorf("error field = %q, want spec.parent.child", errs[0].Field)
			}
		})
	}
}

func TestValidateUpdate_RawMapRejectsNullParentOfImmutableField(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.parent.child": {FieldPath: "spec.parent.child", WriteMode: registry.Immutable},
	})
	existing := map[string]any{
		"parent": map[string]any{"child": "configured"},
	}

	errs := v.ValidateUpdate(map[string]any{"parent": nil}, existing, featuregate.Default)
	if len(errs) != 1 || errs[0].Field != "spec.parent.child" {
		t.Fatalf("ValidateUpdate() returned %v, want one error on spec.parent.child", errs)
	}
}

func TestValidateUpdate_RejectsImmutableFieldChangeFromOmittedZeroValue(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.oidcConfigId": {FieldPath: "spec.oidcConfigId", WriteMode: registry.Immutable},
	})

	// An omitempty zero value is absent from the flattened existing spec entirely,
	// which must still be treated as a change when the update sets a real value.
	existing := map[string]any{}
	updated := map[string]any{"oidcConfigId": "new-oidc-config"}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation error when setting an immutable field that was previously omitted")
	}

	if errs[0].Field != "spec.oidcConfigId" {
		t.Errorf("expected error on spec.oidcConfigId, got %s", errs[0].Field)
	}
}

func TestValidateCreate_RejectsFeatureGatedField(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.tags": {FieldPath: "spec.tags", WriteMode: registry.Mutable, FeatureGate: "SeaplaneAutoScaling"},
	})

	spec := map[string]any{
		"tags": map[string]string{"env": "prod"},
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation error for feature-gated field")
	}

	if errs[0].Field != "spec.tags" {
		t.Errorf("expected error on spec.tags, got %s", errs[0].Field)
	}
}

func TestValidateCreate_AllowsFeatureGatedFieldWhenEnabled(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.tags": {FieldPath: "spec.tags", WriteMode: registry.Mutable, FeatureGate: "SeaplaneAutoScaling"},
	})

	spec := map[string]any{
		"tags": map[string]string{"env": "prod"},
	}

	errs := v.ValidateCreate(spec, featuregate.TechPreviewNoUpgrade)
	if errs != nil {
		t.Errorf("expected no errors with gate enabled, got %v", errs)
	}
}

func TestValidateCreate_NilSpecReturnsNil(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{})
	errs := v.ValidateCreate(nil, featuregate.Default)
	if errs != nil {
		t.Errorf("expected nil for nil spec, got %v", errs)
	}
}

func TestValidateUpdate_RejectsServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.creatorARN": {FieldPath: "spec.creatorARN", WriteMode: registry.ServiceSet},
	})

	existing := map[string]any{"creatorARN": "old-arn"}
	updated := map[string]any{"creatorARN": "new-arn"}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation error for service-set field on update")
	}
}

func TestValidateUpdate_AllowsUnchangedServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.creatorARN": {FieldPath: "spec.creatorARN", WriteMode: registry.ServiceSet},
	})

	existing := map[string]any{"creatorARN": "same-arn"}
	updated := map[string]any{"creatorARN": "same-arn"}

	if errs := v.ValidateUpdate(updated, existing, featuregate.Default); errs != nil {
		t.Fatalf("expected unchanged service-set field to be allowed, got %v", errs)
	}
}

func TestValidateUpdate_AllowsUnchangedNestedServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.management": {FieldPath: "spec.management", WriteMode: registry.ServiceSet},
	})

	existing := map[string]any{"management": map[string]any{"autoRepair": true, "upgradeType": "Replace"}}
	updated := map[string]any{"management": map[string]any{"autoRepair": true, "upgradeType": "Replace"}}
	if errs := v.ValidateUpdate(updated, existing, featuregate.Default); errs != nil {
		t.Errorf("unchanged service-set fields should be allowed on round-trip update, got %v", errs)
	}
}

func TestValidateUpdate_RejectsServiceSetFieldsWithoutExistingSpec(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.creatorARN": {FieldPath: "spec.creatorARN", WriteMode: registry.ServiceSet},
	})

	updated := map[string]any{"creatorARN": "some-arn"}

	if errs := v.ValidateUpdate(updated, nil, featuregate.Default); errs == nil {
		t.Fatal("expected service-set field to be rejected without existing spec")
	}
}

func TestValidateUpdate_AllowsMatchingServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.accountId": {FieldPath: "spec.accountId", WriteMode: registry.ServiceSet},
	})

	errs := v.ValidateUpdate(
		map[string]any{"accountId": "acct-1", "properties": map[string]any{"changed": "true"}},
		map[string]any{"accountId": "acct-1"},
		featuregate.Default,
	)
	if errs != nil {
		t.Fatalf("echo of server accountId should pass, got %v", errs)
	}
}

func TestValidateUpdate_AllowsBothUnsetServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.accountId": {FieldPath: "spec.accountId", WriteMode: registry.ServiceSet},
	})

	// Client sends empty; server has no value. Treat as both unset.
	errs := v.ValidateUpdate(map[string]any{"accountId": ""}, map[string]any{}, featuregate.Default)
	if errs != nil {
		t.Fatalf("both-unset service-set should pass, got %v", errs)
	}
}

func TestValidateUpdate_RejectsClearingServiceSetFields(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.creatorARN": {FieldPath: "spec.creatorARN", WriteMode: registry.ServiceSet},
	})

	existing := map[string]any{"creatorARN": "arn:aws:iam::123456789012:user/platform"}
	updated := map[string]any{"creatorARN": ""}
	if errs := v.ValidateUpdate(updated, existing, featuregate.Default); errs == nil {
		t.Fatal("expected validation error when clearing a non-zero service-set field")
	}
}

func TestValidateUpdate_RejectsServiceSetCaseVariant(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.accountId": {FieldPath: "spec.accountId", WriteMode: registry.ServiceSet},
	})

	for _, key := range []string{"AccountId", "ACCOUNTID"} {
		errs := v.ValidateUpdate(map[string]any{key: "999"}, map[string]any{"accountId": "acct-1"}, featuregate.Default)
		if errs == nil {
			t.Fatalf("key %q: expected rejection", key)
		}
		if errs[0].Field != "spec.accountId" {
			t.Errorf("key %q: field = %q, want spec.accountId", key, errs[0].Field)
		}
	}
}

func TestFlattenToFieldPaths(t *testing.T) {
	spec := map[string]any{
		"name": "test",
		"hostedCluster": map[string]any{
			"fips":    true,
			"channel": "stable-4.16",
		},
	}

	result := flattenToFieldPaths(spec)

	expected := []string{
		"spec.name",
		"spec.hostedCluster",
		"spec.hostedCluster.fips",
		"spec.hostedCluster.channel",
	}

	for _, path := range expected {
		if _, found := result[path]; !found {
			t.Errorf("expected path %s in result, not found", path)
		}
	}
}

// Root volume write-mode matches v1: size is settable on create only; type/iops/encrypted/encryptionKey
// are always computed by the platform and enforced here against the real registry.FieldRegistry.
func TestValidateCreate_RejectsNonDefaultRootVolumeServiceSetFields(t *testing.T) {
	v := NewFieldValidator("NodePool")

	newSpecWithRootVolume := func(rv rest.Volume) *rest.NodePoolSpec {
		return &rest.NodePoolSpec{
			NodePool: rest.NodePoolSpecPassthrough{
				Platform: rest.NodePoolPlatform{
					AWS: &rest.AWSNodePoolPlatform{
						RootVolume: &rv,
					},
				},
			},
		}
	}

	trueVal, falseVal := true, false
	tests := []struct {
		name      string
		rv        rest.Volume
		wantField string
	}{
		{"type", rest.Volume{Size: 500, Type: "io2"}, "spec.nodePool.platform.aws.rootVolume.type"},
		{"iops", rest.Volume{Size: 500, IOPS: 3000}, "spec.nodePool.platform.aws.rootVolume.iops"},
		{"encrypted true", rest.Volume{Size: 500, Encrypted: &trueVal}, "spec.nodePool.platform.aws.rootVolume.encrypted"},
		// A non-nil pointer to false is still an explicit value, not an omitted field.
		{"encrypted false", rest.Volume{Size: 500, Encrypted: &falseVal}, "spec.nodePool.platform.aws.rootVolume.encrypted"},
		{"encryptionKey", rest.Volume{Size: 500, EncryptionKey: "arn:aws:kms:us-east-1:123:key/abc"}, "spec.nodePool.platform.aws.rootVolume.encryptionKey"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := v.ValidateCreate(newSpecWithRootVolume(tt.rv), featuregate.Default)
			if errs == nil {
				t.Fatalf("expected validation error for non-default rootVolume.%s, got nil", tt.name)
			}
			found := false
			for _, e := range errs {
				if e.Field == tt.wantField {
					found = true
					if e.Reason != "field is platform-managed and cannot be set by customers" {
						t.Errorf("unexpected reason: %s", e.Reason)
					}
				}
			}
			if !found {
				t.Errorf("expected error on %s, got %v", tt.wantField, errs)
			}
		})
	}
}

func TestValidateCreate_AllowsRootVolumeWithOnlySizeSet(t *testing.T) {
	v := NewFieldValidator("NodePool")

	spec := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					RootVolume: &rest.Volume{Size: 500},
				},
			},
		},
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors for rootVolume with only size set, got %v", errs)
	}
}

func TestValidateUpdate_RejectsRootVolumeSizeChange(t *testing.T) {
	v := NewFieldValidator("NodePool")

	existing := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					RootVolume: &rest.Volume{Size: 500},
				},
			},
		},
	}
	updated := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					RootVolume: &rest.Volume{Size: 800},
				},
			},
		},
	}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs == nil {
		t.Fatal("expected validation error for changing rootVolume.size on update")
	}
	found := false
	for _, e := range errs {
		if e.Field == "spec.nodePool.platform.aws.rootVolume.size" {
			found = true
			if e.Reason != "field is immutable and cannot be changed after creation" {
				t.Errorf("unexpected reason: %s", e.Reason)
			}
		}
	}
	if !found {
		t.Errorf("expected error on spec.nodePool.platform.aws.rootVolume.size, got %v", errs)
	}
}

func TestValidateUpdate_AllowsRootVolumeUnchanged(t *testing.T) {
	v := NewFieldValidator("NodePool")

	spec := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					RootVolume: &rest.Volume{Size: 500},
				},
			},
		},
	}

	errs := v.ValidateUpdate(spec, spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors when rootVolume is unchanged, got %v", errs)
	}
}

func TestValidateCreate_FeatureGateAwareWriteMode(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.maxPods": {
			FieldPath: "spec.maxPods",
			WriteMode: registry.ServiceSet,
			FeatureGateAwareWriteModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "SeaplaneKubeletAdvanced", WriteMode: registry.Mutable},
				{FeatureGate: "", WriteMode: registry.ServiceSet},
			},
		},
	})

	spec := map[string]any{"maxPods": 110}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs == nil {
		t.Fatal("expected error without feature gate")
	}

	errs = v.ValidateCreate(spec, featuregate.TechPreviewNoUpgrade)
	if errs != nil {
		t.Errorf("expected no errors with gate enabled, got %v", errs)
	}
}

// A Windows pool is License Included: the image family is fixed at the AMI the
// pool launches from, so it can be chosen on create but never changed.
func TestValidateCreate_AllowsWindowsImageType(t *testing.T) {
	v := NewFieldValidator("NodePool")

	spec := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					ImageType: hypershiftv1beta1.ImageTypeWindows,
				},
			},
		},
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors selecting Windows on create, got %v", errs)
	}
}

func TestValidateUpdate_RejectsImageTypeChange(t *testing.T) {
	v := NewFieldValidator("NodePool")

	specWithImageType := func(imageType hypershiftv1beta1.ImageType) *rest.NodePoolSpec {
		return &rest.NodePoolSpec{
			NodePool: rest.NodePoolSpecPassthrough{
				Platform: rest.NodePoolPlatform{
					AWS: &rest.AWSNodePoolPlatform{
						ImageType: imageType,
					},
				},
			},
		}
	}

	tests := []struct {
		name     string
		existing *rest.NodePoolSpec
		updated  *rest.NodePoolSpec
	}{
		{"linux to windows", specWithImageType(hypershiftv1beta1.ImageTypeLinux), specWithImageType(hypershiftv1beta1.ImageTypeWindows)},
		{"windows to linux", specWithImageType(hypershiftv1beta1.ImageTypeWindows), specWithImageType(hypershiftv1beta1.ImageTypeLinux)},
		// Unset means Linux, so adopting Windows later is still a conversion.
		{"unset to windows", specWithImageType(""), specWithImageType(hypershiftv1beta1.ImageTypeWindows)},
		// Blanking a Windows pool back to "" cannot be seen here: imageType is
		// omitempty, so it drops out of the flattened spec entirely. The
		// handler catches that case off the raw request body instead, see
		// TestNodePoolHandlerUpdateRejectsBlankedImageType.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := v.ValidateUpdate(tt.updated, tt.existing, featuregate.Default)
			if errs == nil {
				t.Fatal("expected validation error for changing imageType on update")
			}
			found := false
			for _, e := range errs {
				if e.Field == "spec.nodePool.platform.aws.imageType" {
					found = true
					if e.Reason != "field is immutable and cannot be changed after creation" {
						t.Errorf("unexpected reason: %s", e.Reason)
					}
				}
			}
			if !found {
				t.Errorf("expected error on spec.nodePool.platform.aws.imageType, got %v", errs)
			}
		})
	}
}

func TestValidateUpdate_AllowsImageTypeUnchanged(t *testing.T) {
	v := NewFieldValidator("NodePool")

	spec := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			Platform: rest.NodePoolPlatform{
				AWS: &rest.AWSNodePoolPlatform{
					ImageType: hypershiftv1beta1.ImageTypeWindows,
				},
			},
		},
	}

	errs := v.ValidateUpdate(spec, spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors when imageType is unchanged, got %v", errs)
	}
}

func TestValidateUpdate_RejectsHiddenServiceSetChange(t *testing.T) {
	v := newTestValidator(map[string]registry.FieldMeta{
		"spec.hostedCluster.pullSecret": {FieldPath: "spec.hostedCluster.pullSecret", WriteMode: registry.ServiceSet, Hidden: true},
	})

	existing := map[string]any{"hostedCluster": map[string]any{"pullSecret": map[string]any{"name": "platform"}}}
	updated := map[string]any{"hostedCluster": map[string]any{"pullSecret": map[string]any{"name": "attacker"}}}
	if errs := v.ValidateUpdate(updated, existing, featuregate.Default); errs == nil {
		t.Fatal("expected rejection of hidden service-set field change")
	}
	if errs := v.ValidateUpdate(existing, existing, featuregate.Default); errs != nil {
		t.Fatalf("echo of hidden service-set field should pass, got %v", errs)
	}
}

// nodeVolumeDetachTimeout was platform-managed until it was enabled for v2
// alongside its sibling nodeDrainTimeout. Customers can now set it on create
// and retune it afterwards.
func TestValidateCreate_AllowsNodeVolumeDetachTimeout(t *testing.T) {
	v := NewFieldValidator("NodePool")

	spec := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			NodeVolumeDetachTimeout: &metav1.Duration{Duration: 25 * time.Minute},
		},
	}

	errs := v.ValidateCreate(spec, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors setting nodeVolumeDetachTimeout on create, got %v", errs)
	}
}

func TestValidateUpdate_AllowsNodeVolumeDetachTimeoutChange(t *testing.T) {
	v := NewFieldValidator("NodePool")

	existing := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			NodeVolumeDetachTimeout: &metav1.Duration{Duration: 25 * time.Minute},
		},
	}
	updated := &rest.NodePoolSpec{
		NodePool: rest.NodePoolSpecPassthrough{
			NodeVolumeDetachTimeout: &metav1.Duration{Duration: 45 * time.Minute},
		},
	}

	errs := v.ValidateUpdate(updated, existing, featuregate.Default)
	if errs != nil {
		t.Errorf("expected no errors retuning nodeVolumeDetachTimeout, got %v", errs)
	}
}
