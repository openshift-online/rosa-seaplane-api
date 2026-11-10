package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

// TestManagementAdmission proves even a seeded admin needs enrollment, and enrollment alone grants no management.
func TestManagementAdmission(t *testing.T) {
	cfg := config.NewConfig()
	cfg.Regional.AccountID = "123456789012"
	cfg.Regional.AWSRegion = "us-east-1"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, enrolledDB(t, logger), authztest.Management(t, "333333333333", "us-east-1"), logger)
	if err != nil {
		t.Fatal(err)
	}
	admin := func(account string) string { return "arn:aws:sts::" + account + ":assumed-role/admin/session" }
	user := func(account string) string { return "arn:aws:iam::" + account + ":user/test" }
	for _, tc := range []struct {
		method, path, account, caller, body string
		code                                int
	}{
		{"GET", "/api/v0/authz/policies", "333333333333", admin("333333333333"), "", 403},
		{"POST", "/api/v0/accounts", "123456789012", user("123456789012"), `{"accountId":"333333333333"}`, 204},
		{"GET", "/api/v0/clusters", "333333333333", user("333333333333"), "", 403},
		{"GET", "/api/v0/authz/policies", "333333333333", user("333333333333"), "", 403},
		{"GET", "/api/v0/authz/policies", "333333333333", admin("333333333333"), "", 200},
		{"GET", "/api/v0/authz/policies", "123456789012", admin("123456789012"), "", 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set(middleware.HeaderAccountID, tc.account)
		r.Header.Set(middleware.HeaderCallerARN, tc.caller)
		w := httptest.NewRecorder()
		srv.apiServer.Handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s %s %s: %d %s", tc.method, tc.path, tc.account, tc.caller, w.Code, w.Body)
		}
	}
}

// TestManagementBackendHTTP verifies real service backend outages through HTTP.
func TestManagementBackendHTTP(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"__type":"InternalServerError"}`)
	}))
	defer endpoint.Close()
	client := dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL); o.RetryMaxAttempts = 1 })
	cfg := config.NewConfig()
	cfg.Regional.AWSRegion = "us-east-1"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	management, err := authz.NewService(client, authztest.Tables, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, enrolledDB(t, logger, "999999999999"), management, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/v0/authz/policies", `{"id":"read","content":"permit(principal, action, resource);"}`, 503},
		{"GET", "/api/v0/authz/policies", "", 503},
		{"POST", "/api/v0/authz/attachments", `{"policy":{"id":"read","type":"custom"},"principalArn":"arn:aws:iam::999999999999:user/test","scope":"regional"}`, 503},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set(middleware.HeaderAccountID, "999999999999")
		r.Header.Set(middleware.HeaderCallerARN, "arn:aws:sts::999999999999:assumed-role/admin/session")
		w := httptest.NewRecorder()
		srv.apiServer.Handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}
