package validation

import (
	"fmt"
	"strings"

	"github.com/openshift-online/rosa-seaplane-api/featuregate"
	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
)

// Operation represents the type of API operation
type Operation string

const (
	// OperationCreate is for creating new resources
	OperationCreate Operation = "create"
	// OperationUpdate is for updating existing resources
	OperationUpdate Operation = "update"
)

// Request represents an API request to validate
type Request struct {
	// Operation is the type of operation (create or update)
	Operation Operation

	// ResourceType is the CRD Kind being validated (e.g., "Cluster", "NodePool")
	ResourceType string

	// Fields maps field paths to their values (for validation we only need the paths)
	Fields map[string]interface{}

	// FeatureSet is the customer's feature set (Default, TechPreview, DevPreview)
	FeatureSet featuregate.FeatureSet

	// ExistingFields contains field paths from the existing resource (for update operations)
	// Used to detect which fields are being changed
	ExistingFields map[string]interface{}

	// EnabledGates is the list of feature gates enabled for this customer
	// Used to determine effective write-mode when FeatureGateAwareWriteModes is set
	EnabledGates []string
}

// IsFeatureGateEnabled returns true if the given feature gate is enabled for this request
func (r *Request) IsFeatureGateEnabled(gateName string) bool {
	for _, gate := range r.EnabledGates {
		if gate == gateName {
			return true
		}
	}
	return false
}

// ValidationError represents a validation failure
type ValidationError struct {
	FieldPath string
	Reason    string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("field %s: %s", e.FieldPath, e.Reason)
}

// ValidationErrors is a collection of validation errors
type ValidationErrors []*ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}

	var sb strings.Builder
	sb.WriteString("validation failed:\n")
	for _, err := range e {
		sb.WriteString("  ")
		sb.WriteString(err.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

// Validator validates API requests against field metadata
type Validator struct {
	registry registry.TypedFieldRegistry
}

// NewValidator creates a validator using the generated field registry
func NewValidator() *Validator {
	return &Validator{
		registry: registry.FieldRegistry,
	}
}

// Validate checks a request against field metadata rules
func (v *Validator) Validate(req *Request) error {
	var errors ValidationErrors

	// Get fields for this resource type
	fieldsForType, exists := v.registry[req.ResourceType]
	if !exists {
		// Resource type not in registry - allow all fields
		return nil
	}

	for fieldPath := range req.Fields {
		meta, exists := fieldsForType[fieldPath]
		if !exists {
			// Field not in registry - might be a field without markers (allowed)
			continue
		}

		// Check feature gate access
		if meta.FeatureGate != "" {
			if !featuregate.IsGateEnabled(meta.FeatureGate, req.FeatureSet) {
				errors = append(errors, &ValidationError{
					FieldPath: fieldPath,
					Reason:    fmt.Sprintf("requires feature gate %s which is not enabled in %s feature set", meta.FeatureGate, req.FeatureSet),
				})
				continue
			}
		}

		// Check write mode
		if err := v.validateWriteMode(fieldPath, meta, req); err != nil {
			errors = append(errors, err)
		}
	}

	if len(errors) > 0 {
		return errors
	}

	return nil
}

// validateWriteMode checks if a field can be set based on its write mode
func (v *Validator) validateWriteMode(fieldPath string, meta registry.FieldMeta, req *Request) *ValidationError {
	// Determine effective write-mode based on feature-gate-aware overrides
	effectiveMode := meta.WriteMode // Default fallback

	if len(meta.FeatureGateAwareWriteModes) > 0 {
		// Check for specific gate match first (takes precedence)
		matched := false
		for _, override := range meta.FeatureGateAwareWriteModes {
			if override.FeatureGate != "" && req.IsFeatureGateEnabled(override.FeatureGate) {
				effectiveMode = override.WriteMode
				matched = true
				break // First specific match wins
			}
		}

		// If no specific match, check for default override (empty gate)
		if !matched {
			for _, override := range meta.FeatureGateAwareWriteModes {
				if override.FeatureGate == "" {
					effectiveMode = override.WriteMode
					break
				}
			}
		}
	}

	// Enforce the effective mode
	switch effectiveMode {
	case registry.ServiceSet:
		// Service-set fields cannot be set by customers at all
		return &ValidationError{
			FieldPath: fieldPath,
			Reason:    "field is platform-managed (service-set) and cannot be set by customers",
		}

	case registry.Immutable:
		// Immutable fields can be set on create but not changed on update
		if req.Operation == OperationUpdate {
			// Check if the field is actually being changed
			if req.ExistingFields != nil {
				_, existsInOld := req.ExistingFields[fieldPath]
				if existsInOld {
					return &ValidationError{
						FieldPath: fieldPath,
						Reason:    "field is immutable and cannot be changed after creation",
					}
				}
			}
			// If field doesn't exist in old resource, this is adding a new field on update
			// which is allowed for immutable fields (they can be set once)
		}
		// On create, immutable fields can be set
		return nil

	case registry.Mutable:
		// Mutable fields can always be set
		return nil

	default:
		// Unknown write mode - be permissive
		return nil
	}
}

// ValidateFieldAccess checks if a customer can access a specific field
func (v *Validator) ValidateFieldAccess(resourceType string, fieldPath string, featureSet featuregate.FeatureSet) error {
	fieldsForType, exists := v.registry[resourceType]
	if !exists {
		// Resource type not in registry - allowed
		return nil
	}

	meta, exists := fieldsForType[fieldPath]
	if !exists {
		// Field not in registry - allowed
		return nil
	}

	// Check feature gate
	if meta.FeatureGate != "" {
		if !featuregate.IsGateEnabled(meta.FeatureGate, featureSet) {
			return fmt.Errorf("field %s requires feature gate %s which is not enabled in %s feature set",
				fieldPath, meta.FeatureGate, featureSet)
		}
	}

	return nil
}

// GetFieldMetadata returns metadata for a field path in a resource type
func (v *Validator) GetFieldMetadata(resourceType string, fieldPath string) (registry.FieldMeta, bool) {
	fieldsForType, exists := v.registry[resourceType]
	if !exists {
		return registry.FieldMeta{}, false
	}

	meta, exists := fieldsForType[fieldPath]
	return meta, exists
}
