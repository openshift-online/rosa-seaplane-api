package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const startupBundle = `formatVersion: 1
registeredAccounts: ["123456789012"]
policies:
  - id: read
    ownerAccountID: "123456789012"
    content: 'permit(principal, action in Seaplane::Action::"ReadOnly", resource);'
attachments:
  - id: user-read
    policyID: read
    principalARN: arn:aws:iam::123456789012:user/reader
    scope: global
`

func startupFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authz.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startupCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	flag := serveCmd.Flags().Lookup("authz-config-file")
	if flag == nil {
		t.Fatal("missing startup flag authz-config-file")
	}
	cmd.Flags().String(flag.Name, flag.DefValue, flag.Usage)
	cmd.Flags().String("authz-management-config-file", "", "Optional management configuration")
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestStartupFailureLogs(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, strings.Replace(startupBundle, "policyID: read", "policyID: missing", 1)))
	output, err := os.CreateTemp(t.TempDir(), "startup-log")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = output
	t.Cleanup(func() {
		os.Stdout = original
		_ = output.Close()
	})
	if err := runServe(startupCommand(t), nil); err == nil {
		t.Fatal("invalid startup succeeded")
	}
	content, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"authorization startup failed", `"stage":"binding"`, "dangling policy reference", `"AttachmentID":"user-read"`} {
		if !strings.Contains(string(content), detail) {
			t.Errorf("startup log missing %q: %s", detail, content)
		}
	}
}

func TestStartupInputs(t *testing.T) {
	valid := startupFile(t, startupBundle)
	invalid := startupFile(t, "not a bundle")
	for _, tc := range []struct {
		name, fileEnv, wantError string
		args                     []string
	}{
		{"required file", "", "AUTHZ_CONFIG_FILE", nil},
		{"missing file", filepath.Join(t.TempDir(), "missing"), "load authorization", nil},
		{"valid environment", valid, "", nil},
		{"flag beats file env", invalid, "", []string{"--authz-config-file=" + valid}},
		{"empty file flag beats env", valid, "AUTHZ_CONFIG_FILE", []string{"--authz-config-file="}},
		{"invalid file flag beats env", valid, "load authorization", []string{"--authz-config-file=" + invalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AUTHZ_CONFIG_FILE", tc.fileEnv)
			cfg, authorizer, err := loadStartupConfig(startupCommand(t, tc.args...), "us-east-1")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || cfg != nil || authorizer != nil {
					t.Fatalf("want %q and no usable startup dependencies, got cfg=%v authorizer=%v err=%v", tc.wantError, cfg, authorizer, err)
				}
				return
			}
			if err != nil || cfg == nil || authorizer == nil {
				t.Fatalf("valid startup rejected: %v", err)
			}
			if cfg.Authz.ConfigFile != valid || cfg.Regional.AWSRegion != "us-east-1" || !authorizer.IsAccountRegistered(t.Context(), "123456789012") {
				t.Fatalf("wrong startup configuration: %+v", cfg)
			}
		})
	}
}

func TestStartupDefaultsAndBinds(t *testing.T) {
	t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
	for _, address := range []string{"", "127.0.0.1"} {
		t.Run("bind="+address, func(t *testing.T) {
			t.Setenv("API_BIND_ADDRESS", address)
			t.Setenv("HEALTH_BIND_ADDRESS", address)
			t.Setenv("METRICS_BIND_ADDRESS", address)
			cfg, _, err := loadStartupConfig(startupCommand(t), "us-east-1")
			if err != nil {
				t.Fatal(err)
			}
			want := address
			if want == "" {
				want = "0.0.0.0"
			}
			if cfg.Server.APIBindAddress != want || cfg.Server.HealthBindAddress != want || cfg.Server.MetricsBindAddress != want || cfg.Server.GRPCBindAddress != "0.0.0.0" || cfg.Server.APIPort != 8000 || cfg.Server.HealthPort != 8080 || cfg.Server.MetricsPort != 9090 {
				t.Fatalf("defaults or bind inputs changed: %+v", cfg)
			}
		})
	}
}
