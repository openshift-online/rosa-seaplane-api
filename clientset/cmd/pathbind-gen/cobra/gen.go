package cobra

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	pkg "github.com/openshift-online/rosa-seaplane-api/clientset/cmd/pathbind-gen/pkg"
	"gopkg.in/yaml.v3"
)

func Run(draftPath, overridesPath, outputDir string) error {
	rawOv, err := loadOverrides(overridesPath)
	if err != nil {
		return err
	}

	cfg := rawOv.Config
	if cfg.Package == "" || cfg.RuntimePkg == "" || cfg.RuntimeType == "" || cfg.InteractivePkg == "" {
		return fmt.Errorf("overrides config must set package, runtimePkg, runtimeType, and interactivePkg")
	}

	draftIndex := map[string]map[string]pkg.DraftField{}
	draftSDKTypes := map[string]string{}
	if err := pkg.LoadDraft(draftPath, draftIndex, draftSDKTypes); err != nil {
		return err
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	if err := emitHelpersFile(outputDir, cfg.Package); err != nil {
		return err
	}

	funcMap := buildFuncMap()

	createTmpl := template.Must(template.New("create").Funcs(funcMap).Parse(createTemplate))
	updateTmpl := template.Must(template.New("update").Funcs(funcMap).Parse(updateTemplate))

	allResKeys := pkg.UnionKeys(draftSDKTypes, rawOv.Resources)
	for _, resKey := range allResKeys {
		ovRes := rawOv.Resources[resKey]

		sdkType, ok := draftSDKTypes[resKey]
		if !ok {
			sdkType = pkg.SDKTypeForOwner[pkg.TitleCase(resKey)]
		}

		aliases, err := pkg.BuildMergedAliases(draftIndex[resKey], ovRes.Aliases)
		if err != nil {
			return fmt.Errorf("building aliases for %s: %w", resKey, err)
		}
		for _, alias := range aliases {
			if alias.Type == "map" || alias.Type == "string[]" {
				return fmt.Errorf("cobra does not support %s field %s", alias.Type, alias.Path)
			}
		}

		createFields, createFlagFields, reqCreate, updateFields, updateFlagFields, reqUpdate := pkg.CategorizeAliases(aliases)

		resName := pkg.TitleCase(resKey)
		sdkShort := pkg.SDKShortType(sdkType)
		runtimeAlias := pkg.PkgAlias(cfg.RuntimePkg)

		unsetPtrFields := pkg.CollectUnsetPtrFields(aliases)

		td := pkg.CobraTemplateData{
			Package:                  cfg.Package,
			RuntimePkgImport:         cfg.RuntimePkg,
			RuntimeAlias:             runtimeAlias,
			RuntimeType:              cfg.RuntimeType,
			InteractivePkg:           cfg.InteractivePkg,
			ResourceName:             resName,
			SDKShortType:             sdkShort,
			CreateFields:             createFields,
			UpdateFields:             updateFields,
			CreateFlagFields:         createFlagFields,
			UpdateFlagFields:         updateFlagFields,
			RequiredCreateFlagFields: reqCreate,
			RequiredUpdateFlagFields: reqUpdate,
			HasUpdateFields:          len(updateFlagFields) > 0,
			UnsetPtrFields:           unsetPtrFields,
		}

		if err := emitFile(createTmpl, td, filepath.Join(outputDir, strings.ToLower(resName)+"_create_gen.go"), true); err != nil {
			return err
		}
		if td.HasUpdateFields {
			if err := emitFile(updateTmpl, td, filepath.Join(outputDir, strings.ToLower(resName)+"_update_gen.go"), false); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadOverrides(path string) (*pkg.Overrides, error) {
	rawOv, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading overrides %s: %w", path, err)
	}
	var ov pkg.Overrides
	if err := yaml.Unmarshal(rawOv, &ov); err != nil {
		return nil, fmt.Errorf("parsing overrides: %w", err)
	}
	return &ov, nil
}

// licenseHeader is prepended to every generated file.
const licenseHeader = "// Copyright Red Hat\n// SPDX-License-Identifier: Apache-2.0\n\n"

func emitFile(tmpl *template.Template, data pkg.CobraTemplateData, path string, withHeader bool) error {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, licenseHeader)

	if !withHeader {
		fmt.Fprintf(&buf, "package %s\n\nimport (\n\t\"context\"\n\t\"encoding/json\"%s\n\t\"github.com/spf13/cobra\"\n\t%s %q\n\tv1alpha1 \"github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public\"\n\tpathbind \"github.com/openshift-online/rosa-seaplane-api/clientset/pathbind\"\n\tplatform \"github.com/openshift-online/rosa-seaplane-api/clientset/platform\"\n\tinteractive %q\n\t\"k8s.io/apimachinery/pkg/types\"\n)\n\n",
			data.Package,
			updateFmtImport(data),
			data.RuntimeAlias, data.RuntimePkgImport,
			data.InteractivePkg,
		)
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("rendering %s: %w", path, err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		debugPath := path + ".debug"
		if debugErr := os.WriteFile(debugPath, buf.Bytes(), 0o644); debugErr != nil {
			return fmt.Errorf("formatting %s: %w (also failed to write debug file %s: %v)", path, err, debugPath, debugErr)
		}
		return fmt.Errorf("formatting %s: %w\n(unformatted written to %s)", path, err, debugPath)
	}

	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Printf("pathbind-gen: wrote %s\n", path)
	return nil
}

func updateFmtImport(data pkg.CobraTemplateData) string {
	for _, field := range data.UpdateFields {
		if !field.Resettable {
			return "\n\t\"fmt\""
		}
	}
	return ""
}

func buildFuncMap() template.FuncMap {
	return template.FuncMap{
		"lower": strings.ToLower,
		"contains": func(slice []string, item string) bool {
			for _, o := range slice {
				if o == item {
					return true
				}
			}
			return false
		},
		"flagCall": func(a pkg.MergedAlias) string {
			var inner string
			switch a.Type {
			case "*bool":
				inner = fmt.Sprintf("input.%s = new(bool); f.BoolVar(input.%s, %q, false, %q)",
					a.GoName, a.GoName, a.Flag, a.Description)
			case "bool":
				inner = fmt.Sprintf("f.BoolVar(&input.%s, %q, false, %q)", a.GoName, a.Flag, a.Description)
			case "*int32":
				inner = fmt.Sprintf("input.%s = new(int32); f.Int32Var(input.%s, %q, 0, %q)",
					a.GoName, a.GoName, a.Flag, a.Description)
			case "int32":
				inner = fmt.Sprintf("f.Int32Var(&input.%s, %q, 0, %q)", a.GoName, a.Flag, a.Description)
			case "*int64":
				inner = fmt.Sprintf("input.%s = new(int64); f.Int64Var(input.%s, %q, 0, %q)",
					a.GoName, a.GoName, a.Flag, a.Description)
			case "int64":
				inner = fmt.Sprintf("f.Int64Var(&input.%s, %q, 0, %q)", a.GoName, a.Flag, a.Description)
			case "*string":
				inner = fmt.Sprintf("input.%s = new(string); f.StringVar(input.%s, %q, \"\", %q)",
					a.GoName, a.GoName, a.Flag, a.Description)
			case "string":
				inner = fmt.Sprintf("f.StringVar(&input.%s, %q, \"\", %q)", a.GoName, a.Flag, a.Description)
			default:
				inner = fmt.Sprintf("f.StringVar(&input.%s, %q, \"\", %q)", a.GoName, a.Flag, a.Description)
			}
			return fmt.Sprintf("registerIfNew(f, %q, func() { %s })", a.Flag, inner)
		},
		"emptyCheck": func(a pkg.MergedAlias) string {
			switch a.Type {
			case "*bool", "*int32", "*int64", "*string":
				return fmt.Sprintf("input.%s == nil", a.GoName)
			case "bool":
				return fmt.Sprintf("input.%s == false", a.GoName)
			case "int32", "int64":
				return fmt.Sprintf("input.%s == 0", a.GoName)
			case "string":
				return fmt.Sprintf(`input.%s == ""`, a.GoName)
			default:
				return fmt.Sprintf(`input.%s == ""`, a.GoName)
			}
		},
		"promptAssign": func(a pkg.MergedAlias) string {
			return fmt.Sprintf(
				`input.%s, err = interactive.GetString(interactive.Input{Question: %q, Help: cmd.Flags().Lookup(%q).Usage, Required: true})`,
				a.GoName, a.Description, a.Flag,
			)
		},
		"createSDKCall": func(td pkg.CobraTemplateData) string {
			return fmt.Sprintf(
				"created, err := r.SeaplaneClient.SeaplaneV1alpha1().%ss().Create(ctx, obj, platform.CreateOptions{})",
				td.ResourceName,
			)
		},
		"updateSDKCall": func(td pkg.CobraTemplateData) string {
			return fmt.Sprintf(
				"updated, err := r.SeaplaneClient.SeaplaneV1alpha1().%ss().Patch(ctx, name, types.MergePatchType, payload, platform.PatchOptions{})",
				td.ResourceName,
			)
		},
	}
}

func emitHelpersFile(outputDir, pkgName string) error {
	tmpl := template.Must(template.New("helpers").Parse(helpersTemplate))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"Package": pkgName}); err != nil {
		return fmt.Errorf("rendering helpers_gen.go: %w", err)
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("formatting helpers_gen.go: %w", err)
	}
	path := filepath.Join(outputDir, "helpers_gen.go")
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Printf("pathbind-gen: wrote %s\n", path)
	return nil
}
