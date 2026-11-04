package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// TestManagementLocalEndpoint proves startup uses the existing SDK configuration and its DynamoDB endpoint override.
func TestManagementLocalEndpoint(t *testing.T) {
	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input struct{ TableName string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.TableName != "policies" || !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".PutItem") {
			t.Errorf("unexpected request %s %+v", r.Header.Get("X-Amz-Target"), input)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer endpoint.Close()
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	t.Setenv("AWS_ENDPOINT_URL_DYNAMODB", endpoint.URL)
	t.Setenv("AUTHZ_MANAGEMENT_CONFIG_FILE", startupFile(t, startupManagement))
	cfg, err := awsconfig.LoadDefaultConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	management, err := loadManagement(startupCommand(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := management.CreatePolicy(t.Context(), authz.ManagementCaller{AccountID: "123456789012", CallerARN: "arn:aws:sts::123456789012:assumed-role/admin/session", Request: authz.RequestContext{RequestTime: time.Now()}}, "read", `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`)
	if err != nil || policy.Revision == "" || calls != 1 {
		t.Fatalf("policy=%+v calls=%d error=%v", policy, calls, err)
	}
}

// TestManagementStartupFail rejects invalid supplied management config before a PostgreSQL connection is attempted.
func TestManagementStartupFail(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AUTHZ_CONFIG_FILE", startupFile(t, startupBundle))
	t.Setenv("AUTHZ_MANAGEMENT_CONFIG_FILE", startupFile(t, "formatVersion: 99"))
	t.Setenv("POSTGRES_DSN", "invalid postgres connection")
	err := runServe(startupCommand(t), nil)
	if err == nil || !strings.Contains(err.Error(), "management config") {
		t.Fatalf("want management validation before DB startup, got %v", err)
	}
}
