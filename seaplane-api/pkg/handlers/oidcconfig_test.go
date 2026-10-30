package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

const (
	testOidcIssuerBaseURL = "https://oidc.example.com"
	testRegion            = "us-east-1"
)

// testOidcConfigCR creates an OidcConfig CR with Namespace=account-<accountID>, Name=configID,
// mirroring the namespace/name scheme used by seaplanedb.Client for OidcConfig operations.
func testOidcConfigCR(configID, accountID string, spec seaplanev1alpha1.OidcConfigSpec) *seaplanev1alpha1.OidcConfig {
	return &seaplanev1alpha1.OidcConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configID,
			Namespace: "account-" + accountID,
			UID:       types.UID(configID),
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Spec: spec,
	}
}

func oidcConfigCreateBody(name string, spec map[string]any) []byte {
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     spec,
	})
	return body
}

// testIssuerIndex creates the Index reserving issuerURL for oidcConfigID, as the reconciler would.
func testIssuerIndex(issuerURL, oidcConfigID, accountID string) *seaplanev1alpha1.Index {
	return &seaplanev1alpha1.Index{
		ObjectMeta: metav1.ObjectMeta{
			Name:      seaplanev1alpha1.IssuerURLIndexName(issuerURL),
			Namespace: seaplanev1alpha1.OidcIssuerReservationsNamespace,
			Labels: map[string]string{
				seaplanev1alpha1.AccountIDLabel: accountID,
				seaplanev1alpha1.OwnerUIDLabel:  oidcConfigID,
			},
		},
	}
}

func testManagedOidcConfigSpec(accountID string) seaplanev1alpha1.OidcConfigSpec {
	return seaplanev1alpha1.OidcConfigSpec{
		Type:      seaplanev1alpha1.OidcConfigTypeManaged,
		AccountID: accountID,
	}
}

func testUnmanagedOidcConfigSpec(accountID string) seaplanev1alpha1.OidcConfigSpec {
	return seaplanev1alpha1.OidcConfigSpec{
		Type:             seaplanev1alpha1.OidcConfigTypeUnmanaged,
		AccountID:        accountID,
		SecretArn:        "arn:aws:secretsmanager:us-east-1:123456789012:secret:key",
		InstallerRoleArn: "arn:aws:iam::123456789012:role/installer",
	}
}

func TestOidcConfigHandler_List_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-1", testAccountID, testManagedOidcConfigSpec(testAccountID)),
		testOidcConfigCR("oidc-2", testAccountID, testManagedOidcConfigSpec(testAccountID)),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs", nil)
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if int(result["total"].(float64)) != 2 {
		t.Errorf("expected total=2, got %v", result["total"])
	}
	items := result["items"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

func TestOidcConfigHandler_List_Empty(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs", nil)
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if int(result["total"].(float64)) != 0 {
		t.Errorf("expected total=0, got %v", result["total"])
	}
	items := result["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected 0 items, got %d", len(items))
	}
}

func TestOidcConfigHandler_List_Pagination(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-1", testAccountID, testManagedOidcConfigSpec(testAccountID)),
		testOidcConfigCR("oidc-2", testAccountID, testManagedOidcConfigSpec(testAccountID)),
		testOidcConfigCR("oidc-3", testAccountID, testManagedOidcConfigSpec(testAccountID)),
		testOidcConfigCR("foreign", "999999999999", testManagedOidcConfigSpec("999999999999")),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs?limit=2&offset=1", nil)
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if int(result["total"].(float64)) != 3 {
		t.Errorf("expected total=3, got %v", result["total"])
	}
	if int(result["limit"].(float64)) != 2 {
		t.Errorf("expected limit=2, got %v", result["limit"])
	}
	if int(result["offset"].(float64)) != 1 {
		t.Errorf("expected offset=1, got %v", result["offset"])
	}
	items := result["items"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items (offset=1, limit=2 of 3), got %d", len(items))
	}
}

func TestOidcConfigHandler_List_OffsetBeyondTotal(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-1", testAccountID, testManagedOidcConfigSpec(testAccountID)),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs?offset=10", nil)
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if int(result["total"].(float64)) != 1 {
		t.Errorf("expected total=1, got %v", result["total"])
	}
	items := result["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected 0 items when offset beyond total, got %d", len(items))
	}
}

func TestOidcConfigHandler_Create_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	handler.generateID = func() string { return "generated-config-id" }

	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name":      "test-config",
			"namespace": "account-" + testAccountID,
			"labels":    map[string]any{seaplanev1alpha1.AccountIDLabel: "999999999999"},
		},
		"spec": map[string]any{
			"type":      "managed",
			"accountId": "999999999999",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if name := metaField(result, "name"); name != "test-config" {
		t.Errorf("expected metadata.name=test-config, got %v", name)
	}
	spec := result["spec"].(map[string]any)
	if spec["type"] != "managed" {
		t.Errorf("expected spec.type=managed, got %v", spec["type"])
	}
	wantIssuerURL := testOidcIssuerBaseURL + "/generated-config-id"
	if spec["issuerUrl"] != wantIssuerURL {
		t.Errorf("expected spec.issuerUrl=%s, got %v", wantIssuerURL, spec["issuerUrl"])
	}

	stored, err := handler.db.GetOidcConfig(req.Context(), "test-config")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.AccountID != testAccountID || stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("stored ownership = (%q, %q), want caller account %q", stored.Spec.AccountID, stored.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
	}
}

func TestOIDCCreateRejectsNamespace(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	body := `{"metadata":{"name":"test-config","namespace":"account-999999999999"},"spec":{"type":"managed"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", strings.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if message := decodeErrorMessage(t, w); !strings.Contains(message, ErrOidcConfigCreateNamespaceMismatch.Code) {
		t.Errorf("error message = %q, want code %s", message, ErrOidcConfigCreateNamespaceMismatch.Code)
	}
	if _, err := handler.db.GetOidcConfig(req.Context(), "test-config"); !seaplanedb.IsNotFound(err) {
		t.Errorf("rejected OIDC config was stored: %v", err)
	}
}

func TestOidcConfigHandler_Create_ManagedRejectsWhenIssuerBaseURLNotConfigured(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	// A blank base URL must never be silently turned into a path-only
	// issuerUrl (e.g. "/generated-config-id"); the server should refuse to
	// create the config instead.
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), "", clusterAuthorizer(t, lifecyclePermit), logger)
	handler.generateID = func() string { return "generated-config-id" }

	body := oidcConfigCreateBody("test-config", map[string]any{"type": "managed"})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateIssuerNotConfigured.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateIssuerNotConfigured.Code, errResp["message"])
	}

	if _, err := handler.db.GetOidcConfig(req.Context(), "test-config"); err == nil {
		t.Error("expected no OidcConfig CR to be created when the issuer base URL is not configured")
	}
}

func TestOidcConfigHandler_Create_ManagedIgnoresClientIssuerUrl(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	handler.generateID = func() string { return "generated-config-id" }

	body := oidcConfigCreateBody("test-config", map[string]any{
		"type":      "managed",
		"issuerUrl": "https://arbitrary.example.com/bypass",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	wantIssuerURL := testOidcIssuerBaseURL + "/generated-config-id"
	spec := result["spec"].(map[string]any)
	if issuerURL, _ := spec["issuerUrl"].(string); issuerURL != wantIssuerURL {
		t.Errorf("expected client-supplied issuerUrl to be overridden with %q, got %q", wantIssuerURL, issuerURL)
	}

	cr, err := handler.db.GetOidcConfig(req.Context(), "test-config")
	if err != nil {
		t.Fatalf("failed to fetch created CR: %v", err)
	}
	if cr.Spec.IssuerUrl != wantIssuerURL {
		t.Errorf("expected stored CR issuerUrl=%q, got %q", wantIssuerURL, cr.Spec.IssuerUrl)
	}
}

func TestOidcConfigHandler_Create_InvalidJSON(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader([]byte("not json")))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateInvalidBody.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateInvalidBody.Code, errResp["message"])
	}
}

func TestOidcConfigHandler_Create_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"missing spec", []byte(`{}`)},
		{"missing type", []byte(`{"spec":{}}`)},
		{"empty type", []byte(`{"spec":{"type":""}}`)},
		{"whitespace spec", []byte(`{"spec":{ }}`)},
		{"whitespace with newline spec", []byte(`{"spec":{
}}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

			req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(tt.body))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}

			var errResp map[string]any
			_ = json.NewDecoder(w.Body).Decode(&errResp)
			if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateMissingFields.Code) {
				t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateMissingFields.Code, errResp["message"])
			}
		})
	}
}

func TestOidcConfigHandler_Create_InvalidType(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	body := oidcConfigCreateBody("test-config", map[string]any{"type": "bogus"})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateInvalidType.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateInvalidType.Code, errResp["message"])
	}
}

func TestOidcConfigHandler_Create_InvalidFieldsForType(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]any
	}{
		{
			name: "unmanaged missing secretArn",
			spec: map[string]any{
				"type":             "unmanaged",
				"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
				"issuerUrl":        "https://example.com/oidc",
			},
		},
		{
			name: "unmanaged missing installerRoleArn",
			spec: map[string]any{
				"type":      "unmanaged",
				"secretArn": "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
				"issuerUrl": "https://example.com/oidc",
			},
		},
		{
			name: "unmanaged missing issuerUrl",
			spec: map[string]any{
				"type":             "unmanaged",
				"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
				"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
			},
		},
		{
			name: "managed with secretArn set",
			spec: map[string]any{
				"type":      "managed",
				"secretArn": "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
			},
		},
		{
			name: "managed with installerRoleArn set",
			spec: map[string]any{
				"type":             "managed",
				"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

			body := oidcConfigCreateBody("test-config", tt.spec)
			req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
			}

			var errResp map[string]any
			_ = json.NewDecoder(w.Body).Decode(&errResp)
			if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateInvalidFields.Code) {
				t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateInvalidFields.Code, errResp["message"])
			}
		})
	}
}

func TestOidcConfigHandler_Create_UnmanagedSuccess(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	handler.generateID = func() string { return "generated-config-id" }

	body := oidcConfigCreateBody("unmanaged-config", map[string]any{
		"type":             "unmanaged",
		"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
		"issuerUrl":        "https://example.com/oidc",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	spec := result["spec"].(map[string]any)
	if spec["type"] != "unmanaged" {
		t.Errorf("expected spec.type=unmanaged, got %v", spec["type"])
	}
}

// TestOidcConfigHandler_Create_UnmanagedNormalizesIssuerUrl checks that issuerUrl is canonicalized
// before it's stored or used as the Index name.
func TestOidcConfigHandler_Create_UnmanagedNormalizesIssuerUrl(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	handler.generateID = func() string { return "generated-config-id" }

	body := oidcConfigCreateBody("normalized-config", map[string]any{
		"type":             "unmanaged",
		"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
		"issuerUrl":        "https://EXAMPLE.com:443/oidc/?foo=bar#frag",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	const want = "https://example.com/oidc"

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	spec := result["spec"].(map[string]any)
	if spec["issuerUrl"] != want {
		t.Errorf("expected normalized spec.issuerUrl=%s, got %v", want, spec["issuerUrl"])
	}

	cr, err := handler.db.GetOidcConfig(req.Context(), "normalized-config")
	if err != nil {
		t.Fatalf("failed to fetch created CR: %v", err)
	}
	if cr.Spec.IssuerUrl != want {
		t.Errorf("stored issuerUrl = %q, want %q", cr.Spec.IssuerUrl, want)
	}
}

// TestOidcConfigHandler_Create_UnmanagedInvalidIssuerUrl checks that a malformed issuerUrl gets a 400.
func TestOidcConfigHandler_Create_UnmanagedInvalidIssuerUrl(t *testing.T) {
	tests := []struct {
		name      string
		issuerUrl string
	}{
		{"not https", "http://example.com/oidc"},
		{"has userinfo", "https://user:pass@example.com/oidc"},
		{"contains whitespace", "https://example.com/oi dc"},
		{"relative", "/just/a/path"},
		{"empty host", "https:///oidc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

			body := oidcConfigCreateBody("invalid-config", map[string]any{
				"type":             "unmanaged",
				"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
				"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
				"issuerUrl":        tt.issuerUrl,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			var errResp map[string]any
			_ = json.NewDecoder(w.Body).Decode(&errResp)
			if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateInvalidIssuerUrl.Code) {
				t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateInvalidIssuerUrl.Code, errResp["message"])
			}
		})
	}
}

// TestOidcConfigHandler_Create_UnmanagedDuplicateIssuerUrlSameAccount seeds an Index for the URL,
// so Create's fast-path check catches the duplicate synchronously.
func TestOidcConfigHandler_Create_UnmanagedDuplicateIssuerUrlSameAccount(t *testing.T) {
	scheme := newTestScheme()
	existingSpec := seaplanev1alpha1.OidcConfigSpec{
		Type:             seaplanev1alpha1.OidcConfigTypeUnmanaged,
		IssuerUrl:        "https://example.com/oidc",
		SecretArn:        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		InstallerRoleArn: "arn:aws:iam::123456789012:role/installer",
		AccountID:        testAccountID,
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-existing", testAccountID, existingSpec),
		testIssuerIndex("https://example.com/oidc", "oidc-existing", testAccountID),
	).Build()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	body := oidcConfigCreateBody("duplicate-config", map[string]any{
		"type":             "unmanaged",
		"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:bar",
		"installerRoleArn": "arn:aws:iam::123456789012:role/installer2",
		"issuerUrl":        "https://example.com/oidc",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigCreateDuplicateIssuerUrl.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigCreateDuplicateIssuerUrl.Code, errResp["message"])
	}
}

// TestOidcConfigHandler_Create_UnmanagedDuplicateIssuerUrlDifferentAccountRejected checks that the
// Index's shared namespace rejects a duplicate URL even across accounts.
func TestOidcConfigHandler_Create_UnmanagedDuplicateIssuerUrlDifferentAccountRejected(t *testing.T) {
	otherAccount := "999999999999"
	scheme := newTestScheme()
	existingSpec := seaplanev1alpha1.OidcConfigSpec{
		Type:             seaplanev1alpha1.OidcConfigTypeUnmanaged,
		IssuerUrl:        "https://example.com/oidc",
		SecretArn:        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		InstallerRoleArn: "arn:aws:iam::123456789012:role/installer",
		AccountID:        otherAccount,
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-existing", otherAccount, existingSpec),
		testIssuerIndex("https://example.com/oidc", "oidc-existing", otherAccount),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	body := oidcConfigCreateBody("duplicate-config", map[string]any{
		"type":             "unmanaged",
		"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:bar",
		"installerRoleArn": "arn:aws:iam::123456789012:role/installer2",
		"issuerUrl":        "https://example.com/oidc",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate issuerUrl reserved by a different account, got %d: %s", w.Code, w.Body.String())
	}
}

// TestOidcConfigHandler_Create_UnmanagedSameIssuerUrlBothSucceedWithoutIndexYet documents the
// accepted trade-off: Create's fast-path check is best-effort, so two never-before-seen duplicates can both return 201.
func TestOidcConfigHandler_Create_UnmanagedSameIssuerUrlBothSucceedWithoutIndexYet(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	n := 0
	handler.generateID = func() string {
		n++
		return fmt.Sprintf("generated-config-id-%d", n)
	}

	spec := map[string]any{
		"type":             "unmanaged",
		"secretArn":        "arn:aws:secretsmanager:us-east-1:123456789012:secret:foo",
		"installerRoleArn": "arn:aws:iam::123456789012:role/installer",
		"issuerUrl":        "https://example.com/oidc-racy",
	}

	for i := range 2 {
		body := oidcConfigCreateBody(fmt.Sprintf("config-%d", i), spec)
		req := httptest.NewRequest(http.MethodPost, "/api/v0/oidc_configs", bytes.NewReader(body))
		req = req.WithContext(testContext(testAccountID))
		w := httptest.NewRecorder()
		handler.Create(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d: expected 201 (no Index reserved yet, so the fast-path check can't catch this), got %d: %s", i, w.Code, w.Body.String())
		}
	}
}

func TestOidcConfigHandler_Get_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-123", testAccountID, testManagedOidcConfigSpec(testAccountID)),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs/oidc-123", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-123"})

	w := httptest.NewRecorder()
	handler.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if uid := metaField(result, "uid"); uid != "oidc-123" {
		t.Errorf("expected metadata.uid=oidc-123, got %v", uid)
	}
}

func TestOidcConfigHandler_Get_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs/no-such-config", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "no-such-config"})

	w := httptest.NewRecorder()
	handler.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigGetNotFound.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigGetNotFound.Code, errResp["message"])
	}
}

func TestOidcConfigHandler_Get_WrongAccount(t *testing.T) {
	otherAccount := "999999999999"
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-123", otherAccount, testManagedOidcConfigSpec(otherAccount)),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/oidc_configs/oidc-123", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-123"})

	w := httptest.NewRecorder()
	handler.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for config owned by a different account, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOidcConfigHandler_Delete_WrongAccount(t *testing.T) {
	const foreignAccount = "999999999999"
	foreign := testOidcConfigCR("oidc-foreign", foreignAccount, testManagedOidcConfigSpec(foreignAccount))
	fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(foreign).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)
	req := httptest.NewRequest(http.MethodDelete, "/api/v0/oidc_configs/oidc-foreign", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-foreign"})
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := handler.db.GetOidcConfig(testContext(foreignAccount), "oidc-foreign"); err != nil {
		t.Fatalf("foreign OIDC config was deleted: %v", err)
	}
}

func TestOidcConfigHandler_Delete_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testOidcConfigCR("oidc-123", testAccountID, testManagedOidcConfigSpec(testAccountID)),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/oidc_configs/oidc-123", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-123"})

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result["config_id"] != "oidc-123" {
		t.Errorf("expected config_id=oidc-123, got %v", result["config_id"])
	}
}

func TestOidcConfigHandler_Delete_InUse(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testOidcConfigCR("oidc-123", testAccountID, testManagedOidcConfigSpec(testAccountID))
	referencingCluster := testClusterCR("cluster-id", "referencing-cluster", testAccountID)
	oidcConfig.Labels = map[string]string{claimedByClusterUIDLabel: string(referencingCluster.UID)}
	referencingCluster.Spec.OidcConfigID = "oidc-123"
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oidcConfig, referencingCluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/oidc_configs/oidc-123", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-123"})

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigDeleteInUse.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigDeleteInUse.Code, errResp["message"])
	}

	if _, err := handler.db.GetOidcConfig(req.Context(), "oidc-123"); err != nil {
		t.Errorf("expected OidcConfig to survive a blocked delete, got error: %v", err)
	}
}

func TestOidcConfigHandler_Delete_AfterClusterDeletedSucceeds(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testOidcConfigCR("oidc-123", testAccountID, testManagedOidcConfigSpec(testAccountID))
	referencingCluster := testClusterCR("cluster-id", "referencing-cluster", testAccountID)
	oidcConfig.Labels = map[string]string{claimedByClusterUIDLabel: string(referencingCluster.UID)}
	referencingCluster.Spec.OidcConfigID = "oidc-123"
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oidcConfig, referencingCluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/oidc_configs/oidc-123", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "oidc-123"})

	// While the cluster still references the config, deletion must be blocked.
	w := httptest.NewRecorder()
	handler.Delete(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 while the cluster still references the config, got %d: %s", w.Code, w.Body.String())
	}

	// Deleting the Cluster alone no longer releases the claim — that's the operator's job — so simulate that release directly on the OidcConfig.
	if err := fc.Delete(context.Background(), referencingCluster); err != nil {
		t.Fatalf("failed to delete referencing cluster: %v", err)
	}
	latest, err := handler.db.GetOidcConfig(testContext(testAccountID), "oidc-123")
	if err != nil {
		t.Fatalf("failed to fetch oidc config: %v", err)
	}
	delete(latest.Labels, claimedByClusterUIDLabel)
	if err := handler.db.UpdateOidcConfigObject(testContext(testAccountID), latest); err != nil {
		t.Fatalf("failed to release claim label: %v", err)
	}

	// Once the claim is released, deletion must succeed.
	w = httptest.NewRecorder()
	handler.Delete(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 once no cluster references the config, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOidcConfigHandler_Delete_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewOidcConfigHandler(seaplanedb.NewClientFrom(fc, logger), testOidcIssuerBaseURL, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/oidc_configs/no-such-config", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "no-such-config"})

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var errResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if !strings.Contains(errResp["message"].(string), ErrOidcConfigDeleteNotFound.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrOidcConfigDeleteNotFound.Code, errResp["message"])
	}
}
