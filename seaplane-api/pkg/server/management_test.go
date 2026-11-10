package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newManagementServer builds a real Management service and HTTP router against the DynamoDB transport fixture.
func newManagementServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.NewConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Regional.AWSRegion = "us-east-1"
	client := authztest.Dynamo(t)
	if _, err := authz.PublishManagedPolicy(t.Context(), client, "policies", "read", `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`); err != nil {
		t.Fatal(err)
	}
	management, err := authz.NewService(client, authztest.Tables, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	authztest.SeedAdmin(t, client, authztest.Tables, "999999999999")
	srv, err := New(cfg, enrolledDB(t, logger, "999999999999"), management, logger)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// enrolledDB returns a fake database in which the given accounts are enrolled.
func enrolledDB(t *testing.T, logger *slog.Logger, accounts ...string) *seaplanedb.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, 0, len(accounts))
	for _, account := range accounts {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "accounts", Name: account}})
	}
	return seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), logger)
}

// managementHTTPRequest sends a gateway-authenticated request through the complete API middleware stack.
func managementHTTPRequest(srv *Server, callerARN, method, path, body, revision string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set(middleware.HeaderAccountID, "999999999999")
	r.Header.Set(middleware.HeaderCallerARN, callerARN)
	if revision != "" {
		r.Header.Set("If-Match", `"`+revision+`"`)
	}
	w := httptest.NewRecorder()
	srv.apiServer.Handler.ServeHTTP(w, r)
	return w
}

// TestManagementRoutes exercises real Management through HTTP and the AWS SDK without a legacy account record.
func TestManagementRoutes(t *testing.T) {
	srv := newManagementServer(t)
	request := func(method, path, body, revision string) *httptest.ResponseRecorder {
		return managementHTTPRequest(srv, "arn:aws:sts::999999999999:assumed-role/admin/session", method, path, body, revision)
	}
	policyBody := `{"id":"read","content":"permit(principal, action in Seaplane::Action::\"ReadOnly\", resource);"}`
	created := request("POST", "/api/v0/authz/policies", policyBody, "")
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	var policy struct{ ID, Revision, OwnerAccountID, Type string }
	if err := json.Unmarshal(created.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.ID != "read" || policy.Revision == "" || policy.Type != "custom" || policy.OwnerAccountID != "999999999999" {
		t.Fatalf("bad policy response %s", created.Body)
	}
	listed := request("GET", "/api/v0/authz/policies", "", "")
	var list struct {
		Kind  string
		Total int
		Items []struct{ ID, Type string }
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Kind != "PolicyList" || list.Total != 3 || len(list.Items) != 3 {
		t.Fatalf("bad list: %s", listed.Body)
	}
	kinds := map[string]bool{}
	for _, item := range list.Items {
		kinds[item.Type] = true
	}
	if !kinds["custom"] || !kinds["managed"] {
		t.Fatalf("missing policy type: %s", listed.Body)
	}
	for _, tc := range []struct {
		method, path, body, revision string
		code                         int
	}{
		{"GET", "/api/v0/authz/policies", "", "", 200},
		{"GET", "/api/v0/authz/policies/read", "", "", 200},
		{"GET", "/api/v0/authz/policies/read?type=managed", "", "", 200},
		{"PUT", "/api/v0/authz/policies/read?type=managed", `{"content":"x"}`, policy.Revision, 403},
		{"DELETE", "/api/v0/authz/policies/read?type=managed", "", policy.Revision, 403},
		{"DELETE", "/api/v0/authz/policies/read", "", "550e8400-e29b-41d4-a716-446655440000", 409},
		{"GET", "/api/v0/clusters", "", "", 403},
		{"POST", "/api/v0/authz/check", "{}", "", 404},
	} {
		w := request(tc.method, tc.path, tc.body, tc.revision)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	updated := request("PUT", "/api/v0/authz/policies/read", `{"content":"permit(principal, action in Seaplane::Action::\"ReadOnly\", resource);"}`, policy.Revision)
	if updated.Code != 200 {
		t.Fatalf("update: %d %s", updated.Code, updated.Body)
	}
	oldRevision := policy.Revision
	if err := json.Unmarshal(updated.Body.Bytes(), &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Revision == oldRevision {
		t.Fatal("update did not return a new revision")
	}
	invalid := request("POST", "/api/v0/authz/policies", `{"id":"bad","content":"not cedar"}`, "")
	if invalid.Code != 422 {
		t.Fatalf("invalid policy: %d %s", invalid.Code, invalid.Body)
	}
	deleted := request("DELETE", "/api/v0/authz/policies/read", "", policy.Revision)
	var receipt authz.DeletionReceipt
	if deleted.Code != 200 || json.Unmarshal(deleted.Body.Bytes(), &receipt) != nil || receipt.DeletedRevision != policy.Revision || receipt.CommittedRegion != "us-east-1" {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
}

// TestRegionalDelegationHTTP checks scoped delegation and immediate revocation through stored grants.
func TestRegionalDelegationHTTP(t *testing.T) {
	srv := newManagementServer(t)
	callerARN := "arn:aws:sts::999999999999:assumed-role/admin/session"
	request := func(method, path, body, revision string) *httptest.ResponseRecorder {
		return managementHTTPRequest(srv, callerARN, method, path, body, revision)
	}
	read := request("POST", "/api/v0/authz/policies", `{"id":"read","content":"permit(principal, action in Seaplane::Action::\"ReadOnly\", resource);"}`, "")
	if read.Code != 201 {
		t.Fatalf("read policy: %d %s", read.Code, read.Body)
	}
	delegate := "arn:aws:iam::999999999999:user/delegate"
	callerARN = delegate
	denied := request("GET", "/api/v0/authz/attachments?scope=regional", "", "")
	if denied.Code != 403 {
		t.Fatalf("ungranted delegate: %d %s", denied.Code, denied.Body)
	}
	callerARN = "arn:aws:sts::999999999999:assumed-role/admin/session"
	grantPolicy := request("POST", "/api/v0/authz/policies", `{"id":"regional-admin","content":"permit(principal, action in Seaplane::Action::\"RegionalAttachmentAdmin\", resource);"}`, "")
	if grantPolicy.Code != 201 {
		t.Fatalf("delegation policy: %d %s", grantPolicy.Code, grantPolicy.Body)
	}
	grant := request("POST", "/api/v0/authz/attachments", `{"policy":{"id":"regional-admin","type":"custom"},"principalArn":"`+delegate+`","scope":"regional"}`, "")
	if grant.Code != 201 {
		t.Fatalf("attachment: %d %s", grant.Code, grant.Body)
	}
	var attachment struct{ ID, Revision, Region, Scope string }
	if err := json.Unmarshal(grant.Body.Bytes(), &attachment); err != nil {
		t.Fatal(err)
	}
	if attachment.Region != "us-east-1" || attachment.Scope != "regional" {
		t.Fatalf("bad attachment response: %s", grant.Body)
	}
	callerARN = delegate
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/v0/authz/attachments?scope=regional", 200},
		{"/api/v0/authz/attachments?scope=global", 403},
		{"/api/v0/authz/attachments", 403},
		{"/api/v0/authz/policies", 403},
	} {
		w := request("GET", tc.path, "", "")
		if w.Code != tc.code {
			t.Errorf("delegated %s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	local := request("POST", "/api/v0/authz/attachments", `{"policy":{"id":"read","type":"custom"},"principalArn":"`+delegate+`","scope":"regional"}`, "")
	if local.Code != 201 {
		t.Fatalf("delegated create: %d %s", local.Code, local.Body)
	}
	callerARN = "arn:aws:sts::999999999999:assumed-role/admin/session"
	revoked := request("DELETE", "/api/v0/authz/attachments/"+attachment.ID+"?scope=regional", "", attachment.Revision)
	var revokeReceipt authz.DeletionReceipt
	if revoked.Code != 200 || json.Unmarshal(revoked.Body.Bytes(), &revokeReceipt) != nil || revokeReceipt.DeletedRevision != attachment.Revision {
		t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body)
	}
	callerARN = delegate
	denied = request("GET", "/api/v0/authz/attachments?scope=regional", "", "")
	if denied.Code != 403 {
		t.Fatalf("revoked delegate: %d %s", denied.Code, denied.Body)
	}
	callerARN = "arn:aws:sts::999999999999:assumed-role/admin/session"
	foreign := request("POST", "/api/v0/authz/attachments", `{"policy":{"id":"read","type":"custom"},"principalArn":"arn:aws:iam::123456789012:user/foreign"}`, "")
	if foreign.Code != 422 {
		t.Fatalf("foreign principal: %d %s", foreign.Code, foreign.Body)
	}
}
