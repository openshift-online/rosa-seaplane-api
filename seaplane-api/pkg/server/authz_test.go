package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const serverBundle = `formatVersion: 1
registeredAccounts: ["123456789012"]
policies:
  - id: read
    ownerAccountID: "123456789012"
    content: 'permit(principal, action in Seaplane::Action::"ReadOnly", resource);'
  - id: unregistered-read
    ownerAccountID: "999999999999"
    content: 'permit(principal, action in Seaplane::Action::"ReadOnly", resource);'
attachments:
  - id: user-read
    policyID: read
    principalARN: arn:aws:iam::123456789012:user/test
    scope: global
  - id: unregistered-user-read
    policyID: unregistered-read
    principalARN: arn:aws:iam::999999999999:user/test
    scope: global
`

func configuredAuthorizer(t *testing.T, cfg *config.Config) *authz.Authorizer {
	t.Helper()
	cfg.Regional.AWSRegion = "us-east-1"
	cfg.Authz.ConfigFile = filepath.Join(t.TempDir(), "authz.yaml")
	if err := os.WriteFile(cfg.Authz.ConfigFile, []byte(serverBundle), 0600); err != nil {
		t.Fatal(err)
	}
	authorizer, err := authz.LoadConfig(cfg.Authz.ConfigFile, cfg.Regional.AWSRegion)
	if err != nil {
		t.Fatal(err)
	}
	return authorizer
}

func newConfiguredServer(t *testing.T, cfg *config.Config, db *seaplanedb.Client, logger *slog.Logger) (*Server, error) {
	t.Helper()
	authorizer := configuredAuthorizer(t, cfg)
	return New(cfg, db, authorizer, nil, logger)
}

func TestNewRequiresAuthorizer(t *testing.T) {
	cfg := config.NewConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, nil, nil, nil, logger)
	if err == nil || srv != nil {
		t.Fatalf("missing authorizer accepted: server=%v error=%v", srv, err)
	}
}

func authzMetricSamples(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	samples := map[string]float64{}
	for _, family := range families {
		name := family.GetName()
		if !strings.HasPrefix(name, "authz_") {
			continue
		}
		for _, metric := range family.Metric {
			labels := ""
			for _, label := range metric.Label {
				labels += "/" + label.GetName() + "=" + label.GetValue()
			}
			if metric.Counter != nil {
				samples[name+labels] = metric.Counter.GetValue()
			}
			if metric.Histogram != nil {
				samples[name+labels] = float64(metric.Histogram.GetSampleCount())
			}
		}
	}
	return samples
}

// Assert the terminal sample and the absence of intermediate retry/action outcomes.
func assertAuthzOutcome(t *testing.T, before, after map[string]float64, operation, outcome string) {
	t.Helper()
	for _, family := range []string{"authz_requests_total", "authz_duration_seconds"} {
		for _, candidate := range []string{"allow", "deny", "error"} {
			want := float64(0)
			if candidate == outcome {
				want = 1
			}
			key := family + "/operation=" + operation + "/outcome=" + candidate
			if delta := after[key] - before[key]; delta != want {
				t.Errorf("%s delta=%v, want %v", key, delta, want)
			}
		}
	}
}

func TestServerEnrollment(t *testing.T) {
	t.Setenv("TARGET_GROUP_ARN", "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/test/id")
	cfg := config.NewConfig()
	authorizer := configuredAuthorizer(t, cfg)
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).Build(), logger)
	srv, err := New(cfg, db, authorizer, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, account, caller string
		wantStatus, wantAttempts    int
	}{
		{"enrolled with grant", "/api/v0/clusters", "123456789012", "arn:aws:iam::123456789012:user/test", http.StatusOK, 1},
		{"enrolled without grant", "/api/v0/clusters", "123456789012", "arn:aws:iam::123456789012:user/no-grants", http.StatusForbidden, 1},
		{"unregistered with grant", "/api/v0/clusters", "999999999999", "arn:aws:iam::999999999999:user/test", http.StatusForbidden, 0},
		{"missing identity", "/api/v0/clusters", "", "", http.StatusForbidden, 0},
		{"mismatch", "/api/v0/clusters", "123456789012", "arn:aws:iam::999999999999:user/test", http.StatusForbidden, 0},
		{"malformed ARN", "/api/v0/clusters", "123456789012", "bad-arn", http.StatusForbidden, 0},
		{"live", "/api/v0/live", "", "", http.StatusOK, 0},
		{"ready", "/api/v0/ready", "", "", http.StatusOK, 0},
		{"info with malformed identity", "/api/v0/info", "123", "bad-arn", http.StatusOK, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := authzMetricSamples(t)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set(middleware.HeaderAccountID, tc.account)
			req.Header.Set(middleware.HeaderCallerARN, tc.caller)
			w := httptest.NewRecorder()
			srv.apiServer.Handler.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d, body=%s", w.Code, w.Body.String())
			}
			after := authzMetricSamples(t)
			if tc.wantAttempts == 0 && !reflect.DeepEqual(before, after) {
				t.Fatal("public or admission rejection counted as authorization attempt")
			}
			var attempts float64
			for key, value := range after {
				if strings.HasPrefix(key, "authz_requests_total/") {
					attempts += value - before[key]
				}
			}
			if attempts != float64(tc.wantAttempts) {
				t.Fatalf("authorization attempts=%v, want %d", attempts, tc.wantAttempts)
			}
			if w.Code != http.StatusForbidden {
				return
			}
			var status metav1.Status
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Kind != "Status" || status.Status != metav1.StatusFailure || status.Code != http.StatusForbidden || status.Reason != metav1.StatusReasonForbidden || strings.Contains(status.Message, tc.caller) && tc.caller != "" || status.Details != nil {
				t.Fatalf("unsafe or unstructured rejection: %+v", status)
			}
			if tc.name == "enrolled without grant" && strings.Contains(status.Message, "Account is not registered") {
				t.Fatal("missing grants confused with missing enrollment")
			}
		})
	}
}

func TestServerRateLimitBeforeAdmission(t *testing.T) {
	cfg := config.NewConfig()
	cfg.RateLimit = config.RateLimitConfig{Enabled: true, InMemory: true, DefaultRate: 1, DefaultBurst: 1, DefaultWindow: 60}
	authorizer := configuredAuthorizer(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, nil, authorizer, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	before := authzMetricSamples(t)
	for _, want := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters", nil)
		req.Header.Set(middleware.HeaderAccountID, "999999999999")
		req.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::999999999999:user/test")
		srv.apiServer.Handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("want %d, got %d: %s", want, w.Code, w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, authzMetricSamples(t)) {
		t.Fatal("admission rejection or rate-limited request counted as authorization attempt")
	}
}

// Allowlisted accounts get full access to their own resources until revoked.
func TestServerAccountAllowlist(t *testing.T) {
	cfg := config.NewConfig()
	cfg.Regional.AccountID = "123456789012"
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).Build(), logger)
	srv, err := newConfiguredServer(t, cfg, db, logger)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, account, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(middleware.HeaderAccountID, account)
		req.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::"+account+":user/test")
		w := httptest.NewRecorder()
		srv.apiServer.Handler.ServeHTTP(w, req)
		return w.Code
	}
	const customer = "333333333333"
	if code := call(http.MethodGet, "/api/v0/clusters", customer, ""); code != http.StatusForbidden {
		t.Fatalf("unenrolled account: %d", code)
	}
	if code := call(http.MethodPost, "/api/v0/accounts", "123456789012", `{"accountId":"`+customer+`"}`); code != http.StatusNoContent {
		t.Fatalf("enroll: %d", code)
	}
	if code := call(http.MethodGet, "/api/v0/clusters", customer, ""); code != http.StatusOK {
		t.Fatalf("allowlisted account not granted: %d", code)
	}
	if code := call(http.MethodDelete, "/api/v0/accounts/"+customer, "123456789012", ""); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code := call(http.MethodGet, "/api/v0/clusters", customer, ""); code != http.StatusForbidden {
		t.Fatalf("revoked account still granted: %d", code)
	}
}
