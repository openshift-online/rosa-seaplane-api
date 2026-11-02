package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	if server == nil {
		t.Fatal("expected non-nil server")
	}

	if server.cfg == nil {
		t.Error("expected non-nil config")
	}

	if server.logger == nil {
		t.Error("expected non-nil logger")
	}

	if server.apiServer == nil {
		t.Error("expected non-nil apiServer")
	}

	if server.healthServer == nil {
		t.Error("expected non-nil healthServer")
	}

	if server.metricsServer == nil {
		t.Error("expected non-nil metricsServer")
	}

	if server.healthHandler == nil {
		t.Error("expected non-nil healthHandler")
	}
}

func TestNew_WithCustomConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIBindAddress:     "127.0.0.1",
			APIPort:            9000,
			HealthBindAddress:  "127.0.0.1",
			HealthPort:         9001,
			MetricsBindAddress: "127.0.0.1",
			MetricsPort:        9002,
			ShutdownTimeout:    15 * time.Second,
		},
		Logging: config.LoggingConfig{
			Level:  "debug",
			Format: "text",
		},
	}

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	if server.apiServer.Addr != "127.0.0.1:9000" {
		t.Errorf("expected apiServer.Addr=127.0.0.1:9000, got %s", server.apiServer.Addr)
	}

	if server.healthServer.Addr != "127.0.0.1:9001" {
		t.Errorf("expected healthServer.Addr=127.0.0.1:9001, got %s", server.healthServer.Addr)
	}

	if server.metricsServer.Addr != "127.0.0.1:9002" {
		t.Errorf("expected metricsServer.Addr=127.0.0.1:9002, got %s", server.metricsServer.Addr)
	}
}

func TestServer_HealthRoutes(t *testing.T) {
	// Operational endpoints remain accessible without identity.
	t.Setenv("TARGET_GROUP_ARN", "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/test/id")
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	tests := []struct {
		name           string
		path           string
		expectedStatus int
	}{
		{
			name:           "liveness on API server",
			path:           "/api/v0/live",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "readiness on API server",
			path:           "/api/v0/ready",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "info on API server",
			path:           "/api/v0/info",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			server.apiServer.Handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestServer_IdentityMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	// Test that identity middleware extracts headers
	req := httptest.NewRequest(http.MethodGet, "/api/v0/live", nil)
	req.Header.Set(middleware.HeaderAccountID, "123456789012")
	req.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::123456789012:user/test")
	req.Header.Set(middleware.HeaderRequestID, "test-request-123")

	w := httptest.NewRecorder()
	server.apiServer.Handler.ServeHTTP(w, req)

	// The health endpoint should still return OK
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}

func TestServer_NodePoolUpdateRouteAcceptsPatch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	server, err := newConfiguredServer(t, config.NewConfig(), nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	request := httptest.NewRequest(http.MethodPatch, "/api/v0/nodepools/test-id", nil)
	recorder := httptest.NewRecorder()
	server.apiServer.Handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("PATCH nodepool status = %d, want %d (route matched and reached authorization)", recorder.Code, http.StatusForbidden)
	}
}

func TestServer_RequiresIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).Build(), logger)
	srv, err := newConfiguredServer(t, config.NewConfig(), db, logger)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/api/v0/clusters", "/api/v0/nodepools", "/api/v0/oidc_configs",
	} {
		for _, tc := range []struct {
			name, accountID, callerARN string
			wantStatus                 int
		}{
			{"no identity", "", "", http.StatusForbidden},
			{"missing ARN", "123456789012", "", http.StatusForbidden},
			{"missing account", "", "arn:aws:iam::123456789012:user/test", http.StatusForbidden},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(middleware.HeaderAccountID, tc.accountID)
				req.Header.Set(middleware.HeaderCallerARN, tc.callerARN)
				w := httptest.NewRecorder()
				srv.apiServer.Handler.ServeHTTP(w, req)
				if w.Code != tc.wantStatus {
					t.Errorf("want %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestServer_MetricsRoute(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	server.metricsServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	// Verify it returns Prometheus metrics format
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty metrics response")
	}
}

func TestServer_HealthServerRoutes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	tests := []struct {
		name           string
		path           string
		expectedStatus int
	}{
		{
			name:           "healthz endpoint",
			path:           "/healthz",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "readyz endpoint",
			path:           "/readyz",
			expectedStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			server.healthServer.Handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestServer_InvalidRoutes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	tests := []struct {
		name           string
		path           string
		expectedStatus int
	}{
		{
			name:           "invalid API path",
			path:           "/api/v0/invalid",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "root path",
			path:           "/",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "invalid health path",
			path:           "/health",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			server.apiServer.Handler.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestServer_ReadinessToggle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	// Initially should be ready
	req := httptest.NewRequest(http.MethodGet, "/api/v0/ready", nil)
	w := httptest.NewRecorder()
	server.apiServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 when ready, got %d", w.Code)
	}

	// Set not ready
	server.healthHandler.SetReady(false)

	req = httptest.NewRequest(http.MethodGet, "/api/v0/ready", nil)
	w = httptest.NewRecorder()
	server.apiServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503 when not ready, got %d", w.Code)
	}

	// Set ready again
	server.healthHandler.SetReady(true)

	req = httptest.NewRequest(http.MethodGet, "/api/v0/ready", nil)
	w = httptest.NewRecorder()
	server.apiServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 when ready again, got %d", w.Code)
	}
}

func TestServer_ServerAddresses(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIBindAddress:     "0.0.0.0",
			APIPort:            8000,
			HealthBindAddress:  "0.0.0.0",
			HealthPort:         8080,
			MetricsBindAddress: "0.0.0.0",
			MetricsPort:        9090,
			ShutdownTimeout:    30 * time.Second,
		},
		Logging: config.LoggingConfig{
			Level:  "info",
			Format: "json",
		},
	}

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	if server.apiServer.Addr != "0.0.0.0:8000" {
		t.Errorf("expected apiServer.Addr=0.0.0.0:8000, got %s", server.apiServer.Addr)
	}

	if server.healthServer.Addr != "0.0.0.0:8080" {
		t.Errorf("expected healthServer.Addr=0.0.0.0:8080, got %s", server.healthServer.Addr)
	}

	if server.metricsServer.Addr != "0.0.0.0:9090" {
		t.Errorf("expected metricsServer.Addr=0.0.0.0:9090, got %s", server.metricsServer.Addr)
	}

	// Verify timeouts — all three servers must have ReadHeaderTimeout to prevent
	// Slowloris DoS attacks where the client sends headers very slowly.
	if server.apiServer.ReadHeaderTimeout == 0 {
		t.Error("apiServer.ReadHeaderTimeout must be set (Slowloris protection)")
	}
	if server.apiServer.ReadTimeout != 30*time.Second {
		t.Errorf("expected apiServer.ReadTimeout=30s, got %v", server.apiServer.ReadTimeout)
	}
	if server.apiServer.WriteTimeout != 30*time.Second {
		t.Errorf("expected apiServer.WriteTimeout=30s, got %v", server.apiServer.WriteTimeout)
	}
	if server.apiServer.IdleTimeout == 0 {
		t.Error("apiServer.IdleTimeout must be set")
	}

	if server.healthServer.ReadHeaderTimeout == 0 {
		t.Error("healthServer.ReadHeaderTimeout must be set (Slowloris protection)")
	}
	if server.healthServer.ReadTimeout != 10*time.Second {
		t.Errorf("expected healthServer.ReadTimeout=10s, got %v", server.healthServer.ReadTimeout)
	}
	if server.healthServer.WriteTimeout != 10*time.Second {
		t.Errorf("expected healthServer.WriteTimeout=10s, got %v", server.healthServer.WriteTimeout)
	}
	if server.healthServer.IdleTimeout == 0 {
		t.Error("healthServer.IdleTimeout must be set")
	}

	if server.metricsServer.ReadHeaderTimeout == 0 {
		t.Error("metricsServer.ReadHeaderTimeout must be set (Slowloris protection)")
	}
	if server.metricsServer.IdleTimeout == 0 {
		t.Error("metricsServer.IdleTimeout must be set")
	}

	const wantMaxHeaderBytes = 1 << 14 // 16 KiB
	if server.apiServer.MaxHeaderBytes != wantMaxHeaderBytes {
		t.Errorf("expected apiServer.MaxHeaderBytes=%d (16 KiB), got %d", wantMaxHeaderBytes, server.apiServer.MaxHeaderBytes)
	}
}

func TestServer_TimeoutOverrides(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()
	cfg.Server.ReadHeaderTimeout = 5 * time.Second
	cfg.Server.ReadTimeout = 15 * time.Second
	cfg.Server.WriteTimeout = 20 * time.Second
	cfg.Server.IdleTimeout = 45 * time.Second
	cfg.Server.MaxHeaderBytes = 8192 // 8 KiB

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	if server.apiServer.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("expected ReadHeaderTimeout=5s, got %v", server.apiServer.ReadHeaderTimeout)
	}
	if server.apiServer.ReadTimeout != 15*time.Second {
		t.Errorf("expected ReadTimeout=15s, got %v", server.apiServer.ReadTimeout)
	}
	if server.apiServer.WriteTimeout != 20*time.Second {
		t.Errorf("expected WriteTimeout=20s, got %v", server.apiServer.WriteTimeout)
	}
	if server.apiServer.IdleTimeout != 45*time.Second {
		t.Errorf("expected IdleTimeout=45s, got %v", server.apiServer.IdleTimeout)
	}
	if server.apiServer.MaxHeaderBytes != 8192 {
		t.Errorf("expected MaxHeaderBytes=8192, got %d", server.apiServer.MaxHeaderBytes)
	}
}

func TestServer_ReadHeaderTimeoutCappedAtReadTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig()
	cfg.Server.ReadHeaderTimeout = 60 * time.Second // exceeds ReadTimeout
	cfg.Server.ReadTimeout = 20 * time.Second

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	// ReadHeaderTimeout must not exceed ReadTimeout — if it did, the header
	// deadline would never fire before the total read deadline cuts it off.
	if server.apiServer.ReadHeaderTimeout > server.apiServer.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) exceeds ReadTimeout (%v); header deadline would never fire",
			server.apiServer.ReadHeaderTimeout, server.apiServer.ReadTimeout)
	}
	if server.apiServer.ReadHeaderTimeout != 20*time.Second {
		t.Errorf("expected ReadHeaderTimeout capped at ReadTimeout (20s), got %v", server.apiServer.ReadHeaderTimeout)
	}
}

func TestServer_TimeoutOverridesZeroUsesDefault(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.NewConfig() // all timeout fields are zero — must fall back to hardcoded defaults

	server, err := newConfiguredServer(t, cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error creating server: %v", err)
	}

	if server.apiServer.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("expected default ReadHeaderTimeout=10s, got %v", server.apiServer.ReadHeaderTimeout)
	}
	if server.apiServer.ReadTimeout != 30*time.Second {
		t.Errorf("expected default ReadTimeout=30s, got %v", server.apiServer.ReadTimeout)
	}
	if server.apiServer.WriteTimeout != 30*time.Second {
		t.Errorf("expected default WriteTimeout=30s, got %v", server.apiServer.WriteTimeout)
	}
	if server.apiServer.IdleTimeout != 60*time.Second {
		t.Errorf("expected default IdleTimeout=60s, got %v", server.apiServer.IdleTimeout)
	}
	if server.apiServer.MaxHeaderBytes != 1<<14 {
		t.Errorf("expected default MaxHeaderBytes=%d, got %d", 1<<14, server.apiServer.MaxHeaderBytes)
	}
}
