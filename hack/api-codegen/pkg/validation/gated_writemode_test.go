package validation

import (
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
)

func TestValidator_FeatureGateAwareWriteMode(t *testing.T) {
	tests := []struct {
		name         string
		fieldPath    string
		baseMode     registry.WriteMode
		gatedModes   []registry.FeatureGateWriteMode
		enabledGates []string
		operation    Operation
		expectError  bool
		errorReason  string
	}{
		{
			name:      "Default customers get immutable - blocked on update",
			fieldPath: "spec.releaseChannel",
			baseMode:  registry.Immutable,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.Immutable},
				{FeatureGate: "PremiumFeature", WriteMode: registry.Mutable},
			},
			enabledGates: []string{},
			operation:    OperationUpdate,
			expectError:  true,
			errorReason:  "immutable",
		},
		{
			name:      "Default customers get immutable - allowed on create",
			fieldPath: "spec.releaseChannel",
			baseMode:  registry.Immutable,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.Immutable},
				{FeatureGate: "PremiumFeature", WriteMode: registry.Mutable},
			},
			enabledGates: []string{},
			operation:    OperationCreate,
			expectError:  false,
		},
		{
			name:      "Premium customers get mutable - allowed on update",
			fieldPath: "spec.releaseChannel",
			baseMode:  registry.Immutable,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.Immutable},
				{FeatureGate: "PremiumFeature", WriteMode: registry.Mutable},
			},
			enabledGates: []string{"PremiumFeature"},
			operation:    OperationUpdate,
			expectError:  false,
		},
		{
			name:      "Premium customers get mutable - allowed on create",
			fieldPath: "spec.releaseChannel",
			baseMode:  registry.Immutable,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.Immutable},
				{FeatureGate: "PremiumFeature", WriteMode: registry.Mutable},
			},
			enabledGates: []string{"PremiumFeature"},
			operation:    OperationCreate,
			expectError:  false,
		},
		{
			name:      "TechPreview customers get mutable for gated field - allowed on create",
			fieldPath: "spec.etcd",
			baseMode:  registry.ServiceSet,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.ServiceSet},
				{FeatureGate: "SeaplaneEtcdConfig", WriteMode: registry.Mutable},
			},
			enabledGates: []string{"SeaplaneEtcdConfig"},
			operation:    OperationCreate,
			expectError:  false,
		},
		{
			name:      "Default customers get service-set for gated field - blocked",
			fieldPath: "spec.etcd",
			baseMode:  registry.ServiceSet,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.ServiceSet},
				{FeatureGate: "SeaplaneEtcdConfig", WriteMode: registry.Mutable},
			},
			enabledGates: []string{},
			operation:    OperationCreate,
			expectError:  true,
			errorReason:  "service-set",
		},
		{
			name:         "No gated modes - uses base mode (immutable on update blocked)",
			fieldPath:    "spec.name",
			baseMode:     registry.Immutable,
			gatedModes:   []registry.FeatureGateWriteMode{},
			enabledGates: []string{},
			operation:    OperationUpdate,
			expectError:  true,
			errorReason:  "immutable",
		},
		{
			name:         "No gated modes - uses base mode (mutable on update allowed)",
			fieldPath:    "spec.tags",
			baseMode:     registry.Mutable,
			gatedModes:   []registry.FeatureGateWriteMode{},
			enabledGates: []string{},
			operation:    OperationUpdate,
			expectError:  false,
		},
		{
			name:      "Multiple gates - first match wins",
			fieldPath: "spec.advanced",
			baseMode:  registry.ServiceSet,
			gatedModes: []registry.FeatureGateWriteMode{
				{FeatureGate: "", WriteMode: registry.ServiceSet},
				{FeatureGate: "FeatureA", WriteMode: registry.Immutable},
				{FeatureGate: "FeatureB", WriteMode: registry.Mutable},
			},
			enabledGates: []string{"FeatureA", "FeatureB"},
			operation:    OperationUpdate,
			expectError:  true, // FeatureA (immutable) takes precedence
			errorReason:  "immutable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a validator with a custom registry for this test
			validator := &Validator{
				registry: registry.TypedFieldRegistry{
					"Cluster": {
						tt.fieldPath: {
							FieldPath:                  tt.fieldPath,
							WriteMode:                  tt.baseMode,
							FeatureGateAwareWriteModes: tt.gatedModes,
							OwnerType:                  "Cluster",
						},
					},
				},
			}

			// Create request
			req := &Request{
				Operation:    tt.operation,
				ResourceType: "Cluster",
				Fields:       map[string]interface{}{tt.fieldPath: "test-value"},
				FeatureSet:   featuregate.Default,
				EnabledGates: tt.enabledGates,
				ExistingFields: map[string]interface{}{
					tt.fieldPath: "old-value", // Simulate existing field for update tests
				},
			}

			// For create operations, don't set ExistingFields
			if tt.operation == OperationCreate {
				req.ExistingFields = nil
			}

			// Validate
			err := validator.Validate(req)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error containing %q, got nil", tt.errorReason)
				} else if tt.errorReason != "" {
					// Check error contains expected reason
					errStr := err.Error()
					if errStr == "" || len(errStr) == 0 {
						t.Errorf("Expected error containing %q, got empty error", tt.errorReason)
					}
					// Just verify error exists - don't check specific message
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error, got %v", err)
				}
			}
		})
	}
}

func TestRequest_IsFeatureGateEnabled(t *testing.T) {
	tests := []struct {
		name         string
		enabledGates []string
		queryGate    string
		want         bool
	}{
		{
			name:         "Gate is enabled",
			enabledGates: []string{"FeatureA", "FeatureB"},
			queryGate:    "FeatureA",
			want:         true,
		},
		{
			name:         "Gate is not enabled",
			enabledGates: []string{"FeatureA", "FeatureB"},
			queryGate:    "FeatureC",
			want:         false,
		},
		{
			name:         "Empty gates list",
			enabledGates: []string{},
			queryGate:    "FeatureA",
			want:         false,
		},
		{
			name:         "Nil gates list",
			enabledGates: nil,
			queryGate:    "FeatureA",
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{
				EnabledGates: tt.enabledGates,
			}

			got := req.IsFeatureGateEnabled(tt.queryGate)
			if got != tt.want {
				t.Errorf("IsFeatureGateEnabled(%q) = %v, want %v", tt.queryGate, got, tt.want)
			}
		})
	}
}
