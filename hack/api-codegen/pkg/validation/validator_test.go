package validation

import (
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
)

func TestValidator_Validate_WriteMode(t *testing.T) {
	tests := []struct {
		name        string
		fieldPath   string
		writeMode   registry.WriteMode
		operation   Operation
		existsInOld bool
		wantErr     bool
		errContains string
	}{
		{
			name:      "mutable field on create - allowed",
			fieldPath: "spec.displayName",
			writeMode: registry.Mutable,
			operation: OperationCreate,
			wantErr:   false,
		},
		{
			name:      "mutable field on update - allowed",
			fieldPath: "spec.displayName",
			writeMode: registry.Mutable,
			operation: OperationUpdate,
			wantErr:   false,
		},
		{
			name:      "immutable field on create - allowed",
			fieldPath: "spec.name",
			writeMode: registry.Immutable,
			operation: OperationCreate,
			wantErr:   false,
		},
		{
			name:        "immutable field on update (field exists) - blocked",
			fieldPath:   "spec.name",
			writeMode:   registry.Immutable,
			operation:   OperationUpdate,
			existsInOld: true,
			wantErr:     true,
			errContains: "immutable and cannot be changed",
		},
		{
			name:        "immutable field on update (field new) - allowed",
			fieldPath:   "spec.name",
			writeMode:   registry.Immutable,
			operation:   OperationUpdate,
			existsInOld: false,
			wantErr:     false,
		},
		{
			name:        "service-set field on create - blocked",
			fieldPath:   "spec.accountId",
			writeMode:   registry.ServiceSet,
			operation:   OperationCreate,
			wantErr:     true,
			errContains: "platform-managed",
		},
		{
			name:        "service-set field on update - blocked",
			fieldPath:   "spec.accountId",
			writeMode:   registry.ServiceSet,
			operation:   OperationUpdate,
			wantErr:     true,
			errContains: "platform-managed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test validator with a single field for Cluster resource type
			v := &Validator{
				registry: registry.TypedFieldRegistry{
					"Cluster": {
						tt.fieldPath: {
							FieldPath: tt.fieldPath,
							WriteMode: tt.writeMode,
							OwnerType: "Cluster",
						},
					},
				},
			}

			req := &Request{
				Operation:    tt.operation,
				ResourceType: "Cluster",
				Fields:       map[string]interface{}{tt.fieldPath: "test-value"},
				FeatureSet:   featuregate.Default,
			}

			if tt.existsInOld {
				req.ExistingFields = map[string]interface{}{tt.fieldPath: "old-value"}
			}

			err := v.Validate(req)

			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("Validate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestValidator_Validate_FeatureGates(t *testing.T) {
	tests := []struct {
		name        string
		fieldPath   string
		featureGate string
		featureSet  featuregate.FeatureSet
		wantErr     bool
		errContains string
	}{
		{
			name:        "gated field with Default feature set - blocked",
			fieldPath:   "spec.tags",
			featureGate: "SeaplaneAutoScaling",
			featureSet:  featuregate.Default,
			wantErr:     true,
			errContains: "requires feature gate SeaplaneAutoScaling",
		},
		{
			name:        "gated field with TechPreview feature set - allowed",
			fieldPath:   "spec.tags",
			featureGate: "SeaplaneAutoScaling",
			featureSet:  featuregate.TechPreviewNoUpgrade,
			wantErr:     false,
		},
		{
			name:        "gated field with DevPreview feature set - allowed",
			fieldPath:   "spec.tags",
			featureGate: "SeaplaneAutoScaling",
			featureSet:  featuregate.DevPreviewNoUpgrade,
			wantErr:     false,
		},
		{
			name:       "non-gated field with Default feature set - allowed",
			fieldPath:  "spec.displayName",
			featureSet: featuregate.Default,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &Validator{
				registry: registry.TypedFieldRegistry{
					"Cluster": {
						tt.fieldPath: {
							FieldPath:   tt.fieldPath,
							WriteMode:   registry.Mutable,
							FeatureGate: tt.featureGate,
							OwnerType:   "Cluster",
						},
					},
				},
			}

			req := &Request{
				Operation:    OperationCreate,
				ResourceType: "Cluster",
				Fields:       map[string]interface{}{tt.fieldPath: "test-value"},
				FeatureSet:   tt.featureSet,
			}

			err := v.Validate(req)

			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("Validate() error = %v, want error containing %q", err, tt.errContains)
			}
		})
	}
}

func TestValidator_Validate_MultipleErrors(t *testing.T) {
	v := &Validator{
		registry: registry.TypedFieldRegistry{
			"Cluster": {
				"spec.accountId": {
					FieldPath: "spec.accountId",
					WriteMode: registry.ServiceSet,
					OwnerType: "Cluster",
				},
				"spec.hostedClusterName": {
					FieldPath: "spec.hostedClusterName",
					WriteMode: registry.ServiceSet,
					OwnerType: "Cluster",
				},
				"spec.tags": {
					FieldPath:   "spec.tags",
					WriteMode:   registry.Mutable,
					FeatureGate: "SeaplaneAutoScaling",
					OwnerType:   "Cluster",
				},
			},
		},
	}

	req := &Request{
		Operation:    OperationCreate,
		ResourceType: "Cluster",
		Fields: map[string]interface{}{
			"spec.accountId":         "test-account",
			"spec.hostedClusterName": "hc-test-id",
			"spec.tags":              map[string]string{"key": "value"},
		},
		FeatureSet: featuregate.Default,
	}

	err := v.Validate(req)
	if err == nil {
		t.Fatal("Validate() expected error, got nil")
	}

	errStr := err.Error()

	// Should have all three errors
	if !strings.Contains(errStr, "spec.accountId") {
		t.Error("expected error for spec.accountId")
	}
	if !strings.Contains(errStr, "spec.hostedClusterName") {
		t.Error("expected error for spec.hostedClusterName")
	}
	if !strings.Contains(errStr, "spec.tags") {
		t.Error("expected error for spec.tags")
	}
	if !strings.Contains(errStr, "service-set") {
		t.Error("expected error mentioning service-set")
	}
	if !strings.Contains(errStr, "feature gate") {
		t.Error("expected error mentioning feature gate")
	}
}

func TestValidator_ValidateFieldAccess(t *testing.T) {
	v := &Validator{
		registry: registry.TypedFieldRegistry{
			"Cluster": {
				"spec.tags": {
					FieldPath:   "spec.tags",
					WriteMode:   registry.Mutable,
					FeatureGate: "SeaplaneAutoScaling",
					OwnerType:   "Cluster",
				},
				"spec.displayName": {
					FieldPath: "spec.displayName",
					WriteMode: registry.Mutable,
					OwnerType: "Cluster",
				},
			},
		},
	}

	tests := []struct {
		name       string
		fieldPath  string
		featureSet featuregate.FeatureSet
		wantErr    bool
	}{
		{
			name:       "gated field with insufficient feature set",
			fieldPath:  "spec.tags",
			featureSet: featuregate.Default,
			wantErr:    true,
		},
		{
			name:       "gated field with sufficient feature set",
			fieldPath:  "spec.tags",
			featureSet: featuregate.TechPreviewNoUpgrade,
			wantErr:    false,
		},
		{
			name:       "non-gated field",
			fieldPath:  "spec.displayName",
			featureSet: featuregate.Default,
			wantErr:    false,
		},
		{
			name:       "unknown field - allowed",
			fieldPath:  "spec.unknown",
			featureSet: featuregate.Default,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateFieldAccess("Cluster", tt.fieldPath, tt.featureSet)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateFieldAccess() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidator_GetFieldMetadata(t *testing.T) {
	v := &Validator{
		registry: registry.TypedFieldRegistry{
			"Cluster": {
				"spec.name": {
					FieldPath: "spec.name",
					WriteMode: registry.Immutable,
					OwnerType: "Cluster",
				},
			},
		},
	}

	// Field exists
	meta, exists := v.GetFieldMetadata("Cluster", "spec.name")
	if !exists {
		t.Error("expected field to exist")
	}
	if meta.WriteMode != registry.Immutable {
		t.Errorf("expected WriteMode=Immutable, got %v", meta.WriteMode)
	}

	// Field doesn't exist
	_, exists = v.GetFieldMetadata("Cluster", "spec.unknown")
	if exists {
		t.Error("expected field to not exist")
	}
}

func TestNewValidator_UsesGeneratedRegistry(t *testing.T) {
	v := NewValidator()
	if v == nil {
		t.Fatal("NewValidator() returned nil")
	}

	// Verify it's using the real generated registry by checking a known field
	// This tests that the integration with pkg/registry works
	meta, exists := v.GetFieldMetadata("Cluster", "spec.properties")
	if !exists {
		t.Error("expected spec.properties to exist in generated registry for Cluster")
	}
	if meta.WriteMode != registry.Mutable {
		t.Errorf("expected spec.properties to be Mutable, got %v", meta.WriteMode)
	}
}

func TestValidationErrors_Error(t *testing.T) {
	tests := []struct {
		name   string
		errors ValidationErrors
		want   string
	}{
		{
			name:   "empty errors",
			errors: ValidationErrors{},
			want:   "no validation errors",
		},
		{
			name: "single error",
			errors: ValidationErrors{
				{FieldPath: "spec.name", Reason: "is required"},
			},
			want: "field spec.name: is required",
		},
		{
			name: "multiple errors",
			errors: ValidationErrors{
				{FieldPath: "spec.name", Reason: "is required"},
				{FieldPath: "spec.region", Reason: "is invalid"},
			},
			want: "validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.errors.Error()
			if !strings.Contains(got, tt.want) {
				t.Errorf("Error() = %q, want containing %q", got, tt.want)
			}
		})
	}
}
