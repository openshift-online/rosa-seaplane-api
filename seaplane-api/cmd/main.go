package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/spf13/cobra"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/server"
)

var (
	// Config flags
	logLevel                 string
	logFormat                string
	legacyDynamoDBRegion     string
	postgresDSN              string
	oidcIssuerBaseURL        string
	defaultClusterExpiration time.Duration
	apiPort                  int
	healthPort               int
	metricsPort              int
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "rosa-seaplane-api",
	Short: "Seaplane Platform API",
	Long:  "Seaplane platform API for ROSA HCP regional cluster management",
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the API server",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	serveCmd.Flags().StringVar(&logFormat, "log-format", "json", "Log format (json, text)")
	serveCmd.Flags().String("allowed-accounts", "", "Deprecated compatibility flag; ignored")
	serveCmd.Flags().String("authz-config-file", "", "Authorization configuration bundle (required)")
	serveCmd.Flags().StringVar(&legacyDynamoDBRegion, "dynamodb-region", "", "Deprecated compatibility flag; used only as a region fallback")
	serveCmd.Flags().String("dynamodb-prefix", "", "Deprecated compatibility flag; ignored")
	serveCmd.Flags().StringVar(&postgresDSN, "postgres-dsn", "", "PostgreSQL connection string (required)")
	serveCmd.Flags().StringVar(&oidcIssuerBaseURL, "oidc-issuer-base-url", "", "Base URL for OIDC issuer (e.g. https://<cloudfront-domain>)")
	serveCmd.Flags().DurationVar(&defaultClusterExpiration, "default-cluster-expiration", 0, "Default cluster lifetime (e.g. 24h). Clusters created without an explicit expirationTimestamp get one stamped at creation. Zero means no default.")
	serveCmd.Flags().IntVar(&apiPort, "api-port", 8000, "API server port")
	serveCmd.Flags().IntVar(&healthPort, "health-port", 8080, "Health check server port")
	serveCmd.Flags().IntVar(&metricsPort, "metrics-port", 9090, "Metrics server port")

	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// Create logger
	logger := createLogger(logLevel, logFormat)

	logger.Info(
		"starting rosa-seaplane-api",
		"log_level", logLevel,
		"log_format", logFormat,
	)

	// Detect AWS region from SDK default chain (IMDS, AWS_REGION env var, etc.)
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return fmt.Errorf("failed to detect AWS region: %w", err)
	}
	if awsCfg.Region == "" && legacyDynamoDBRegion != "" {
		awsCfg.Region = legacyDynamoDBRegion
	}
	if awsCfg.Region == "" {
		return fmt.Errorf("AWS region could not be detected from environment; set AWS_REGION")
	}
	logger.Info("detected AWS region", "region", awsCfg.Region)

	// Validate authorization before database connections or serving listeners.
	cfg, authorizer, err := loadStartupConfig(cmd, awsCfg.Region)
	if err != nil {
		if failure, ok := errors.AsType[*authz.Failure](err); ok {
			logger.Error("authorization startup failed", "stage", failure.Stage, "cause", failure.Err, "provenance", failure.Provenance, "diagnostics", failure.Diagnostics)
		} else {
			logger.Error("authorization startup failed", "error", err)
		}
		return err
	}

	cfg.Regional.AccountID = os.Getenv("AWS_ACCOUNT_ID")
	if cfg.Regional.AccountID != "" && !accountIDPattern.MatchString(cfg.Regional.AccountID) {
		return fmt.Errorf("AWS_ACCOUNT_ID must be a 12-digit AWS account ID")
	}

	// Management clusters to register on startup, replacing the registration endpoint.
	mgmtClusters, err := managementClusters(os.Getenv("MANAGEMENT_CLUSTERS"), awsCfg.Region)
	if err != nil {
		return err
	}

	cfg.Logging.Level = logLevel
	cfg.Logging.Format = logFormat
	if postgresDSN == "" {
		postgresDSN = os.Getenv("POSTGRES_DSN")
	}
	if postgresDSN == "" {
		return fmt.Errorf("--postgres-dsn or POSTGRES_DSN is required")
	}
	cfg.DB.DSN = postgresDSN

	cfg.Regional.OIDCIssuerBaseURL = oidcIssuerBaseURL
	cfg.Regional.DefaultClusterExpiration = defaultClusterExpiration
	cfg.Server.APIPort = apiPort
	cfg.Server.HealthPort = healthPort
	cfg.Server.MetricsPort = metricsPort

	// Rate limiting configuration from environment variables
	if os.Getenv("RATE_LIMIT_ENABLED") == "true" {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.RedisAddr = os.Getenv("REDIS_ENDPOINT")
		cfg.RateLimit.InMemory = os.Getenv("RATE_LIMIT_IN_MEMORY") == "true"
		cfg.RateLimit.ConfigFile = os.Getenv("RATE_LIMIT_CONFIG_FILE")
		if v := os.Getenv("RATE_LIMIT_DEFAULT_RATE"); v != "" {
			rate, err := strconv.Atoi(v)
			if err != nil {
				logger.Warn("ignoring malformed RATE_LIMIT_DEFAULT_RATE", "value", v, "error", err)
			} else {
				cfg.RateLimit.DefaultRate = rate
			}
		}
		if v := os.Getenv("RATE_LIMIT_DEFAULT_BURST"); v != "" {
			burst, err := strconv.Atoi(v)
			if err != nil {
				logger.Warn("ignoring malformed RATE_LIMIT_DEFAULT_BURST", "value", v, "error", err)
			} else {
				cfg.RateLimit.DefaultBurst = burst
			}
		}
		if v := os.Getenv("RATE_LIMIT_DEFAULT_WINDOW"); v != "" {
			window, err := strconv.Atoi(v)
			if err != nil {
				logger.Warn("ignoring malformed RATE_LIMIT_DEFAULT_WINDOW", "value", v, "error", err)
			} else {
				cfg.RateLimit.DefaultWindow = window
			}
		}
		if os.Getenv("RATE_LIMIT_TEST_MODE") == "true" {
			cfg.RateLimit.DefaultRate = 3
			cfg.RateLimit.DefaultBurst = 6
			cfg.RateLimit.DefaultWindow = 1
			cfg.RateLimit.InMemory = true
			logger.Info("rate limiting in TEST MODE (rate=3, burst=6, window=1s, in-memory)")
		} else {
			logger.Info(
				"rate limiting enabled",
				"in_memory", cfg.RateLimit.InMemory,
				"config_file", cfg.RateLimit.ConfigFile,
			)
		}
	}

	applyServerLimitConfig(cfg, logger)

	// Create seaplane DB client (Postgres via pgruntime)
	dbClient, err := seaplanedb.NewClient(context.Background(), cfg.DB.DSN, logger)
	if err != nil {
		return fmt.Errorf("failed to create seaplanedb client: %w", err)
	}
	defer dbClient.Close()

	if err := registerManagementClusters(context.Background(), dbClient, mgmtClusters, logger); err != nil {
		return err
	}

	// Create server
	srv, err := server.New(cfg, dbClient, authorizer, logger)
	if err != nil {
		return fmt.Errorf("failed to create server: %w", err)
	}

	// Setup signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run server
	logger.Info(
		"server configuration",
		"api_port", cfg.Server.APIPort,
		"health_port", cfg.Server.HealthPort,
		"metrics_port", cfg.Server.MetricsPort,
		"aws_region", awsCfg.Region,
	)

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

func createLogger(level, format string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "info":
		logLevel = slog.LevelInfo
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: logLevel,
	}

	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

// parseMaxRequestBodyBytes parses a MAX_REQUEST_BODY_BYTES string value.
// Returns (n, true) when v is a valid positive integer, (0, false) otherwise.
func parseMaxRequestBodyBytes(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// applyServerLimitConfig reads server limit env vars and applies them to cfg.
func applyServerLimitConfig(cfg *config.Config, logger *slog.Logger) {
	if n, ok := parseMaxRequestBodyBytes(os.Getenv("MAX_REQUEST_BODY_BYTES")); ok {
		cfg.Server.MaxRequestBodyBytes = n
	} else if os.Getenv("MAX_REQUEST_BODY_BYTES") != "" {
		logger.Warn("invalid MAX_REQUEST_BODY_BYTES, using default", "value", os.Getenv("MAX_REQUEST_BODY_BYTES"))
	}

	for _, t := range []struct {
		env string
		dst *time.Duration
	}{
		{"READ_HEADER_TIMEOUT_SECONDS", &cfg.Server.ReadHeaderTimeout},
		{"READ_TIMEOUT_SECONDS", &cfg.Server.ReadTimeout},
		{"WRITE_TIMEOUT_SECONDS", &cfg.Server.WriteTimeout},
		{"IDLE_TIMEOUT_SECONDS", &cfg.Server.IdleTimeout},
	} {
		if d, ok := parseDurationSeconds(os.Getenv(t.env)); ok {
			*t.dst = d
		} else if os.Getenv(t.env) != "" {
			logger.Warn("invalid timeout env var, using default", "env", t.env, "value", os.Getenv(t.env))
		}
	}

	if n, ok := parseMaxRequestBodyBytes(os.Getenv("MAX_REQUEST_HEADER_BYTES")); ok && n <= math.MaxInt {
		cfg.Server.MaxHeaderBytes = int(n)
	} else if os.Getenv("MAX_REQUEST_HEADER_BYTES") != "" {
		logger.Warn("invalid MAX_REQUEST_HEADER_BYTES, using default", "value", os.Getenv("MAX_REQUEST_HEADER_BYTES"))
	}
}

// parseDurationSeconds parses a whole-second duration string (e.g. "30").
// Returns (d, true) when v is a valid positive integer within the representable
// range of time.Duration, (0, false) otherwise.
func parseDurationSeconds(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	const maxSeconds = int64(math.MaxInt64 / time.Second)
	if n > maxSeconds {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}
