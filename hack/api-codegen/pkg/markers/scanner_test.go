package markers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkerScanner_DiscoverCRDTypes(t *testing.T) {
	scanner, err := NewScanner([]string{}, false)
	if err != nil {
		t.Fatalf("NewScanner() failed: %v", err)
	}

	// Verify that discovery populated crdTypes (implementation test)
	if len(scanner.crdTypes) == 0 {
		t.Fatal("crdTypes is empty - discoverCRDTypes() should populate from scheme")
	}

	// Verify ALL discovered types have valid GVK format (tests discovery logic)
	for kind, gvk := range scanner.crdTypes {
		// Every discovered type should have a non-empty kind and GVK
		if kind == "" {
			t.Error("discovered type with empty kind")
		}
		if gvk == "" {
			t.Errorf("discovered type %q has empty GVK", kind)
		}

		// GVK format should be: group/version.kind (e.g., "seaplane.io/v1alpha1.Cluster")
		parts := strings.Split(gvk, "/")
		if len(parts) != 2 {
			t.Errorf("GVK format invalid for %q: %s (expected group/version.kind)", kind, gvk)
		}

		versionKind := parts[1]
		if !strings.Contains(versionKind, ".") {
			t.Errorf("GVK missing version.kind separator for %q: %s", kind, gvk)
		}

		kindPart := strings.Split(versionKind, ".")[1]
		if kindPart != kind {
			t.Errorf("GVK kind part %q doesn't match Kind %q", kindPart, kind)
		}
	}
}

func TestMarkerScanner_InferOwnerFromPassthrough(t *testing.T) {
	scanner, err := NewScanner([]string{}, false)
	if err != nil {
		t.Fatalf("NewScanner() failed: %v", err)
	}

	if len(scanner.crdTypes) == 0 {
		t.Skip("no CRD types discovered - cannot test passthrough inference")
	}

	tests := []struct {
		name            string
		passthroughType string
		expectMatch     bool
		validateResult  func(t *testing.T, kind, gvk string, crdTypes map[string]string)
	}{
		{
			name:            "passthrough containing a discovered CRD kind name should match",
			passthroughType: "SomeTypePassthrough",
			expectMatch:     false, // Only matches if type name contains a real discovered kind
			validateResult: func(t *testing.T, kind, gvk string, crdTypes map[string]string) {
				// If it matched, verify the kind is actually in discovered types
				if kind != "" {
					if _, found := crdTypes[kind]; !found {
						t.Errorf("matched kind %q not in discovered CRD types", kind)
					}
					// Verify GVK is the correct mapping
					if gvk != crdTypes[kind] {
						t.Errorf("GVK for %q mismatch: expected %q, got %q", kind, crdTypes[kind], gvk)
					}
				}
			},
		},
		{
			name:            "unknown passthrough not containing any CRD kind should not match",
			passthroughType: "UnknownThingPassthrough",
			expectMatch:     false,
			validateResult: func(t *testing.T, kind, gvk string, crdTypes map[string]string) {
				if kind != "" {
					t.Errorf("unknown type matched to kind %q; inferOwnerFromPassthrough should only match known CRD kinds", kind)
				}
				if gvk != "" {
					t.Errorf("unknown type returned GVK %q; expected empty", gvk)
				}
			},
		},
	}

	// Generate test case for each discovered CRD type
	for discoveredKind := range scanner.crdTypes {
		// Create a passthrough type name containing the discovered kind
		passthroughName := discoveredKind + "Passthrough"

		tests = append(tests, struct {
			name            string
			passthroughType string
			expectMatch     bool
			validateResult  func(t *testing.T, kind, gvk string, crdTypes map[string]string)
		}{
			name:            fmt.Sprintf("%s should match discovered kind %q", passthroughName, discoveredKind),
			passthroughType: passthroughName,
			expectMatch:     true,
			validateResult: func(t *testing.T, kind, gvk string, crdTypes map[string]string) {
				if kind != discoveredKind {
					t.Errorf("expected kind %q, got %q", discoveredKind, kind)
				}
				expectedGVK := crdTypes[discoveredKind]
				if gvk != expectedGVK {
					t.Errorf("expected GVK %q, got %q", expectedGVK, gvk)
				}
			},
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, gvk := scanner.inferOwnerFromPassthrough(tt.passthroughType)
			matched := kind != ""

			if matched != tt.expectMatch {
				t.Errorf("match expectation failed: expected match=%v, but got kind=%q", tt.expectMatch, kind)
			}

			if tt.validateResult != nil {
				tt.validateResult(t, kind, gvk, scanner.crdTypes)
			}
		})
	}
}

func TestMarkerScanner_ScanPopulatesTypedRegistry(t *testing.T) {
	// Integration test: Verify that scanning populates TypedFieldRegistry with proper owner context
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() failed: %v", err)
	}

	apiDir := filepath.Join(wd, "../../../../api/v1alpha1")
	scanner, err := NewScanner([]string{apiDir}, false)
	if err != nil {
		t.Fatalf("NewScanner(%q) failed: %v", apiDir, err)
	}

	if err := scanner.Scan(); err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}

	// Verify that registry was populated
	if len(scanner.TypedRegistry) == 0 {
		t.Fatal("TypedRegistry is empty after scan - no fields were found")
	}

	// Verify Cluster CRD type fields are present
	clusterFields, hasCluster := scanner.TypedRegistry["Cluster"]
	if !hasCluster {
		t.Fatal("Cluster CRD type not found in registry")
	}

	if len(clusterFields) == 0 {
		t.Error("Cluster has no fields in registry")
		return
	}

	// Verify all Cluster fields have correct owner context
	for path, field := range clusterFields {
		if field.OwnerType != "Cluster" {
			t.Errorf("Cluster field %q has OwnerType %q, want Cluster", path, field.OwnerType)
		}
		if !strings.Contains(field.OwnerGVK, "Cluster") {
			t.Errorf("Cluster field %q has OwnerGVK %q, should contain Cluster", path, field.OwnerGVK)
		}
	}
}

func TestMarkerScanner_ResettableMarker(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() failed: %v", err)
	}

	scanner, err := NewScanner([]string{filepath.Join(wd, "../../../../api/v1alpha1")}, false)
	if err != nil {
		t.Fatalf("NewScanner() failed: %v", err)
	}
	if err := scanner.Scan(); err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}

	nodePoolFields, found := scanner.TypedRegistry["NodePool"]
	if !found {
		t.Fatal("NodePool owner not found in registry")
	}
	if meta := nodePoolFields["spec.labels"]; !meta.Resettable {
		t.Fatalf("spec.labels Resettable = false, want true: %#v", meta)
	}
	if meta := nodePoolFields["spec.nodePool.taints"]; !meta.Resettable {
		t.Fatalf("spec.nodePool.taints Resettable = false, want true: %#v", meta)
	}
}

func TestMarkerScanner_UpstreamReducedTypesDetected(t *testing.T) {
	// Integration test: verify upstream-reduced object markers are detected
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() failed: %v", err)
	}

	apiDir := filepath.Join(wd, "../../../../api/v1alpha1")
	scanner, err := NewScanner([]string{apiDir}, false)
	if err != nil {
		t.Fatalf("NewScanner(%q) failed: %v", apiDir, err)
	}

	if err := scanner.Scan(); err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}

	// Verify that upstream-reduced types were found
	if len(scanner.upstreamReducedTypes) == 0 {
		t.Error("No upstream-reduced types were detected")
		return
	}

	// Check for expected upstream-reduced types
	expectedTypes := map[string]string{
		"ClusterConfiguration": "hypershiftv1beta1.ClusterConfiguration",
		"MachineConfigSpec":    "hypershiftv1beta1.MachineConfigSpec",
	}

	for localType, expectedUpstream := range expectedTypes {
		mapping, ok := scanner.upstreamReducedTypes[localType]
		if !ok {
			t.Errorf("expected upstream-reduced type %q not found", localType)
			continue
		}

		if mapping.LocalType != localType {
			t.Errorf("%q LocalType = %q, want %q", localType, mapping.LocalType, localType)
		}

		if !strings.Contains(mapping.UpstreamType, expectedUpstream) {
			t.Errorf("%q UpstreamType = %q, should contain %q", localType, mapping.UpstreamType, expectedUpstream)
		}
	}
}

func TestMarkerScanner_EmbeddedUpstreamTypesTracked(t *testing.T) {
	// Integration test: verify that embedded upstream types are tracked for synthetic path generation
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() failed: %v", err)
	}

	apiDir := filepath.Join(wd, "../../../../api/v1alpha1")
	scanner, err := NewScanner([]string{apiDir}, false)
	if err != nil {
		t.Fatalf("NewScanner(%q) failed: %v", apiDir, err)
	}

	if err := scanner.Scan(); err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}

	// Verify that embeddings were tracked
	if len(scanner.embeddedUpstreamTypes) == 0 {
		t.Error("No embedded upstream types were tracked")
		return
	}

	// Check for MachineConfigSpec embedding
	foundMachineConfig := false
	for _, embedding := range scanner.embeddedUpstreamTypes {
		if embedding.LocalType == "MachineConfigSpec" {
			foundMachineConfig = true
			if !strings.Contains(embedding.ContainerFieldPath, "machineConfig") {
				t.Errorf("MachineConfigSpec embedding has wrong path: %q", embedding.ContainerFieldPath)
			}
			if !strings.Contains(embedding.UpstreamType, "MachineConfigSpec") {
				t.Errorf("MachineConfigSpec embedding has wrong upstream type: %q", embedding.UpstreamType)
			}
			break
		}
	}

	if !foundMachineConfig {
		t.Error("MachineConfigSpec embedding not found")
	}
}

func TestMarkerScanner_PerCRDTypeOwnerContext(t *testing.T) {
	// Integration test: verify that different CRD types have separate field registries with proper owner context
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() failed: %v", err)
	}

	apiDir := filepath.Join(wd, "../../../../api/v1alpha1")
	scanner, err := NewScanner([]string{apiDir}, false)
	if err != nil {
		t.Fatalf("NewScanner(%q) failed: %v", apiDir, err)
	}

	if err := scanner.Scan(); err != nil {
		t.Fatalf("Scan() failed: %v", err)
	}

	clusterFields, hasCluster := scanner.TypedRegistry["Cluster"]
	nodepoolFields, hasNodePool := scanner.TypedRegistry["NodePool"]

	if !hasCluster || !hasNodePool {
		t.Skip("Both Cluster and NodePool types required for this test")
	}

	// Verify both have fields
	if len(clusterFields) == 0 {
		t.Error("Cluster should have fields")
	}
	if len(nodepoolFields) == 0 {
		t.Error("NodePool should have fields")
	}

	// Verify owner context is correct for each type - this is the key test for TypedFieldRegistry
	clusterOwnerMismatch := false
	for path, field := range clusterFields {
		if field.OwnerType != "Cluster" {
			t.Errorf("Cluster field %q has OwnerType %q, not Cluster", path, field.OwnerType)
			clusterOwnerMismatch = true
			break
		}
	}

	nodepoolOwnerMismatch := false
	for path, field := range nodepoolFields {
		if field.OwnerType != "NodePool" {
			t.Errorf("NodePool field %q has OwnerType %q, not NodePool", path, field.OwnerType)
			nodepoolOwnerMismatch = true
			break
		}
	}

	if !clusterOwnerMismatch && !nodepoolOwnerMismatch {
		t.Log("Both Cluster and NodePool fields have correct owner context")
	}

	// Verify the TypedFieldRegistry structure preserves per-type separation
	if len(scanner.TypedRegistry) < 2 {
		t.Errorf("TypedFieldRegistry should have at least 2 owner types, got %d", len(scanner.TypedRegistry))
	}
}
