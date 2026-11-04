package authz

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const managementYAML = `formatVersion: 1
policyTable: authz-policies
globalTable: authz-global
regionalTable: authz-regional
accounts:
  "111111111111":
    writerRegion: us-east-1
    administratorARNs:
      - arn:aws:iam::111111111111:role/path/admin
`

// TestManagementConfig rejects coercions, unknown fields, and invalid bootstrap identities.
func TestManagementConfig(t *testing.T) {
	cases := map[string]string{
		"valid":           managementYAML,
		"version":         strings.Replace(managementYAML, "formatVersion: 1", "formatVersion: 2", 1),
		"missing version": strings.Replace(managementYAML, "formatVersion: 1\n", "", 1),
		"unknown":         managementYAML + "unknown: value\n",
		"unknown nested":  managementYAML + "    unknown: value\n",
		"duplicate":       managementYAML + "policyTable: duplicate\n",
		"trailing":        managementYAML + "---\n{}\n",
		"numeric account": strings.Replace(managementYAML, `"111111111111"`, "111111111111", 1),
		"foreign admin":   strings.Replace(managementYAML, "arn:aws:iam::111111111111", "arn:aws:iam::222222222222", 1),
		"null admins":     strings.Replace(managementYAML, "administratorARNs:\n      - arn:aws:iam::111111111111:role/path/admin", "administratorARNs: null", 1),
		"null accounts":   strings.Split(managementYAML, "accounts:")[0] + "accounts: null\n",
		"duplicate table": strings.Replace(managementYAML, "authz-regional", "authz-global", 1),
		"invalid table":   strings.Replace(managementYAML, "authz-regional", "x/y", 1),
		"invalid region":  strings.Replace(managementYAML, "us-east-1", "bad", 1),
		"anchor":          strings.Replace(managementYAML, "writerRegion: us-east-1", "writerRegion: &region us-east-1", 1),
		"role alias":      managementYAML + "      - arn:aws:iam::111111111111:role/other/admin\n",
		"duplicate admin": managementYAML + "      - arn:aws:iam::111111111111:role/path/admin\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management.yaml")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadManagementConfig(path)
			if name == "valid" {
				if err != nil || cfg.Accounts["111111111111"].WriterRegion != "us-east-1" {
					t.Fatalf("config = %+v, %v", cfg, err)
				}
			} else if !errors.Is(err, ErrValidation) {
				t.Fatalf("accepted invalid config: %+v, %v", cfg, err)
			}
		})
	}
	if _, err := LoadManagementConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing file")
	}
}

// TestManagementActionGroups keeps management grants out of resource action groups.
func TestManagementActionGroups(t *testing.T) {
	model, _, err := loadSchema()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CreatePolicy", "ListPolicies", "DescribePolicy", "UpdatePolicy", "DeletePolicy", "CreateAttachment", "ListAttachments", "DeleteAttachment", "CreateAttachmentRegional", "ListAttachmentsRegional", "DeleteAttachmentRegional"} {
		action := model.Actions[entityUID("Action", name)].Entity
		for _, group := range []string{"AllActions", "ReadOnly"} {
			if action.Parents.Contains(entityUID("Action", group)) {
				t.Fatalf("%s inherits %s", name, group)
			}
		}
	}
}
