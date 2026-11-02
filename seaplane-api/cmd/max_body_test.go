package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

func TestApplyServerLimitConfig_ValidEnvVars(t *testing.T) {
	t.Setenv("MAX_REQUEST_BODY_BYTES", "2097152") // 2 MiB
	t.Setenv("READ_HEADER_TIMEOUT_SECONDS", "5")
	t.Setenv("READ_TIMEOUT_SECONDS", "15")
	t.Setenv("WRITE_TIMEOUT_SECONDS", "20")
	t.Setenv("IDLE_TIMEOUT_SECONDS", "45")
	t.Setenv("MAX_REQUEST_HEADER_BYTES", "8192")

	cfg := config.NewConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	applyServerLimitConfig(cfg, logger)

	if cfg.Server.MaxRequestBodyBytes != 2097152 {
		t.Errorf("MaxRequestBodyBytes: got %d, want 2097152", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout: got %v, want 5s", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.ReadTimeout != 15*time.Second {
		t.Errorf("ReadTimeout: got %v, want 15s", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 20*time.Second {
		t.Errorf("WriteTimeout: got %v, want 20s", cfg.Server.WriteTimeout)
	}
	if cfg.Server.IdleTimeout != 45*time.Second {
		t.Errorf("IdleTimeout: got %v, want 45s", cfg.Server.IdleTimeout)
	}
	if cfg.Server.MaxHeaderBytes != 8192 {
		t.Errorf("MaxHeaderBytes: got %d, want 8192", cfg.Server.MaxHeaderBytes)
	}
}

func TestApplyServerLimitConfig_InvalidEnvVarsLeaveFieldsZero(t *testing.T) {
	t.Setenv("MAX_REQUEST_BODY_BYTES", "invalid")
	t.Setenv("READ_HEADER_TIMEOUT_SECONDS", "-1")
	t.Setenv("READ_TIMEOUT_SECONDS", "0")
	t.Setenv("WRITE_TIMEOUT_SECONDS", "abc")
	t.Setenv("IDLE_TIMEOUT_SECONDS", "1.5")
	t.Setenv("MAX_REQUEST_HEADER_BYTES", "bad")

	cfg := config.NewConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	applyServerLimitConfig(cfg, logger)

	if cfg.Server.MaxRequestBodyBytes != 0 {
		t.Errorf("MaxRequestBodyBytes: expected 0 on invalid input, got %d", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.ReadHeaderTimeout != 0 {
		t.Errorf("ReadHeaderTimeout: expected 0 on invalid input, got %v", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.ReadTimeout != 0 {
		t.Errorf("ReadTimeout: expected 0 on invalid input, got %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 0 {
		t.Errorf("WriteTimeout: expected 0 on invalid input, got %v", cfg.Server.WriteTimeout)
	}
	if cfg.Server.IdleTimeout != 0 {
		t.Errorf("IdleTimeout: expected 0 on invalid input, got %v", cfg.Server.IdleTimeout)
	}
	if cfg.Server.MaxHeaderBytes != 0 {
		t.Errorf("MaxHeaderBytes: expected 0 on invalid input, got %d", cfg.Server.MaxHeaderBytes)
	}
}

func TestApplyServerLimitConfig_AbsentEnvVarsLeaveFieldsZero(t *testing.T) {
	cfg := config.NewConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	applyServerLimitConfig(cfg, logger)

	if cfg.Server.MaxRequestBodyBytes != 0 {
		t.Errorf("MaxRequestBodyBytes: expected 0 when env var absent, got %d", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.ReadHeaderTimeout != 0 {
		t.Errorf("ReadHeaderTimeout: expected 0 when env var absent, got %v", cfg.Server.ReadHeaderTimeout)
	}
	if cfg.Server.MaxHeaderBytes != 0 {
		t.Errorf("MaxHeaderBytes: expected 0 when env var absent, got %d", cfg.Server.MaxHeaderBytes)
	}
}

func TestParseDurationSeconds(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantD  time.Duration
		wantOK bool
	}{
		{"empty string", "", 0, false},
		{"valid 30s", "30", 30 * time.Second, true},
		{"valid 1s", "1", time.Second, true},
		{"zero rejected", "0", 0, false},
		{"negative rejected", "-5", 0, false},
		{"non-numeric", "fast", 0, false},
		{"float rejected", "1.5", 0, false},
		{"whitespace", " ", 0, false},
		{"large valid", "3600", 3600 * time.Second, true},
		{"overflow rejected", "9223372037", 0, false}, // > math.MaxInt64/time.Second
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotD, gotOK := parseDurationSeconds(tt.input)
			if gotOK != tt.wantOK {
				t.Errorf("ok: got %v, want %v", gotOK, tt.wantOK)
			}
			if gotD != tt.wantD {
				t.Errorf("duration: got %v, want %v", gotD, tt.wantD)
			}
		})
	}
}

func TestParseMaxRequestBodyBytes(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantN  int64
		wantOK bool
	}{
		{"empty string", "", 0, false},
		{"valid positive", "1048576", 1048576, true},
		{"valid small", "64", 64, true},
		{"zero rejected", "0", 0, false},
		{"negative rejected", "-1", 0, false},
		{"non-numeric", "abc", 0, false},
		{"float rejected", "1.5", 0, false},
		{"whitespace", " ", 0, false},
		{"very large", "10737418240", 10737418240, true}, // 10 GiB — valid even if impractical
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotN, gotOK := parseMaxRequestBodyBytes(tt.input)
			if gotOK != tt.wantOK {
				t.Errorf("ok: got %v, want %v", gotOK, tt.wantOK)
			}
			if gotN != tt.wantN {
				t.Errorf("n: got %d, want %d", gotN, tt.wantN)
			}
		})
	}
}
