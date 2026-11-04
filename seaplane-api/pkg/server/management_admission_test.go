package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestManagementAdmission proves legacy enrollment grants no management privileges, even for a configured management account.
func TestManagementAdmission(t *testing.T) {
	cfg := config.NewConfig()
	cfg.Regional.AccountID = "123456789012"
	authorizer := configuredAuthorizer(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	db := seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).Build(), logger)
	management, err := authz.NewManagement(managementDynamo(t), authz.ManagementConfig{FormatVersion: 1, PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional", Accounts: map[string]authz.ManagementAccount{"333333333333": {WriterRegion: "us-east-1", AdministratorARNs: []string{"arn:aws:iam::333333333333:role/admin"}}}}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, db, authorizer, management, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, account, body string
		code                        int
	}{
		{"POST", "/api/v0/accounts", "123456789012", `{"accountId":"333333333333"}`, 204},
		{"GET", "/api/v0/clusters", "333333333333", "", 200},
		{"GET", "/api/v0/authz/policies", "333333333333", "", 403},
		{"GET", "/api/v0/authz/policies", "123456789012", "", 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set(middleware.HeaderAccountID, tc.account)
		r.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::"+tc.account+":user/test")
		w := httptest.NewRecorder()
		srv.apiServer.Handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s %s: %d %s", tc.method, tc.path, tc.account, w.Code, w.Body)
		}
	}
	absent, err := New(cfg, db, authorizer, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	absent.apiServer.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/v0/authz/policies", nil))
	if w.Code != 404 {
		t.Fatalf("disabled management: %d %s", w.Code, w.Body)
	}
}

// TestManagementWriterHTTP verifies real service writer guidance and backend outages through HTTP.
func TestManagementWriterHTTP(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"__type":"InternalServerError"}`)
	}))
	defer endpoint.Close()
	client := dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL); o.RetryMaxAttempts = 1 })
	cfg := config.NewConfig()
	authorizer := configuredAuthorizer(t, cfg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	management, err := authz.NewManagement(client, authz.ManagementConfig{FormatVersion: 1, PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional", Accounts: map[string]authz.ManagementAccount{"999999999999": {WriterRegion: "us-west-2", AdministratorARNs: []string{"arn:aws:iam::999999999999:role/admin"}}}}, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, nil, authorizer, management, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/v0/authz/policies", `{"id":"read","content":"x"}`, 409},
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
		if tc.code == 409 {
			var status metav1.Status
			_ = json.Unmarshal(w.Body.Bytes(), &status)
			if w.Header().Get("X-Authz-Writer-Region") != "us-west-2" || status.Details == nil || status.Details.Causes[0].Field != "writerRegion" || status.Details.Causes[0].Message != "us-west-2" {
				t.Fatalf("missing guidance: %s", w.Body)
			}
		}
	}
}
