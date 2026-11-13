package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// TestManagementLocalEndpoint proves startup uses the existing SDK configuration and its DynamoDB endpoint override.
func TestManagementLocalEndpoint(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var input struct{ TableName string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		// An ungranted caller's management request only looks up its grants.
		if input.TableName != "global-attachments" || !strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".Query") {
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
	setStartupTables(t)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	management, err := loadAuthorization(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = management.ListPolicies(t.Context(), authz.ManagementCaller{AccountID: "123456789012", CallerARN: "arn:aws:sts::123456789012:assumed-role/admin/session", Request: authz.RequestContext{RequestTime: time.Now()}})
	if !errors.Is(err, authz.ErrManagementDenied) || calls.Load() == 0 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}
