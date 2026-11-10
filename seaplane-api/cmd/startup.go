package main

import (
	"os"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
)

func loadStartupConfig(region string) *config.Config {
	cfg := config.NewConfig()
	cfg.Regional.AWSRegion = region
	for _, input := range []struct {
		env    string
		target *string
	}{
		{"API_BIND_ADDRESS", &cfg.Server.APIBindAddress},
		{"HEALTH_BIND_ADDRESS", &cfg.Server.HealthBindAddress},
		{"METRICS_BIND_ADDRESS", &cfg.Server.MetricsBindAddress},
	} {
		if value := os.Getenv(input.env); value != "" {
			*input.target = value
		}
	}
	return cfg
}
