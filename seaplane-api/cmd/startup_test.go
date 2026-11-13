package main

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestStartupFailureLogs(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AUTHZ_POLICY_TABLE", "")
	output, err := os.CreateTemp(t.TempDir(), "startup-log")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = output
	t.Cleanup(func() { os.Stdout = original; _ = output.Close() })
	if err := runServe(&cobra.Command{}, nil); err == nil {
		t.Fatal("invalid startup succeeded")
	}
	content, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "authorization startup failed") || !strings.Contains(string(content), "failed to initialize authorization service") {
		t.Fatalf("missing startup diagnostic: %s", content)
	}
}

func TestStartupDefaultsAndBinds(t *testing.T) {
	for _, address := range []string{"", "127.0.0.1"} {
		t.Run("bind="+address, func(t *testing.T) {
			t.Setenv("API_BIND_ADDRESS", address)
			t.Setenv("HEALTH_BIND_ADDRESS", address)
			t.Setenv("METRICS_BIND_ADDRESS", address)
			cfg := loadStartupConfig("us-east-1")
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
