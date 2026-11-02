package markers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	// Marker patterns
	openapiGenPattern                = regexp.MustCompile(`\+k8s:openapi-gen=false`)
	writeModePattern                 = regexp.MustCompile(`\+seaplane:write-mode=(mutable|immutable|service-set)`)
	resettablePattern                = regexp.MustCompile(`\+seaplane:resettable=(true|false)`)
	featureGatePattern               = regexp.MustCompile(`\+openshift:enable:FeatureGate=(\w+)`)
	featureGateAwareWriteModePattern = regexp.MustCompile(`\+seaplane:validation:FeatureGateAwareWriteMode:featureGate="([^"]*)",writeMode="(mutable|immutable|service-set)"`)
	upstreamReducedObjectPattern     = regexp.MustCompile(`\+seaplane:upstream-reduced-object=([^\s]+)`)
)

// NewScanner creates a new marker scanner
func NewScanner(inputDirs []string, verbose bool) (*MarkerScanner, error) {
	scanner := &MarkerScanner{
		InputDirs:             inputDirs,
		TypedRegistry:         make(TypedFieldRegistry),
		crdTypes:              make(map[string]string),
		typeCache:             make(map[string]*ast.StructType),
		upstreamReducedTypes:  make(map[string]UpstreamReducedMapping),
		embeddedUpstreamTypes: make(map[string]EmbeddedUpstreamType),
		verbose:               verbose,
	}

	// Initialize scheme and discover CRD types
	if err := scanner.discoverCRDTypes(); err != nil {
		return nil, fmt.Errorf("discovering CRD types: %w", err)
	}

	return scanner, nil
}

// discoverCRDTypes initializes the scheme and populates crdTypes map
// Maps Kind name (e.g., "Cluster") to full GVK string (e.g., "seaplane.io/v1alpha1.Cluster")
func (s *MarkerScanner) discoverCRDTypes() error {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("adding v1alpha1 to scheme: %w", err)
	}

	// Discover all registered CRD types
	for gvk := range scheme.AllKnownTypes() {
		// Skip List types and empty kinds
		if strings.HasSuffix(gvk.Kind, "List") || gvk.Kind == "" {
			continue
		}

		// Store Kind → GVK string mapping (e.g., "Cluster" → "seaplane.io/v1alpha1.Cluster")
		gvkStr := fmt.Sprintf("%s/%s.%s", gvk.Group, gvk.Version, gvk.Kind)
		s.crdTypes[gvk.Kind] = gvkStr
		s.logf("discovered CRD type: %s → %s", gvk.Kind, gvkStr)
	}

	return nil
}

// inferOwnerFromPassthrough infers the owner CRD and its GVK from a passthrough type name
// Parses passthrough naming convention to find the owner CRD Kind
// Examples: "HostedClusterSpecPassthrough" embeds in Cluster.Spec.HostedCluster
//
//	"NodePoolSpecPassthrough" embeds in NodePool.Spec.NodePool
//
// Uses simple heuristics: match against known CRD types by substring, selecting the most specific match
func (s *MarkerScanner) inferOwnerFromPassthrough(passthroughType string) (ownerKind string, gvk string) {
	// Collect and sort kinds for deterministic, specific matching
	var kinds []string
	for kind := range s.crdTypes {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)

	// Match against known CRD types, preferring the longest (most specific) match
	var bestMatch string
	for _, kind := range kinds {
		// For "HostedClusterSpecPassthrough": "Cluster" matches via substring in "HostedCluster"
		// For "NodePoolSpecPassthrough": "NodePool" matches
		if strings.Contains(passthroughType, kind) && len(kind) > len(bestMatch) {
			bestMatch = kind
		}
	}

	if bestMatch != "" {
		return bestMatch, s.crdTypes[bestMatch]
	}
	return "", ""
}

// logf writes a formatted log line to stderr when verbose mode is enabled.
func (s *MarkerScanner) logf(format string, args ...any) {
	if s.verbose {
		fmt.Fprintf(os.Stderr, "[scanner] "+format+"\n", args...)
	}
}

// Scan walks the input directories and extracts marker metadata
func (s *MarkerScanner) Scan() error {
	for _, dir := range s.InputDirs {
		s.logf("scanning directory: %s", dir)
		if err := s.scanDir(dir); err != nil {
			return fmt.Errorf("scanning directory %s: %w", dir, err)
		}
	}
	// Count total fields across all owner types
	totalFields := 0
	for _, fields := range s.TypedRegistry {
		totalFields += len(fields)
	}
	s.logf("scan complete: %d fields in typed registry", totalFields)
	return nil
}

// scanDir processes all Go files in a directory
func (s *MarkerScanner) scanDir(dir string) error {
	fset := token.NewFileSet()

	//nolint:staticcheck // ParseDir is sufficient for our use case of scanning single directories
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		// Skip test files and generated files
		name := fi.Name()
		return !strings.HasSuffix(name, "_test.go") &&
			(!strings.HasPrefix(name, "zz_generated") || name == "zz_generated.passthrough.go")
	}, parser.ParseComments)

	if err != nil {
		return fmt.Errorf("parsing directory: %w", err)
	}

	// Scope the type cache to this directory so same-named types in
	// different packages don't collide.
	dirCache := make(map[string]*ast.StructType)

	// First pass: cache all struct types across all files in this package
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}

				for _, spec := range gd.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					structType, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					dirCache[typeSpec.Name.Name] = structType

					// Extract upstream-reduced-object marker from the GenDecl comment
					if gd.Doc != nil {
						docText := gd.Doc.Text()
						if matches := upstreamReducedObjectPattern.FindStringSubmatch(docText); len(matches) > 1 {
							s.upstreamReducedTypes[typeSpec.Name.Name] = UpstreamReducedMapping{
								LocalType:    typeSpec.Name.Name,
								UpstreamType: matches[1],
							}
							s.logf("  found upstream-reduced type: %s → %s", typeSpec.Name.Name, matches[1])
						}
					}
				}
			}
		}
	}

	// Install this directory's cache for nested-type resolution
	s.typeCache = dirCache
	s.logf("  cached %d struct types", len(dirCache))

	// Second pass: process root types once with the full cache available
	for typeName, structType := range dirCache {
		if s.isRootType(typeName) {
			visited := make(map[string]bool)
			visited[typeName] = true
			prefix := rootTypePrefix(typeName)
			// Get GVK for this CRD type, or infer from passthrough type name
			gvk := s.crdTypes[typeName]
			ownerKind := typeName
			if gvk == "" && strings.HasSuffix(typeName, "Passthrough") {
				// Infer owner from passthrough type name
				ownerKind, gvk = s.inferOwnerFromPassthrough(typeName)
			}
			s.logf("  root type: %s (prefix=%q, gvk=%q)", typeName, prefix, gvk)
			s.processStruct(typeName, structType, prefix, visited, ownerKind, gvk)
		}
	}

	return nil
}

// rootTypePrefix returns a dotted prefix that namespaces registry keys by root
// type, so fields with the same JSON name in different root types (e.g.
// HostedClusterSpecPassthrough.PausedUntil vs NodePoolSpecPassthrough.PausedUntil)
// don't collide in the flat FieldRegistry map. The prefixes mirror
// conversion/generator.go buildFieldPath so consumers can look up entries
// with the same key they construct.
func rootTypePrefix(typeName string) string {
	if strings.HasSuffix(typeName, "Passthrough") {
		if strings.HasPrefix(typeName, "HostedCluster") {
			return "spec.hostedCluster"
		}
		if strings.HasPrefix(typeName, "NodePool") {
			return "spec.nodePool"
		}
	}
	return ""
}

// isRootType returns true for types that are entry points for marker scanning.
// Uses the scheme to determine if a type is a registered CRD (no hardcoded names).
// Passthrough types are also root types since they carry curated markers.
// For testing and special cases, types that don't end with Spec, Status, List, or ClusterReference are root types.
func (s *MarkerScanner) isRootType(typeName string) bool {
	// Check if type is a registered CRD in the scheme
	if s.crdTypes[typeName] != "" {
		return true
	}

	// Passthrough types are scanned as roots because they carry curated markers
	if strings.HasSuffix(typeName, "Passthrough") {
		return true
	}

	// Upstream-reduced types are root types regardless of naming convention,
	// because their fields carry explicit +seaplane:write-mode markers that
	// generateSyntheticPaths must be able to read from byOwner.
	if _, ok := s.upstreamReducedTypes[typeName]; ok {
		return true
	}

	// Fallback for testing and special types: use the original heuristic
	// This handles types that are not registered CRDs but should still be scanned as roots
	return !strings.HasSuffix(typeName, "Spec") &&
		!strings.HasSuffix(typeName, "Status") &&
		!strings.HasSuffix(typeName, "List") &&
		typeName != "ClusterReference"
}

// processStruct walks struct fields and extracts markers
// ownerKind is the Kind of the CRD that owns these fields (e.g., "Cluster", "NodePool")
// ownerGVK is the full GVK string (e.g., "seaplane.io/v1alpha1.Cluster")
func (s *MarkerScanner) processStruct(_ string, structType *ast.StructType, parentPath string, visited map[string]bool, ownerKind string, ownerGVK string) {
	for _, field := range structType.Fields.List {
		s.processField(field, parentPath, visited, ownerKind, ownerGVK)
	}
}

// processField extracts markers from a single field
func (s *MarkerScanner) processField(field *ast.Field, parentPath string, visited map[string]bool, ownerKind string, ownerGVK string) {
	// Get JSON tag to determine field path
	jsonName := getJSONName(field)
	if jsonName == "" || jsonName == "-" {
		return
	}

	// Build full field path
	var fieldPath string
	if parentPath == "" {
		fieldPath = jsonName
	} else {
		fieldPath = parentPath + "." + jsonName
	}

	// Check if this field's type is an upstream-reduced type
	// If so, mark it for synthetic path generation
	fieldTypeName := s.extractTypeName(field.Type)
	isUpstreamReduced := false
	var localType string

	if fieldTypeName != "" {
		localType = s.getLocalTypeForUpstream(fieldTypeName)
		if localType != "" {
			isUpstreamReduced = true
			// Store the embedding info for synthetic path generation (with deduplication via map key)
			key := ownerKind + "." + fieldPath + "." + localType
			if _, exists := s.embeddedUpstreamTypes[key]; !exists {
				s.embeddedUpstreamTypes[key] = EmbeddedUpstreamType{
					ContainerFieldPath: fieldPath,
					LocalType:          localType,
					UpstreamType:       fieldTypeName,
					CRDOwner:           ownerKind,
					CRDOwnerGVK:        ownerGVK,
				}
				s.logf("      (recording embedding for synthetic path generation: %s → %s)", fieldPath, localType)
			}
		}
	}

	// Extract markers for the field
	meta := s.extractMarkers(field, fieldPath)
	if meta != nil && !isUpstreamReduced {
		// Only add non-upstream-reduced fields to registry
		// Upstream-reduced fields will be synth added via synthetic paths
		meta.OwnerType = ownerKind
		meta.OwnerGVK = ownerGVK
		s.logf("    field: %s  owner=%s  write-mode=%s  hidden=%v  resettable=%v  gate=%s", fieldPath, ownerKind, meta.WriteMode, meta.Hidden, meta.Resettable, meta.FeatureGate)

		// Add to typed registry
		if s.TypedRegistry[ownerKind] == nil {
			s.TypedRegistry[ownerKind] = make(map[string]FieldMeta)
		}
		s.TypedRegistry[ownerKind][fieldPath] = *meta
	} else if meta != nil && isUpstreamReduced {
		// For upstream-reduced container fields, just log the markers but don't add to registry
		meta.OwnerType = ownerKind
		meta.OwnerGVK = ownerGVK
		s.logf("    field: %s (container for upstream-reduced type) owner=%s write-mode=%s", fieldPath, ownerKind, meta.WriteMode)
		// Container field markers will be reflected in synthetic paths
	}

	// Recursively process nested structs (including upstream-reduced types for deeper embeddings)
	s.processNestedType(field.Type, fieldPath, visited, ownerKind, ownerGVK)
}

// processNestedType recursively handles nested struct types.
// visited tracks named types already being traversed to prevent infinite
// recursion on self-referential or mutually recursive structs.
// ownerKind and ownerGVK are propagated through the recursion to track ownership.
func (s *MarkerScanner) processNestedType(expr ast.Expr, fieldPath string, visited map[string]bool, ownerKind string, ownerGVK string) {
	switch t := expr.(type) {
	case *ast.StructType:
		// Inline struct — no named type to track
		s.processStruct("", t, fieldPath, visited, ownerKind, ownerGVK)
	case *ast.StarExpr:
		// Pointer to type
		s.processNestedType(t.X, fieldPath, visited, ownerKind, ownerGVK)
	case *ast.Ident:
		// Named type - skip if already visited in current path (prevents cycles)
		// But allow re-processing the same type at different field paths with different owners
		if visited[t.Name] {
			return
		}
		if structType, ok := s.typeCache[t.Name]; ok {
			// Create new visited scope for nested traversal
			// Inherits cycle prevention from parent, but allows independent processing of shared types
			nestedVisited := make(map[string]bool)
			for k, v := range visited {
				nestedVisited[k] = v
			}
			nestedVisited[t.Name] = true
			s.processStruct(t.Name, structType, fieldPath, nestedVisited, ownerKind, ownerGVK)
			// Don't delete from nestedVisited - it's a separate scope
		}
	case *ast.SelectorExpr:
		// External type (e.g., metav1.Time) - skip
	case *ast.ArrayType:
		// Array/slice - process element type
		s.processNestedType(t.Elt, fieldPath, visited, ownerKind, ownerGVK)
	case *ast.MapType:
		// Map - process value type
		s.processNestedType(t.Value, fieldPath, visited, ownerKind, ownerGVK)
	}
}

// extractMarkers parses comment markers and creates FieldMeta
func (s *MarkerScanner) extractMarkers(field *ast.Field, fieldPath string) *FieldMeta {
	if field.Doc == nil {
		return nil
	}

	comments := field.Doc.Text()

	meta := &FieldMeta{
		FieldPath: fieldPath,
	}

	// Check for openapi-gen=false (field is hidden)
	if openapiGenPattern.MatchString(comments) {
		meta.Hidden = true
	}

	// Extract write mode
	if matches := writeModePattern.FindStringSubmatch(comments); len(matches) > 1 {
		meta.WriteMode = WriteMode(matches[1])
	}

	// Extract reset support
	if matches := resettablePattern.FindStringSubmatch(comments); len(matches) > 1 {
		meta.Resettable = matches[1] == "true"
	}

	// Extract feature gate
	if matches := featureGatePattern.FindStringSubmatch(comments); len(matches) > 1 {
		meta.FeatureGate = matches[1]
	}

	// Extract feature-gate-aware write-modes
	var gatedModes []FeatureGateWriteMode
	for _, match := range featureGateAwareWriteModePattern.FindAllStringSubmatch(comments, -1) {
		featureGate := match[1] // Empty string or gate name
		mode := WriteMode(match[2])
		gatedModes = append(gatedModes, FeatureGateWriteMode{
			FeatureGate: featureGate,
			WriteMode:   mode,
		})
	}

	if len(gatedModes) > 0 {
		meta.FeatureGateAwareWriteModes = gatedModes
	}

	// Only include in registry if at least one marker was found
	if meta.Hidden || meta.WriteMode != "" || meta.Resettable || meta.FeatureGate != "" || len(meta.FeatureGateAwareWriteModes) > 0 {
		return meta
	}

	return nil
}

// getJSONName extracts the JSON field name from struct tags
func getJSONName(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}

	tag := field.Tag.Value
	// Remove backticks
	tag = strings.Trim(tag, "`")

	// Parse json tag
	jsonTag := parseStructTag(tag, "json")
	if jsonTag == "" {
		return ""
	}

	// Handle "name,omitempty" or "name" format
	parts := strings.Split(jsonTag, ",")
	return parts[0]
}

// parseStructTag extracts a specific tag value from struct tag string
func parseStructTag(tag, key string) string {
	// Simple tag parser - handles: `json:"name,omitempty" yaml:"name"`
	parts := strings.Fields(tag)
	prefix := key + `:"`

	for _, part := range parts {
		if strings.HasPrefix(part, prefix) {
			value := strings.TrimPrefix(part, prefix)
			value = strings.TrimSuffix(value, `"`)
			return value
		}
	}

	return ""
}

// Validate checks that all fields in the typed registry have required markers
func (t TypedFieldRegistry) Validate() error {
	var errors []string

	for owner, fields := range t {
		for path, meta := range fields {
			// All visible fields must have a write mode
			if !meta.Hidden && meta.WriteMode == "" {
				errors = append(errors, fmt.Sprintf("%s.%s is missing +seaplane:write-mode marker", owner, path))
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("validation failed:\n  %s", strings.Join(errors, "\n  "))
	}

	return nil
}

// generateSyntheticPaths creates full nested paths for upstream-reduced types embedded in CRDs.
// For example: if Cluster embeds ClusterConfiguration which maps to hypershiftv1beta1.ClusterConfiguration,
// this duplicates all ClusterConfiguration fields under Cluster with full paths like
// spec.hostedCluster.configuration.machineConfig.kernelArguments
func (s *MarkerScanner) generateSyntheticPaths(byOwner map[string][]templateField) {
	s.logf("  generating synthetic paths for upstream-reduced types")
	s.logf("    embedded upstream-reduced types found: %d", len(s.embeddedUpstreamTypes))

	// For each embedded upstream-reduced type, generate synthetic paths
	for _, embedding := range s.embeddedUpstreamTypes {
		s.logf("    processing embedding: %s.%s (local: %s, upstream: %s)", embedding.CRDOwner, embedding.ContainerFieldPath, embedding.LocalType, embedding.UpstreamType)

		// Get the fields from the local type
		localFields, ok := byOwner[embedding.LocalType]
		if !ok {
			s.logf("      (no fields found for local type %s)", embedding.LocalType)
			continue
		}

		// Build a map of existing field paths for deduplication
		existingPaths := make(map[string]bool)
		if crdFields, ok := byOwner[embedding.CRDOwner]; ok {
			for _, field := range crdFields {
				existingPaths[field.FieldPath] = true
			}
		}

		// Generate synthetic paths for each field in the local type
		// e.g., containerPath="spec.hostedCluster.configuration", field="machineConfig.kernelArguments" → "spec.hostedCluster.configuration.machineConfig.kernelArguments"
		for _, field := range localFields {
			syntheticPath := embedding.ContainerFieldPath + "." + field.FieldPath

			// Skip if this path already exists (e.g., when the container field itself has been processed)
			if existingPaths[syntheticPath] {
				s.logf("      synthetic (skipped, already exists): %s", syntheticPath)
				continue
			}

			syntheticField := field
			syntheticField.FieldPath = syntheticPath
			// Update owner context to the CRD that embeds this type
			syntheticField.OwnerType = embedding.CRDOwner
			syntheticField.OwnerGVK = embedding.CRDOwnerGVK

			// Add to CRD owner's field list
			byOwner[embedding.CRDOwner] = append(byOwner[embedding.CRDOwner], syntheticField)

			s.logf("      synthetic: %s", syntheticPath)
		}
	}

	// Sort each owner's fields by path after adding synthetic ones
	for owner := range byOwner {
		sort.Slice(byOwner[owner], func(i, j int) bool {
			return byOwner[owner][i].FieldPath < byOwner[owner][j].FieldPath
		})
	}
}

// updateRegistryWithSyntheticPaths adds synthetic paths from data.ByOwner back to the TypedRegistry
// This ensures they're included in both Go and JSON output
func (s *MarkerScanner) updateRegistryWithSyntheticPaths(byOwner map[string][]templateField) {
	for owner, templateFields := range byOwner {
		// Get current registry fields for this owner
		registryFields, ok := s.TypedRegistry[owner]
		if !ok {
			registryFields = make(map[string]FieldMeta)
			s.TypedRegistry[owner] = registryFields
		}

		// Collect current field paths to detect which ones are synthetic (new)
		existingPaths := make(map[string]bool)
		for path := range registryFields {
			existingPaths[path] = true
		}

		// Add any new fields (synthetic paths) back to the registry
		for _, tfield := range templateFields {
			if !existingPaths[tfield.FieldPath] {
				// This is a synthetic field, convert it back to FieldMeta and add to registry
				writeMode := WriteMode("")
				switch tfield.WriteMode {
				case "Mutable":
					writeMode = Mutable
				case "Immutable":
					writeMode = Immutable
				case "ServiceSet":
					writeMode = ServiceSet
				}

				var gatedModes []FeatureGateWriteMode
				for _, gated := range tfield.GatedWriteModes {
					gateModeVal := WriteMode("")
					switch gated.WriteMode {
					case "Mutable":
						gateModeVal = Mutable
					case "Immutable":
						gateModeVal = Immutable
					case "ServiceSet":
						gateModeVal = ServiceSet
					}
					gatedModes = append(gatedModes, FeatureGateWriteMode{
						FeatureGate: gated.FeatureGate,
						WriteMode:   gateModeVal,
					})
				}

				meta := FieldMeta{
					FieldPath:                  tfield.FieldPath,
					WriteMode:                  writeMode,
					FeatureGate:                tfield.FeatureGate,
					Hidden:                     tfield.Hidden,
					FeatureGateAwareWriteModes: gatedModes,
					OwnerType:                  tfield.OwnerType,
					OwnerGVK:                   tfield.OwnerGVK,
				}

				registryFields[tfield.FieldPath] = meta
			}
		}
	}
}

// extractTypeName extracts the type name from an ast.Expr (handling pointers, etc.)
func (s *MarkerScanner) extractTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return s.extractTypeName(t.X)
	case *ast.SelectorExpr:
		// For selector expressions like hypershiftv1beta1.ClusterConfiguration,
		// return the full qualified name
		if sel, ok := t.X.(*ast.Ident); ok {
			return sel.Name + "." + t.Sel.Name
		}
		return ""
	default:
		return ""
	}
}

// getLocalTypeForUpstream returns the local type for an upstream type if we have a mapped equivalent
func (s *MarkerScanner) getLocalTypeForUpstream(upstreamType string) string {
	// upstreamType might be "hypershiftv1beta1.ClusterConfiguration" or just "ClusterConfiguration"
	// Check for exact match in the mappings
	for localType, mapping := range s.upstreamReducedTypes {
		if mapping.UpstreamType == upstreamType {
			return localType
		}
		// Also match if upstreamType is just the simple name and mapping.UpstreamType ends with it
		if !strings.Contains(upstreamType, ".") && strings.HasSuffix(mapping.UpstreamType, "."+upstreamType) {
			return localType
		}
	}
	return ""
}
