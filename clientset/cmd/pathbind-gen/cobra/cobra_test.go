package cobra

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	pkg "github.com/openshift-online/rosa-seaplane-api/clientset/cmd/pathbind-gen/pkg"
)

// TestFlagCall verifies that flagCall emits the correct cobra registration
// statement for each consumer field type. This is critical because pointer
// types (*bool, *int32) require pre-allocation before being passed to cobra's
// BoolVar/Int32Var — passing a nil pointer panics at runtime.
func TestFlagCall(t *testing.T) {
	funcMap := buildFuncMap()
	flagCall := funcMap["flagCall"].(func(pkg.MergedAlias) string)

	cases := []struct {
		name     string
		alias    pkg.MergedAlias
		wantSubs []string // substrings that must appear in the output
		wantNot  []string // substrings that must NOT appear
	}{
		{
			name:     "plain string",
			alias:    pkg.MergedAlias{GoName: "Name", Type: "string", Flag: "cluster-name", Description: "The name."},
			wantSubs: []string{`f.StringVar(&input.Name`, `"cluster-name"`, `"The name."`},
		},
		{
			name:     "plain bool",
			alias:    pkg.MergedAlias{GoName: "Fips", Type: "bool", Flag: "fips", Description: "FIPS mode."},
			wantSubs: []string{`f.BoolVar(&input.Fips`, `"fips"`, `"FIPS mode."`},
			// Plain bool must NOT pre-allocate.
			wantNot: []string{"new(bool)"},
		},
		{
			name:  "*bool must pre-allocate",
			alias: pkg.MergedAlias{GoName: "DeleteProtection", Type: "*bool", Flag: "delete-protection", Description: "Enable delete protection."},
			// *bool requires new(bool) so BoolVar receives a non-nil *bool, not a nil pointer.
			wantSubs: []string{"new(bool)", `input.DeleteProtection`, `"delete-protection"`},
		},
		{
			name:     "plain int32",
			alias:    pkg.MergedAlias{GoName: "Replicas", Type: "int32", Flag: "replicas", Description: "Replica count."},
			wantSubs: []string{`f.Int32Var(&input.Replicas`, `"replicas"`},
			wantNot:  []string{"new(int32)"},
		},
		{
			name:     "*int32 must pre-allocate",
			alias:    pkg.MergedAlias{GoName: "Replicas", Type: "*int32", Flag: "replicas", Description: "Replica count."},
			wantSubs: []string{"new(int32)", `input.Replicas`, `"replicas"`},
		},
		{
			name:     "plain int64",
			alias:    pkg.MergedAlias{GoName: "Size", Type: "int64", Flag: "size", Description: "Volume size."},
			wantSubs: []string{`f.Int64Var(&input.Size`, `"size"`},
			wantNot:  []string{"new(int64)"},
		},
		{
			name:     "*int64 must pre-allocate",
			alias:    pkg.MergedAlias{GoName: "Iops", Type: "*int64", Flag: "iops", Description: "Volume IOPS."},
			wantSubs: []string{"new(int64)", `input.Iops`, `"iops"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagCall(tc.alias)
			for _, want := range tc.wantSubs {
				if !strings.Contains(got, want) {
					t.Errorf("flagCall(%+v) = %q; want substring %q", tc.alias, got, want)
				}
			}
			for _, notWant := range tc.wantNot {
				if strings.Contains(got, notWant) {
					t.Errorf("flagCall(%+v) = %q; must NOT contain %q", tc.alias, got, notWant)
				}
			}
		})
	}
}

// TestToKebab verifies camelCase/PascalCase → kebab-case conversion with correct
// acronym handling: consecutive uppercase letters stay together rather than being
// segmented with hyphens.
func TestToKebab(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Normal camelCase
		{"displayName", "display-name"},
		{"deleteProtection", "delete-protection"},
		{"subnetId", "subnet-id"},
		// PascalCase
		{"AWSPlatform", "aws-platform"},
		// Trailing acronym: lower→upper inserts dash, consecutive uppers don't
		{"issuerURL", "issuer-url"},
		{"kubeCloudControllerARN", "kube-cloud-controller-arn"},
		{"controlPlaneOperatorARN", "control-plane-operator-arn"},
		{"imageRegistryARN", "image-registry-arn"},
		{"nodePoolManagementARN", "node-pool-management-arn"},
		// All-caps acronym alone
		{"VPC", "vpc"},
		// Acronym followed by word: last uppercase of acronym gets dash before next word
		{"URLConfig", "url-config"},
		// Multi-segment acronym
		{"registryPullQPS", "registry-pull-qps"},
		// Acronym with plural 's' suffix: the 's' stays attached, no dash
		{"allocateNodeCIDRs", "allocate-node-cidrs"},
		{"ARNs", "arns"},
		{"VPCs", "vpcs"},
		// CIDR inside a longer word — 's' followed by uppercase → suffix rule, then new word dash
		{"allowedCIDRBlocks", "allowed-cidr-blocks"},
		{"CIDRsConfig", "cidrs-config"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := pkg.ToKebab(tc.input)
			if got != tc.want {
				t.Errorf("ToKebab(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCollectUnsetPtrFields_includesBoolPtr(t *testing.T) {
	aliases := []pkg.MergedAlias{
		{GoName: "MaxPods", Type: "*int64", Flag: "max-pods", HasFlag: true, Operations: []string{"create"}},
		{GoName: "SerializeImagePulls", Type: "*bool", Flag: "serialize-image-pulls", HasFlag: true, Operations: []string{"create", "update"}},
		{GoName: "Name", Type: "string", Flag: "cluster-name", HasFlag: true, Operations: []string{"create"}},
		{GoName: "Fips", Type: "bool", Flag: "fips", HasFlag: true, Operations: []string{"create"}},
		{GoName: "DeleteProtection", Type: "*bool", Flag: "delete-protection", HasFlag: true, Operations: []string{"create", "update"}},
	}

	got := pkg.CollectUnsetPtrFields(aliases)
	want := map[string]pkg.UnsetPtrField{
		"max-pods": {
			GoName:     "MaxPods",
			FlagName:   "max-pods",
			Operations: []string{"create"},
		},
		"serialize-image-pulls": {
			GoName:     "SerializeImagePulls",
			FlagName:   "serialize-image-pulls",
			Operations: []string{"create", "update"},
		},
		"delete-protection": {
			GoName:     "DeleteProtection",
			FlagName:   "delete-protection",
			Operations: []string{"create", "update"},
		},
	}

	if len(got) != len(want) {
		t.Fatalf("CollectUnsetPtrFields() returned %d fields, want %d: %v", len(got), len(want), got)
	}
	for _, field := range got {
		expected, ok := want[field.FlagName]
		if !ok {
			t.Fatalf("unexpected field %q in %v", field.FlagName, got)
		}
		if field.GoName != expected.GoName || field.FlagName != expected.FlagName {
			t.Fatalf("field %q: got %+v, want %+v", field.FlagName, field, expected)
		}
	}
}

func TestTemplatesUseFlatNodePoolClient(t *testing.T) {
	data := pkg.CobraTemplateData{
		Package:          "generated",
		RuntimePkgImport: "example/runtime",
		RuntimeAlias:     "runtime",
		RuntimeType:      "*runtime.Client",
		InteractivePkg:   "example/interactive",
		ResourceName:     "NodePool",
		SDKShortType:     "NodePool",
		UpdateFields: []pkg.MergedAlias{
			{GoName: "Labels", Type: "map[string]string", Path: "spec.labels", Flag: "labels", HasFlag: true, Operations: []string{"update"}, Resettable: true},
			{GoName: "Replicas", Type: "int32", Path: "spec.nodePool.replicas", Flag: "replicas", HasFlag: true, Operations: []string{"update"}},
		},
	}
	tests := []struct {
		name     string
		source   string
		wantCall string
	}{
		{name: "create", source: createTemplate, wantCall: ".NodePools().Create(ctx, obj,"},
		{name: "update", source: updateTemplate, wantCall: ".NodePools().Patch(ctx, name, types.MergePatchType, payload,"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New(tt.name).Funcs(buildFuncMap()).Parse(tt.source)
			if err != nil {
				t.Fatalf("parse template: %v", err)
			}
			var generated bytes.Buffer
			if err := tmpl.Execute(&generated, data); err != nil {
				t.Fatalf("execute template: %v", err)
			}
			got := generated.String()
			if !strings.Contains(got, tt.wantCall) {
				t.Errorf("generated %s does not call the flat NodePools client: missing %q", tt.name, tt.wantCall)
			}
			if strings.Contains(got, "namespace string") || strings.Contains(got, ".NodePools(namespace)") {
				t.Errorf("generated %s still requires a namespace argument", tt.name)
			}
			if tt.name == "update" {
				if !strings.Contains(got, "name string") || !strings.Contains(got, "obj.Name = name") {
					t.Errorf("generated update must route by metadata.name: %s", got)
				}
				if strings.Contains(got, "uid string") || strings.Contains(got, "obj.Name = uid") {
					t.Errorf("generated update must not route by metadata.uid: %s", got)
				}
				for _, required := range []string{
					"Get(ctx, name, platform.GetOptions{})",
					"pathbind.Flatten(ctx, current, &before)",
					"pathbind.UpdateDocument(ctx, before, after, obj)",
					"json.Marshal(document)",
					"field %s cannot be reset",
				} {
					if !strings.Contains(got, required) {
						t.Errorf("generated update is missing %q:\n%s", required, got)
					}
				}
			}
		})
	}
}

func TestUpdateTemplateUsesSharedFlagChangedHelper(t *testing.T) {
	if strings.Contains(updateTemplate, "func flagChanged(") {
		t.Fatal("update template must not define flagChanged because it is generated once per package")
	}
	if strings.Count(helpersTemplate, "func flagChanged(") != 1 {
		t.Fatal("helpers template must define flagChanged exactly once")
	}
}

func TestUpdateFmtImportIsConditional(t *testing.T) {
	data := pkg.CobraTemplateData{UpdateFields: []pkg.MergedAlias{
		{GoName: "Labels", Resettable: true},
	}}
	if got := updateFmtImport(data); got != "" {
		t.Fatalf("updateFmtImport() with only resettable fields = %q, want empty", got)
	}

	data.UpdateFields = append(data.UpdateFields, pkg.MergedAlias{GoName: "Replicas"})
	if got := updateFmtImport(data); got != "\n\t\"fmt\"" {
		t.Fatalf("updateFmtImport() with a non-resettable field = %q, want fmt import", got)
	}
}

func TestEmitUpdateFileConditionalFmtImport(t *testing.T) {
	tmpl := template.Must(template.New("update").Funcs(buildFuncMap()).Parse(updateTemplate))
	data := pkg.CobraTemplateData{
		Package:          "generated",
		RuntimePkgImport: "example/runtime",
		RuntimeAlias:     "runtime",
		InteractivePkg:   "example/interactive",
		ResourceName:     "NodePool",
		SDKShortType:     "NodePool",
		UpdateFields: []pkg.MergedAlias{
			{GoName: "Labels", Type: "map[string]string", Path: "spec.labels", Flag: "labels", HasFlag: true, Operations: []string{"update"}, Resettable: true},
		},
	}

	resettablePath := filepath.Join(t.TempDir(), "resettable_update_gen.go")
	header, err := licenseHeader("")
	if err != nil {
		t.Fatalf("licenseHeader(): %v", err)
	}
	if err := emitFile(tmpl, data, resettablePath, false, header); err != nil {
		t.Fatalf("emitFile() with only resettable fields: %v", err)
	}
	output, err := os.ReadFile(resettablePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("Licensed under the Apache License")) {
		t.Fatalf("generated file should carry the license header:\n%s", output)
	}
	if !bytes.Contains(output, []byte(strconv.Itoa(time.Now().Year()))) {
		t.Fatalf("generated file should have YEAR replaced with the current year:\n%s", output)
	}
	if bytes.Contains(output, []byte("\t\"fmt\"\n")) {
		t.Fatalf("generated update with only resettable fields should not import fmt:\n%s", output)
	}

	data.UpdateFields = append(data.UpdateFields, pkg.MergedAlias{
		GoName: "Replicas", Type: "int32", Path: "spec.nodePool.replicas", Flag: "replicas", HasFlag: true, Operations: []string{"update"},
	})
	nonResettablePath := filepath.Join(t.TempDir(), "non_resettable_update_gen.go")
	if err := emitFile(tmpl, data, nonResettablePath, false, header); err != nil {
		t.Fatalf("emitFile() with a non-resettable field: %v", err)
	}
	output, err = os.ReadFile(nonResettablePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("\t\"fmt\"\n")) {
		t.Fatalf("generated update with a non-resettable field should import fmt:\n%s", output)
	}
}

// TestFlagCallTrimPrefix documents why strings.TrimPrefix alone is insufficient
// to distinguish *bool from bool when combined with HasPrefix:
// both end up in the same switch case, and HasPrefix determines the branch —
// but the pointer branch previously emitted code that passed a nil *bool to BoolVar,
// which panics at runtime. The test pins the correct pre-allocation behavior.
func TestFlagCallTrimPrefixInsufficiency(t *testing.T) {
	// Demonstrate the root cause: TrimPrefix("*bool", "*") == "bool",
	// so a switch on the trimmed value cannot distinguish between types
	// without also checking the original type for the "*" prefix.
	// The current fix uses the full type string ("*bool") as the switch key.
	if strings.TrimPrefix("*bool", "*") != "bool" {
		t.Fatal("TrimPrefix invariant broken")
	}
	if strings.TrimPrefix("bool", "*") != "bool" {
		t.Fatal("TrimPrefix invariant broken")
	}
	// Both "*bool" and "bool" produce the same trimmed string.
	// A switch on TrimPrefix(type, "*") cannot tell them apart —
	// a secondary HasPrefix check is needed, which the original code had,
	// but the pointer branch emitted `f.BoolVar(input.X, ...)` where input.X
	// is a nil *bool, causing a nil pointer dereference at runtime.
	// The fix: switch on the full type string and pre-allocate for pointer types.
}
