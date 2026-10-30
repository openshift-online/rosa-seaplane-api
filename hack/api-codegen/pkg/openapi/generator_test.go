package openapi

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestFilterHiddenFieldsRetainsMutableNodePoolLabelsAndTaints(t *testing.T) {
	definitions := map[string]apiextensionsv1.JSONSchemaProps{
		"NodePoolSpecPassthrough": {
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"nodeLabels":  {Type: "object"},
				"taints":      {Type: "array"},
				"pausedUntil": {Type: "string"},
			},
		},
	}

	if err := filterHiddenFields(definitions); err != nil {
		t.Fatalf("filterHiddenFields(): %v", err)
	}
	properties := definitions["NodePoolSpecPassthrough"].Properties
	for _, visible := range []string{"nodeLabels", "taints"} {
		if _, ok := properties[visible]; !ok {
			t.Errorf("mutable field %q was filtered from the public schema", visible)
		}
	}
	if _, ok := properties["pausedUntil"]; ok {
		t.Error("service-set field pausedUntil was not filtered from the public schema")
	}
}

func TestFilterHiddenFieldsRemovesAWSTagPassthroughs(t *testing.T) {
	object := func(properties map[string]apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: properties}
	}
	stringField := apiextensionsv1.JSONSchemaProps{Type: "string"}
	resourceTags := apiextensionsv1.JSONSchemaProps{Type: "array"}
	definitions := map[string]apiextensionsv1.JSONSchemaProps{
		"PlatformSpec": object(map[string]apiextensionsv1.JSONSchemaProps{
			"aws": object(map[string]apiextensionsv1.JSONSchemaProps{
				"region": stringField, "resourceTags": resourceTags,
			}),
		}),
		"AWSPlatformSpec": object(map[string]apiextensionsv1.JSONSchemaProps{
			"region": stringField, "resourceTags": resourceTags,
		}),
		"NodePoolPlatform": object(map[string]apiextensionsv1.JSONSchemaProps{
			"aws": object(map[string]apiextensionsv1.JSONSchemaProps{"resourceTags": resourceTags}),
		}),
		"AWSNodePoolPlatform": object(map[string]apiextensionsv1.JSONSchemaProps{"resourceTags": resourceTags}),
	}

	if err := filterHiddenFields(definitions); err != nil {
		t.Fatalf("filterHiddenFields(): %v", err)
	}
	for _, test := range []struct {
		definition string
		path       []string
	}{
		{definition: "PlatformSpec", path: []string{"aws"}},
		{definition: "AWSPlatformSpec"},
		{definition: "NodePoolPlatform", path: []string{"aws"}},
		{definition: "AWSNodePoolPlatform"},
	} {
		schema := definitions[test.definition]
		for _, name := range test.path {
			property, found := schema.Properties[name]
			if !found {
				t.Fatalf("definition %s is missing path %v", test.definition, test.path)
			}
			schema = property
		}
		if _, found := schema.Properties["resourceTags"]; found {
			t.Errorf("resourceTags remains visible in %s at path %v", test.definition, test.path)
		}
	}
}

func TestFilterHiddenFieldsPrunesResourceTagsFromResourceLists(t *testing.T) {
	object := func(properties map[string]apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
		return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: properties}
	}
	resourceTags := apiextensionsv1.JSONSchemaProps{Type: "array"}
	var objectAtPath func([]string) apiextensionsv1.JSONSchemaProps
	objectAtPath = func(path []string) apiextensionsv1.JSONSchemaProps {
		if len(path) == 0 {
			return object(map[string]apiextensionsv1.JSONSchemaProps{"resourceTags": resourceTags})
		}
		return object(map[string]apiextensionsv1.JSONSchemaProps{path[0]: objectAtPath(path[1:])})
	}
	list := func(item apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
		return object(map[string]apiextensionsv1.JSONSchemaProps{
			"items": {
				Type:  "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &item},
			},
		})
	}
	definitions := map[string]apiextensionsv1.JSONSchemaProps{
		"Cluster":      objectAtPath([]string{"spec", "hostedCluster", "platform", "aws"}),
		"ClusterList":  list(objectAtPath([]string{"spec", "hostedCluster", "platform", "aws"})),
		"NodePool":     objectAtPath([]string{"spec", "nodePool", "platform", "aws"}),
		"NodePoolList": list(objectAtPath([]string{"spec", "nodePool", "platform", "aws"})),
	}

	if err := filterHiddenFields(definitions); err != nil {
		t.Fatalf("filterHiddenFields(): %v", err)
	}
	assertHidden := func(name string, schema apiextensionsv1.JSONSchemaProps, path ...string) {
		t.Helper()
		for _, segment := range path {
			property, found := schema.Properties[segment]
			if !found {
				t.Fatalf("definition %s is missing path %v", name, path)
			}
			schema = property
		}
		if _, found := schema.Properties["resourceTags"]; found {
			t.Errorf("resourceTags remains visible in %s at path %v", name, path)
		}
	}
	assertHidden("Cluster", definitions["Cluster"], "spec", "hostedCluster", "platform", "aws")
	assertHidden("NodePool", definitions["NodePool"], "spec", "nodePool", "platform", "aws")
	for _, listName := range []string{"ClusterList", "NodePoolList"} {
		items := definitions[listName].Properties["items"]
		if items.Items == nil || items.Items.Schema == nil {
			t.Fatalf("definition %s has no inline item schema", listName)
		}
		if listName == "ClusterList" {
			assertHidden(listName, *items.Items.Schema, "spec", "hostedCluster", "platform", "aws")
		} else {
			assertHidden(listName, *items.Items.Schema, "spec", "nodePool", "platform", "aws")
		}
	}
}

func TestFilterHiddenFieldsPrunesHiddenStatusFields(t *testing.T) {
	definitions := map[string]apiextensionsv1.JSONSchemaProps{
		"ClusterStatus": {
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"provisioned": {Type: "boolean"},
				"baseDomain":  {Type: "string"},
			},
		},
	}
	// A status field marked +k8s:openapi-gen=false is registered by the
	// marker scanner as "status.<field>" under the root kind.
	hidden := map[string]bool{"status.baseDomain": true}

	if err := filterDefinitionsWithPaths(definitions, hidden); err != nil {
		t.Fatalf("filterDefinitionsWithPaths(): %v", err)
	}
	properties := definitions["ClusterStatus"].Properties
	if _, found := properties["provisioned"]; !found {
		t.Error("visible status field provisioned was filtered from the public schema")
	}
	if _, found := properties["baseDomain"]; found {
		t.Error("hidden status field baseDomain leaked into the public schema")
	}
}

func TestFilterHiddenFieldsFailsOnUnmappedDefinitionWithDottedHiddenPath(t *testing.T) {
	// The guard must compare dotted registry paths, not bare property names:
	// a hidden "status.baseDomain" entry previously slipped past a definition
	// with no typeToRegistryPrefix entry because the bare names never matched.
	definitions := map[string]apiextensionsv1.JSONSchemaProps{
		"UnmappedStatusLike": {
			Type: "object",
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"baseDomain": {Type: "string"},
			},
		},
	}
	hidden := map[string]bool{"status.baseDomain": true}

	err := filterDefinitionsWithPaths(definitions, hidden)
	if err == nil {
		t.Fatal("expected an error for an unmapped definition carrying a hidden field")
	}
	if !strings.Contains(err.Error(), "UnmappedStatusLike") || !strings.Contains(err.Error(), "baseDomain") {
		t.Errorf("error should name the definition and leaked field, got: %v", err)
	}
}

func TestConfigurationUsesLocalType(t *testing.T) {
	output := generateConfigurationSchema(t)

	t.Run("cluster configuration", func(t *testing.T) { assertClusterConfiguration(t, output) })
	t.Run("ingress configuration", func(t *testing.T) { assertIngressConfiguration(t, output) })
	t.Run("scheduler configuration", func(t *testing.T) { assertSchedulerConfiguration(t, output) })
	t.Run("proxy configuration", func(t *testing.T) { assertProxyConfiguration(t, output) })
	t.Run("machineConfig", func(t *testing.T) { assertMachineConfiguration(t, output) })
	t.Run("cluster autoscaling", func(t *testing.T) { assertClusterAutoscaling(t, output) })
	t.Run("capabilities", func(t *testing.T) { assertCapabilities(t, output) })
}

func generateConfigurationSchema(t *testing.T) schemaOutput {
	t.Helper()
	tmpFile := t.TempDir() + "/openapi.json"

	// Resolve the public API package relative to this test file's location
	// (hack/api-codegen/pkg/openapi/) → ../../../../api/v1alpha1/public.
	// Production generation (make generate-openapi) scans the same directory.
	publicDir := "../../../../api/v1alpha1/public"
	if _, err := os.Stat(publicDir); err != nil {
		t.Skipf("public API source not available: %v", err)
	}

	gen := NewGenerator([]string{publicDir}, tmpFile)
	gen.Title = "Test"
	gen.Version = "v1"

	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Read output: %v", err)
	}

	var output schemaOutput
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return output
}

func assertClusterConfiguration(t *testing.T, output schemaOutput) {
	t.Helper()
	// ClusterConfiguration must exist as its own definition (from the local type)
	cc, ok := output.Definitions["ClusterConfiguration"]
	if !ok {
		t.Fatal("ClusterConfiguration definition not found")
	}

	// The local type's markers expose authentication, ingress, proxy, scheduler,
	// and machineConfig.
	// If the upstream hypershiftv1beta1.ClusterConfiguration were used instead,
	// all 10 sub-config fields would be present (no hidden markers).
	for _, visible := range []string{"authentication", "ingress", "proxy", "scheduler", "machineConfig"} {
		if _, found := cc.Properties[visible]; !found {
			t.Errorf("expected visible property %q in ClusterConfiguration", visible)
		}
	}
	for _, hidden := range []string{"apiServer", "featureGate", "image", "network", "oauth", "kubelet"} {
		if _, found := cc.Properties[hidden]; found {
			t.Errorf("property %q should be hidden in ClusterConfiguration (local markers not applied?)", hidden)
		}
	}
}

func assertIngressConfiguration(t *testing.T, output schemaOutput) {
	t.Helper()
	// IngressConfiguration exposes only customer-configurable component routes.
	ic, ok := output.Definitions["IngressConfiguration"]
	if !ok {
		t.Fatal("IngressConfiguration definition not found")
	}
	componentRoutes, found := ic.Properties["componentRoutes"]
	if !found {
		t.Error("componentRoutes property not found in IngressConfiguration")
	} else {
		if componentRoutes.MaxItems == nil || *componentRoutes.MaxItems != 2 {
			t.Errorf("componentRoutes maxItems = %v, want 2", componentRoutes.MaxItems)
		}
		if componentRoutes.XListType == nil || *componentRoutes.XListType != "map" {
			t.Errorf("componentRoutes list type = %v, want map", componentRoutes.XListType)
		}
		if got, want := componentRoutes.XListMapKeys, []string{"namespace", "name"}; !reflect.DeepEqual(got, want) {
			t.Errorf("componentRoutes list map keys = %v, want %v", got, want)
		}
	}
	crc, ok := output.Definitions["ComponentRouteConfiguration"]
	if !ok {
		t.Fatal("ComponentRouteConfiguration definition not found")
	}
	for _, visible := range []string{"namespace", "name", "hostname", "servingCertKeyPairSecret"} {
		if _, found := crc.Properties[visible]; !found {
			t.Errorf("expected visible property %q in ComponentRouteConfiguration", visible)
		}
	}
	if _, found := crc.Properties["labels"]; found {
		t.Error("labels should not be exposed in ComponentRouteConfiguration")
	}
	for field, allowedValues := range map[string]map[string]bool{
		"namespace": {`"openshift-console"`: true},
		"name": {
			`"console"`:   true,
			`"downloads"`: true,
		},
	} {
		property := crc.Properties[field]
		for _, value := range property.Enum {
			delete(allowedValues, string(value.Raw))
		}
		if len(allowedValues) != 0 {
			t.Errorf("%s enum is missing values: %v", field, allowedValues)
		}
	}
}

func assertSchedulerConfiguration(t *testing.T, output schemaOutput) {
	t.Helper()
	// SchedulerConfiguration exposes only the supported scheduler profile choice.
	sc, ok := output.Definitions["SchedulerConfiguration"]
	if !ok {
		t.Fatal("SchedulerConfiguration definition not found")
	}
	profile, ok := sc.Properties["profile"]
	if !ok {
		t.Fatal("profile property not found in SchedulerConfiguration")
	}
	wantProfiles := map[string]bool{
		`"LowNodeUtilization"`:  true,
		`"HighNodeUtilization"`: true,
		`"NoScoring"`:           true,
	}
	for _, value := range profile.Enum {
		delete(wantProfiles, string(value.Raw))
	}
	if len(wantProfiles) != 0 {
		t.Errorf("profile enum is missing values: %v", wantProfiles)
	}
}

func assertProxyConfiguration(t *testing.T, output schemaOutput) {
	t.Helper()
	// ProxyConfiguration exposes user-settable proxy fields but keeps the
	// service-managed fields out of the generated schema.
	pc, ok := output.Definitions["ProxyConfiguration"]
	if !ok {
		t.Fatal("ProxyConfiguration definition not found")
	}
	for _, visible := range []string{"httpProxy", "httpsProxy", "noProxy"} {
		if _, found := pc.Properties[visible]; !found {
			t.Errorf("expected visible property %q in ProxyConfiguration", visible)
		}
	}
	for _, hidden := range []string{"trustedCA", "readinessEndpoints"} {
		if _, found := pc.Properties[hidden]; found {
			t.Errorf("property %q should be hidden in ProxyConfiguration", hidden)
		}
	}
}

func assertMachineConfiguration(t *testing.T, output schemaOutput) {
	t.Helper()
	// MachineConfigSpec must retain its visible fields (nested path test)
	mc, ok := output.Definitions["MachineConfigSpec"]
	if !ok {
		t.Fatal("MachineConfigSpec definition not found")
	}
	if _, found := mc.Properties["allowedKernelArguments"]; !found {
		t.Error("expected visible property \"allowedKernelArguments\" in MachineConfigSpec")
	}
	for _, hidden := range []string{"kernelArguments", "systemdUnits", "files", "extensions"} {
		if _, found := mc.Properties[hidden]; found {
			t.Errorf("property %q should be hidden in MachineConfigSpec", hidden)
		}
	}
}

func assertClusterAutoscaling(t *testing.T, output schemaOutput) {
	t.Helper()
	// Cluster autoscaling is exposed from the HostedCluster passthrough.
	hc, ok := output.Definitions["HostedClusterSpecPassthrough"]
	if !ok {
		t.Fatal("HostedClusterSpecPassthrough definition not found")
	}
	if _, found := hc.Properties["autoscaling"]; !found {
		t.Error("autoscaling property not found in HostedClusterSpecPassthrough")
	}
}

func assertCapabilities(t *testing.T, output schemaOutput) {
	t.Helper()
	// Capabilities must resolve to the local mirror type via $ref so the
	// standalone HostedClusterSpecPassthrough schema is not collapsed to an
	// opaque object.
	hc, ok := output.Definitions["HostedClusterSpecPassthrough"]
	if !ok {
		t.Fatal("HostedClusterSpecPassthrough definition not found")
	}
	capsRef := hc.Properties["capabilities"].Ref
	if capsRef == nil || *capsRef != "#/definitions/Capabilities" {
		t.Fatalf("capabilities property must be a $ref to the Capabilities definition, got %v", capsRef)
	}

	caps, ok := output.Definitions["Capabilities"]
	if !ok {
		t.Fatal("Capabilities definition not found")
	}
	for _, list := range []string{"enabled", "disabled"} {
		prop, found := caps.Properties[list]
		if !found {
			t.Errorf("expected visible property %q in Capabilities", list)
			continue
		}
		if prop.Type != "array" {
			t.Errorf("Capabilities.%s type = %q, want array", list, prop.Type)
		}
		if prop.MaxItems == nil || *prop.MaxItems != 25 {
			t.Errorf("Capabilities.%s maxItems = %v, want 25", list, prop.MaxItems)
		}
		if prop.XListType == nil || *prop.XListType != "atomic" {
			t.Errorf("Capabilities.%s list type = %v, want atomic", list, prop.XListType)
		}
		// The enum of allowed capability names must come from the upstream
		// OptionalCapability type.
		wantValues := map[string]bool{
			`"ImageRegistry"`:     true,
			`"openshift-samples"`: true,
			`"Insights"`:          true,
			`"baremetal"`:         true,
			`"Console"`:           true,
			`"NodeTuning"`:        true,
			`"Ingress"`:           true,
		}
		if prop.Items == nil || prop.Items.Schema == nil {
			t.Errorf("Capabilities.%s has no items schema", list)
			continue
		}
		for _, value := range prop.Items.Schema.Enum {
			delete(wantValues, string(value.Raw))
		}
		if len(wantValues) != 0 {
			t.Errorf("Capabilities.%s enum is missing values: %v", list, wantValues)
		}
		// Per-list immutability CEL rule must survive the passthrough collapse.
		foundImmutable := false
		for _, rule := range prop.XValidations {
			if rule.Rule == "self == oldSelf" {
				foundImmutable = true
			}
		}
		if !foundImmutable {
			t.Errorf("Capabilities.%s is missing the self == oldSelf immutability rule", list)
		}
	}
	// The Ingress-requires-Console rule applies to the disabled list only.
	foundIngressRule := false
	for _, rule := range caps.Properties["disabled"].XValidations {
		if strings.Contains(rule.Rule, "Ingress") {
			foundIngressRule = true
		}
	}
	if !foundIngressRule {
		t.Error("Capabilities.disabled is missing the Ingress/Console validation rule")
	}
	// Cross-field rule lives on the object itself.
	foundCrossField := false
	for _, rule := range caps.XValidations {
		if strings.Contains(rule.Rule, "self.disabled") {
			foundCrossField = true
		}
	}
	if !foundCrossField {
		t.Error("Capabilities is missing the enabled/disabled cross-field validation rule")
	}
}

func TestGenerateMinimal(t *testing.T) {
	tmpFile := "/tmp/openapi-test.json"
	defer func() { _ = os.Remove(tmpFile) }()

	gen := NewGenerator(nil, tmpFile)
	gen.Title = "Test API"
	gen.Version = "v1"

	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if _, err := os.Stat(tmpFile); err != nil {
		t.Fatalf("Output file not created: %v", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read output: %v", err)
	}

	var output schemaOutput
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatalf("Output is not valid JSON: %v", err)
	}

	if output.OpenAPI != "3.0.0" {
		t.Errorf("Expected OpenAPI 3.0.0, got %s", output.OpenAPI)
	}
	if output.Info.Title != "Test API" {
		t.Errorf("Expected title 'Test API', got %s", output.Info.Title)
	}
	if output.Info.Version != "v1" {
		t.Errorf("Expected version 'v1', got %s", output.Info.Version)
	}
	if len(output.Definitions) != 0 {
		t.Errorf("Expected 0 definitions in minimal mode, got %d", len(output.Definitions))
	}
}
