package config

import "time"

type Config struct {
	Server    ServerConfig
	DB        DBConfig
	Regional  RegionalConfig
	Logging   LoggingConfig
	RateLimit RateLimitConfig
	Authz     AuthzConfig
}

type AuthzConfig struct {
	ConfigFile string
}

type RateLimitConfig struct {
	Enabled       bool
	RedisAddr     string
	InMemory      bool
	ConfigFile    string
	DefaultRate   int
	DefaultBurst  int
	DefaultWindow int
}

type DBConfig struct {
	DSN string
}

type RegionalConfig struct {
	OIDCIssuerBaseURL        string
	DefaultClusterExpiration time.Duration
	// AWSRegion is the fixed service region used for authorization and logs.
	AWSRegion string
	// AccountID is the platform's own AWS account, the only caller of the account allowlist.
	AccountID string
}

type ServerConfig struct {
	APIBindAddress      string
	APIPort             int
	GRPCBindAddress     string
	GRPCPort            int
	HealthBindAddress   string
	HealthPort          int
	MetricsBindAddress  string
	MetricsPort         int
	ShutdownTimeout     time.Duration
	MaxRequestBodyBytes int64 // maximum request body size in bytes; 0 uses the middleware default (10 MiB)

	// API server timeout overrides — 0 means use the server package default.
	ReadHeaderTimeout time.Duration // default 10s; prevents Slowloris attacks
	ReadTimeout       time.Duration // default 30s; bounds total request read time
	WriteTimeout      time.Duration // default 30s; bounds total response write time
	IdleTimeout       time.Duration // default 60s; closes idle keep-alive connections

	MaxHeaderBytes int // maximum request header size in bytes; 0 uses the server package default (16 KiB)
}

type LoggingConfig struct {
	Level  string
	Format string
}

func NewConfig() *Config {
	return &Config{
		Server: ServerConfig{
			APIBindAddress:     "0.0.0.0",
			APIPort:            8000,
			GRPCBindAddress:    "0.0.0.0",
			GRPCPort:           8090,
			HealthBindAddress:  "0.0.0.0",
			HealthPort:         8080,
			MetricsBindAddress: "0.0.0.0",
			MetricsPort:        9090,
			ShutdownTimeout:    30 * time.Second,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
	}
}
