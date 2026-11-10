package config

import (
	"testing"
	"time"
)

func TestNewConfig(t *testing.T) {
	cfg := NewConfig()

	if cfg == nil {
		t.Fatal("expected non-nil config")
	}

	// Test Server config defaults
	if cfg.Server.APIBindAddress != "0.0.0.0" {
		t.Errorf("expected APIBindAddress=0.0.0.0, got %s", cfg.Server.APIBindAddress)
	}

	if cfg.Server.APIPort != 8000 {
		t.Errorf("expected APIPort=8000, got %d", cfg.Server.APIPort)
	}

	if cfg.Server.HealthBindAddress != "0.0.0.0" {
		t.Errorf("expected HealthBindAddress=0.0.0.0, got %s", cfg.Server.HealthBindAddress)
	}

	if cfg.Server.HealthPort != 8080 {
		t.Errorf("expected HealthPort=8080, got %d", cfg.Server.HealthPort)
	}

	if cfg.Server.MetricsBindAddress != "0.0.0.0" {
		t.Errorf("expected MetricsBindAddress=0.0.0.0, got %s", cfg.Server.MetricsBindAddress)
	}

	if cfg.Server.MetricsPort != 9090 {
		t.Errorf("expected MetricsPort=9090, got %d", cfg.Server.MetricsPort)
	}

	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("expected ShutdownTimeout=30s, got %v", cfg.Server.ShutdownTimeout)
	}

	// Test Logging config defaults
	if cfg.Logging.Level != "info" {
		t.Errorf("expected Logging.Level=info, got %s", cfg.Logging.Level)
	}

	if cfg.Logging.Format != "json" {
		t.Errorf("expected Logging.Format=json, got %s", cfg.Logging.Format)
	}

}

func TestServerConfig(t *testing.T) {
	cfg := ServerConfig{
		APIBindAddress:     "127.0.0.1",
		APIPort:            9000,
		HealthBindAddress:  "127.0.0.1",
		HealthPort:         9080,
		MetricsBindAddress: "127.0.0.1",
		MetricsPort:        9999,
		ShutdownTimeout:    60 * time.Second,
	}

	if cfg.APIBindAddress != "127.0.0.1" {
		t.Errorf("expected APIBindAddress=127.0.0.1, got %s", cfg.APIBindAddress)
	}

	if cfg.APIPort != 9000 {
		t.Errorf("expected APIPort=9000, got %d", cfg.APIPort)
	}

	if cfg.HealthBindAddress != "127.0.0.1" {
		t.Errorf("expected HealthBindAddress=127.0.0.1, got %s", cfg.HealthBindAddress)
	}

	if cfg.HealthPort != 9080 {
		t.Errorf("expected HealthPort=9080, got %d", cfg.HealthPort)
	}

	if cfg.MetricsBindAddress != "127.0.0.1" {
		t.Errorf("expected MetricsBindAddress=127.0.0.1, got %s", cfg.MetricsBindAddress)
	}

	if cfg.MetricsPort != 9999 {
		t.Errorf("expected MetricsPort=9999, got %d", cfg.MetricsPort)
	}

	if cfg.ShutdownTimeout != 60*time.Second {
		t.Errorf("expected ShutdownTimeout=60s, got %v", cfg.ShutdownTimeout)
	}
}

func TestLoggingConfig(t *testing.T) {
	tests := []struct {
		name   string
		level  string
		format string
	}{
		{
			name:   "info json",
			level:  "info",
			format: "json",
		},
		{
			name:   "debug text",
			level:  "debug",
			format: "text",
		},
		{
			name:   "error json",
			level:  "error",
			format: "json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := LoggingConfig{
				Level:  tt.level,
				Format: tt.format,
			}

			if cfg.Level != tt.level {
				t.Errorf("expected Level=%s, got %s", tt.level, cfg.Level)
			}

			if cfg.Format != tt.format {
				t.Errorf("expected Format=%s, got %s", tt.format, cfg.Format)
			}
		})
	}
}

func TestConfig_CustomValues(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIBindAddress:     "192.168.1.1",
			APIPort:            3000,
			HealthBindAddress:  "192.168.1.1",
			HealthPort:         3001,
			MetricsBindAddress: "192.168.1.1",
			MetricsPort:        3002,
			ShutdownTimeout:    15 * time.Second,
		},
		Logging: LoggingConfig{
			Level:  "debug",
			Format: "text",
		},
	}

	// Verify Server config
	if cfg.Server.APIBindAddress != "192.168.1.1" {
		t.Errorf("expected APIBindAddress=192.168.1.1, got %s", cfg.Server.APIBindAddress)
	}

	if cfg.Server.APIPort != 3000 {
		t.Errorf("expected APIPort=3000, got %d", cfg.Server.APIPort)
	}

	// Verify Logging config
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected Logging.Level=debug, got %s", cfg.Logging.Level)
	}

	if cfg.Logging.Format != "text" {
		t.Errorf("expected Logging.Format=text, got %s", cfg.Logging.Format)
	}

}
