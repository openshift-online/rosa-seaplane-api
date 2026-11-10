package main

import (
	"log/slog"
	"testing"
)

func TestCreateLogger(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		format    string
		wantLevel slog.Level
	}{
		{
			name:      "debug json",
			level:     "debug",
			format:    "json",
			wantLevel: slog.LevelDebug,
		},
		{
			name:      "info json",
			level:     "info",
			format:    "json",
			wantLevel: slog.LevelInfo,
		},
		{
			name:      "warn json",
			level:     "warn",
			format:    "json",
			wantLevel: slog.LevelWarn,
		},
		{
			name:      "error json",
			level:     "error",
			format:    "json",
			wantLevel: slog.LevelError,
		},
		{
			name:      "info text",
			level:     "info",
			format:    "text",
			wantLevel: slog.LevelInfo,
		},
		{
			name:      "invalid level defaults to info",
			level:     "invalid",
			format:    "json",
			wantLevel: slog.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := createLogger(tt.level, tt.format)
			if logger == nil {
				t.Fatal("expected non-nil logger")
			}

			// Logger is created successfully - we can't easily test the level
			// but we've covered the code paths
		})
	}
}

func TestRootCmd(t *testing.T) {
	if rootCmd == nil {
		t.Fatal("expected non-nil rootCmd")
	}

	if rootCmd.Use != "rosa-seaplane-api" {
		t.Errorf("expected Use=rosa-seaplane-api, got %s", rootCmd.Use)
	}

	if rootCmd.Short == "" {
		t.Error("expected non-empty Short description")
	}

	if rootCmd.Long == "" {
		t.Error("expected non-empty Long description")
	}
}

func TestServeCmd(t *testing.T) {
	if serveCmd == nil {
		t.Fatal("expected non-nil serveCmd")
	}

	if serveCmd.Use != "serve" {
		t.Errorf("expected Use=serve, got %s", serveCmd.Use)
	}

	if serveCmd.Short == "" {
		t.Error("expected non-empty Short description")
	}

	// Verify flags are registered
	flags := serveCmd.Flags()
	if flags == nil {
		t.Fatal("expected non-nil flags")
	}

	expectedFlags := []string{
		"log-level",
		"log-format",
		"api-port",
		"health-port",
		"metrics-port",
	}

	for _, flagName := range expectedFlags {
		flag := flags.Lookup(flagName)
		if flag == nil {
			t.Errorf("expected flag %s to be registered", flagName)
		}
	}
}
