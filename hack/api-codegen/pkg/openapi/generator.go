package openapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdgen "sigs.k8s.io/controller-tools/pkg/crd"
	crdmarkers "sigs.k8s.io/controller-tools/pkg/crd/markers"
	"sigs.k8s.io/controller-tools/pkg/loader"
	"sigs.k8s.io/controller-tools/pkg/markers"

	"github.com/openshift-online/rosa-seaplane-api/hack/api-codegen/pkg/registry"
)

// schemaOutput is the top-level JSON structure written to the output file.
type schemaOutput struct {
	OpenAPI     string                                     `json:"openapi"`
	Info        schemaInfo                                 `json:"info"`
	Definitions map[string]apiextensionsv1.JSONSchemaProps `json:"definitions"`
}

type schemaInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// Generate creates an OpenAPI v3 schema from Go types using controller-tools.
func (g *Generator) Generate() error {
	if len(g.InputDirs) == 0 && len(g.InputPackages) == 0 {
		return g.generateMinimal()
	}

	roots, err := g.loadPackages()
	if err != nil {
		return fmt.Errorf("loading packages: %w", err)
	}

	definitions, err := g.generateDefinitions(roots)
	if err != nil {
		return fmt.Errorf("generating definitions: %w", err)
	}

	// Filter hidden fields using the registry
	if err := filterHiddenFields(definitions); err != nil {
		return fmt.Errorf("filtering hidden fields: %w", err)
	}

	// Collapse deeply-nested passthrough types into opaque objects.
	// The public API exposes the top-level wrapper fields but treats the
	// embedded HyperShift spec as a pass-through object.
	collapsePassthroughTypes(definitions)

	output := schemaOutput{
		OpenAPI: "3.0.0",
		Info: schemaInfo{
			Title:   g.Title,
			Version: g.Version,
			Description: fmt.Sprintf(
				"OpenAPI schema for %s generated from Go types with controller-tools\n\n"+
					"Non-mutable fields marked with +k8s:openapi-gen=false are excluded from this schema.",
				g.Title,
			),
		},
		Definitions: definitions,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(g.OutputFile), 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	return os.WriteFile(g.OutputFile, data, 0644)
}

// generateMinimal creates a minimal schema with no definitions.
func (g *Generator) generateMinimal() error {
	output := schemaOutput{
		OpenAPI: "3.0.0",
		Info: schemaInfo{
			Title:       g.Title,
			Version:     g.Version,
			Description: "OpenAPI schema for " + g.Title + " (minimal)",
		},
		Definitions: make(map[string]apiextensionsv1.JSONSchemaProps),
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}

	return os.WriteFile(g.OutputFile, data, 0644)
}

// loadPackages loads Go packages from input directories or import paths.
func (g *Generator) loadPackages() ([]*loader.Package, error) {
	var loadPaths []string
	loadPaths = append(loadPaths, g.InputDirs...)
	loadPaths = append(loadPaths, g.InputPackages...)

	return loader.LoadRoots(loadPaths...)
}

// generateDefinitions uses controller-tools to generate OpenAPI schemas for
// all exported struct types in the loaded packages.
func (g *Generator) generateDefinitions(roots []*loader.Package) (map[string]apiextensionsv1.JSONSchemaProps, error) {
	reg := &markers.Registry{}
	if err := crdmarkers.Register(reg); err != nil {
		return nil, fmt.Errorf("registering markers: %w", err)
	}

	collector := &markers.Collector{Registry: reg}
	checker := &loader.TypeChecker{}

	parser := &crdgen.Parser{
		Collector: collector,
		Checker:   checker,
	}
	crdgen.AddKnownTypes(parser)

	for _, root := range roots {
		parser.NeedPackage(root)
	}

	// Collect all type identifiers, then generate flattened schemas
	var typeIdents []crdgen.TypeIdent
	for ident := range parser.Types {
		typeIdents = append(typeIdents, ident)
	}
	sort.Slice(typeIdents, func(i, j int) bool {
		return typeIdents[i].Name < typeIdents[j].Name
	})

	for _, ident := range typeIdents {
		parser.NeedFlattenedSchemaFor(ident)
	}

	definitions := make(map[string]apiextensionsv1.JSONSchemaProps)
	for ident, schema := range parser.FlattenedSchemata {
		definitions[ident.Name] = schema
	}

	return definitions, nil
}

// typeToRegistryPrefix maps Go definition type names to the dotted prefix
// used in the FieldRegistry. The scanner stores paths like "spec.accountId"
// (rooted at the Cluster/NodePool root type) while openapi-gen definitions
// are keyed by the Go struct name (e.g., "ClusterSpec").
//
// Types without an explicit entry still get a prefix derived generically:
// *Status definitions are rooted at "status" (see registryPrefixForDefinition).
var typeToRegistryPrefix = map[string]string{
	"ClusterSpec":                  "spec",
	"Cluster":                      "",
	"NodePoolSpec":                 "spec",
	"NodePool":                     "",
	"OidcConfigSpec":               "spec",
	"HostedClusterSpecPassthrough": "spec.hostedCluster",
	"NodePoolSpecPassthrough":      "spec.nodePool",
	"ClusterConfiguration":         "",
	"ClusterNetworking":            "",
	"ClusterDNS":                   "",
	"ProxyConfiguration":           "",
	"PlatformSpec":                 "",
	"NodePoolPlatform":             "",
	"AWSPlatformSpec":              "aws",
	"AWSNodePoolPlatform":          "aws",
	"MachineConfigSpec":            "machineConfig",
}

// filterHiddenFields removes fields hidden from the public API in the registry
// from all schema definitions. Mutable fields marked +k8s:openapi-gen=false
// remain in Seaplane's custom public schema.
//
// Every definition that carries hidden fields must resolve to a registry
// prefix — either via typeToRegistryPrefix or the generic *Status rule in
// registryPrefixForDefinition. An unmapped definition whose properties match
// hidden registry entries is an error (the mapping was forgotten).
func filterHiddenFields(definitions map[string]apiextensionsv1.JSONSchemaProps) error {
	hiddenPaths := make(map[string]bool)
	// Collect hidden field paths from all types in the typed registry
	for _, fields := range registry.FieldRegistry {
		for path, meta := range fields {
			if meta.HiddenFromPublicAPI() {
				hiddenPaths[path] = true
			}
		}
	}

	if len(hiddenPaths) == 0 {
		return nil
	}

	return filterDefinitionsWithPaths(definitions, hiddenPaths)
}

// filterDefinitionsWithPaths prunes hidden fields from definitions using the
// supplied hidden path set. Split from filterHiddenFields so tests can exercise
// pruning and the leak guard with synthetic registry paths.
func filterDefinitionsWithPaths(definitions map[string]apiextensionsv1.JSONSchemaProps, hiddenPaths map[string]bool) error {
	for typeName, schema := range definitions {
		prefix, ok := registryPrefixForDefinition(typeName)
		if !ok {
			if leaked := leakedHiddenProps(schema, hiddenPaths); len(leaked) > 0 {
				return fmt.Errorf("definition %q has hidden fields %v but no entry in typeToRegistryPrefix", typeName, leaked)
			}
			continue
		}
		pruned := pruneHiddenProperties(&schema, prefix, hiddenPaths)
		if strings.HasSuffix(typeName, "List") {
			pruneHiddenResourceListItems(pruned, prefix, hiddenPaths)
		}
		definitions[typeName] = *pruned
	}
	return nil
}

func registryPrefixForDefinition(typeName string) (string, bool) {
	if prefix, ok := typeToRegistryPrefix[typeName]; ok {
		return prefix, true
	}
	if strings.HasSuffix(typeName, "List") {
		itemType := strings.TrimSuffix(typeName, "List")
		if prefix, ok := typeToRegistryPrefix[itemType]; ok {
			return prefix, true
		}
	}
	// Status sub-resources are scanned from the CRD root, so their fields are
	// registered under dotted paths rooted at "status" (e.g. ClusterStatus
	// fields are registered as "status.<field>" owned by the root kind).
	// Derive the prefix generically instead of maintaining an allowlist.
	if strings.HasSuffix(typeName, "Status") {
		return "status", true
	}
	return "", false
}

func pruneHiddenResourceListItems(schema *apiextensionsv1.JSONSchemaProps, prefix string, hiddenPaths map[string]bool) {
	if schema.Properties == nil {
		return
	}
	items, ok := schema.Properties["items"]
	if !ok || items.Type != "array" || items.Items == nil || items.Items.Schema == nil {
		return
	}
	items.Items.Schema = pruneHiddenProperties(items.Items.Schema, prefix, hiddenPaths)
	schema.Properties["items"] = items
}

// leakedHiddenProps returns property names from schema that match a hidden
// entry in the registry. Because the definition has no entry in
// typeToRegistryPrefix, its registry prefix is unknown, so a property is
// considered leaked when it matches either a top-level hidden path (e.g.
// "resourceTags") or the final segment of a dotted hidden path (e.g.
// "baseDomain" for "status.baseDomain"). Comparing full dotted paths — not
// bare property names — is what lets this guard catch hidden status and other
// non-spec fields leaking through unmapped definitions.
func leakedHiddenProps(schema apiextensionsv1.JSONSchemaProps, hidden map[string]bool) []string {
	var leaked []string
	for propName := range schema.Properties {
		if hidden[propName] {
			leaked = append(leaked, propName)
			continue
		}
		suffix := "." + propName
		for path := range hidden {
			if strings.HasSuffix(path, suffix) {
				leaked = append(leaked, propName)
				break
			}
		}
	}
	sort.Strings(leaked)
	return leaked
}

// pruneHiddenProperties recursively removes properties whose registry path
// is marked hidden. pathPrefix is the dotted field path so far (e.g., "spec").
func pruneHiddenProperties(schema *apiextensionsv1.JSONSchemaProps, pathPrefix string, hidden map[string]bool) *apiextensionsv1.JSONSchemaProps {
	if schema == nil || schema.Properties == nil {
		return schema
	}

	for propName, propSchema := range schema.Properties {
		var fieldPath string
		if pathPrefix == "" {
			fieldPath = propName
		} else {
			fieldPath = pathPrefix + "." + propName
		}
		if hidden[fieldPath] {
			delete(schema.Properties, propName)
			// Also remove from required list
			removeRequired(schema, propName)
			continue
		}
		pruned := pruneHiddenProperties(&propSchema, fieldPath, hidden)
		schema.Properties[propName] = *pruned
	}

	return schema
}

// removeRequired removes a field name from a schema's Required slice.
func removeRequired(schema *apiextensionsv1.JSONSchemaProps, field string) {
	for i, r := range schema.Required {
		if r == field {
			schema.Required = append(schema.Required[:i], schema.Required[i+1:]...)
			return
		}
	}
}

// refTargets are types whose properties are replaced with $ref pointers.
// The key is the parent type, the value maps field name → definition name.
var refTargets = map[string]map[string]string{
	"ClusterSpec":                  {"hostedCluster": "HostedClusterSpecPassthrough"},
	"NodePoolSpec":                 {"nodePool": "NodePoolSpecPassthrough"},
	"HostedClusterSpecPassthrough": {"configuration": "ClusterConfiguration", "platform": "PlatformSpec", "capabilities": "Capabilities"},
	"NodePoolSpecPassthrough":      {"platform": "NodePoolPlatform"},
	"ClusterConfiguration":         {"machineConfig": "MachineConfigSpec"},
}

// collapsePassthroughTypes replaces inlined nested type properties with
// $ref pointers to their named definitions and marks passthrough types
// with additionalProperties: true. Types not in refTargets have their
// nested properties stripped to keep the spec shallow.
func collapsePassthroughTypes(definitions map[string]apiextensionsv1.JSONSchemaProps) {
	// Mark passthrough types as accepting additional properties
	for _, typeName := range []string{"HostedClusterSpecPassthrough", "NodePoolSpecPassthrough"} {
		if schema, ok := definitions[typeName]; ok {
			schema.AdditionalProperties = &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true}
			definitions[typeName] = schema
		}
	}

	// Replace inlined properties with $ref for all configured targets
	for parentType, fields := range refTargets {
		schema, ok := definitions[parentType]
		if !ok || schema.Properties == nil {
			continue
		}
		for fieldName, defName := range fields {
			if _, exists := schema.Properties[fieldName]; exists {
				schema.Properties[fieldName] = apiextensionsv1.JSONSchemaProps{
					Ref: strPtr("#/definitions/" + defName),
				}
			}
		}
		definitions[parentType] = schema
	}

	// Strip remaining inlined sub-properties from passthrough type fields
	// that aren't wired to $ref (keeps them as shallow type: object).
	for _, typeName := range []string{"HostedClusterSpecPassthrough", "NodePoolSpecPassthrough"} {
		schema, ok := definitions[typeName]
		if !ok {
			continue
		}
		refs := refTargets[typeName]
		for propName, propSchema := range schema.Properties {
			if refs != nil {
				if _, isRef := refs[propName]; isRef {
					continue
				}
			}
			propSchema.Properties = nil
			propSchema.Required = nil
			schema.Properties[propName] = propSchema
		}
		definitions[typeName] = schema
	}
}

func strPtr(s string) *string { return &s }
