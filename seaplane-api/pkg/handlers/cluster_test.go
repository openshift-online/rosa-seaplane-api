package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	configv1 "github.com/openshift/api/config/v1"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

const testAccountID = "123456789012"
const testDNSReservationUID = "11111111-1111-4111-8111-111111111111"

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = seaplanev1alpha1.AddToScheme(s)
	return s
}

func newClusterTestClientBuilder(scheme *runtime.Scheme) *fake.ClientBuilder {
	return newClusterTestClientBuilderWithDNSReservation(scheme, testDNSReservationCR(testAccountID))
}

func newClusterTestClientBuilderWithDNSReservation(scheme *runtime.Scheme, reservation *seaplanev1alpha1.DNSReservation) *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&seaplanev1alpha1.DNSReservation{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&seaplanev1alpha1.OidcConfig{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&seaplanev1alpha1.Cluster{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if cluster, ok := obj.(*seaplanev1alpha1.Cluster); ok && cluster.UID == "" {
					cluster.UID = types.UID(uuid.NewString())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		WithObjects(reservation)
}

func testDNSReservationCR(accountID string) *seaplanev1alpha1.DNSReservation {
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-reservation",
			Namespace: "account-" + accountID,
			UID:       types.UID(testDNSReservationUID),
			Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: accountID},
		},
		Status: seaplanev1alpha1.DNSReservationStatus{
			Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
			BaseDomain: "f7a3.0.example.com",
		},
	}
	reservation.Status.InternalHostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)
	return reservation
}

func testContext(accountID string) context.Context {
	ctx := context.Background()
	ctx = context.WithValue(ctx, middleware.ContextKeyAccountID, accountID)
	ctx = context.WithValue(ctx, middleware.ContextKeyCallerARN, "arn:aws:iam::"+accountID+":user/test")
	return ctx
}

func assertHandlerStatus(t *testing.T, want int, method, path, body string, vars map[string]string, accountID string, invoke func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(testContext(accountID))
	if vars != nil {
		req = mux.SetURLVars(req, vars)
	}
	w := httptest.NewRecorder()
	invoke(w, req)
	if w.Code != want {
		t.Errorf("expected %d, got %d: %s", want, w.Code, w.Body.String())
	}
}

// testClusterCR creates a cluster CR with an account namespace, a database UID,
// and the owning account label.
func testClusterCR(clusterID, clusterName, accountID string) *seaplanev1alpha1.Cluster {
	return &seaplanev1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: "account-" + accountID,
			UID:       types.UID(clusterID),
			Labels: map[string]string{
				seaplanev1alpha1.AccountIDLabel: accountID,
			},
		},
		Spec: seaplanev1alpha1.ClusterSpec{
			DNSReservationID: testDNSReservationUID,
			AccountID:        accountID,
			HostedCluster: seaplanev1alpha1.HostedClusterSpecPassthrough{
				Platform: hypershiftv1beta1.PlatformSpec{
					Type: hypershiftv1beta1.AWSPlatform,
					AWS: &hypershiftv1beta1.AWSPlatformSpec{
						Region: "us-east-1",
					},
				},
			},
		},
	}
}

// metaField extracts a field from the K8s-native metadata object in a decoded response.
func metaField(result map[string]any, field string) any {
	meta, _ := result["metadata"].(map[string]any)
	if meta == nil {
		return nil
	}
	return meta[field]
}

// testOidcConfigID is the OidcConfig ID used by tests that opt into the
// OidcConfig-backed path via specWithOidcConfigID.
const testOidcConfigID = "test-oidc-config"

// testOidcConfigIssuerURL is the issuerUrl on the fixture OidcConfig created
// by testReadyOidcConfig; tests assert the created cluster's issuerURL matches.
const testOidcConfigIssuerURL = "https://oidc.example.com/test-oidc-config"

// minSpec is the minimum valid cluster spec for create requests. It omits
// oidcConfigId, selecting the legacy (auto-generated issuer) path.
var minSpec = map[string]any{
	"dnsReservationId": testDNSReservationUID,
	"hostedCluster": map[string]any{
		"release":    map[string]any{"image": ""},
		"networking": map[string]any{},
	},
}

// specWithOidcConfigID returns a copy of minSpec with oidcConfigId set,
// selecting the OidcConfig-backed issuer path.
func specWithOidcConfigID(id string) map[string]any {
	spec := map[string]any{"oidcConfigId": id}
	for k, v := range minSpec {
		spec[k] = v
	}
	return spec
}

// testReadyOidcConfig returns a managed OidcConfig CR in the Ready phase
// with the given issuerUrl, scoped to accountID.
func testReadyOidcConfig(configID, accountID, issuerURL string) *seaplanev1alpha1.OidcConfig {
	oc := testOidcConfigCR(configID, accountID, testManagedOidcConfigSpec(accountID))
	oc.Spec.IssuerUrl = issuerURL
	oc.Status.Phase = seaplanev1alpha1.OidcConfigPhaseReady
	return oc
}

// clusterBody builds a K8s-native cluster create request body.
// Pass nil spec to use minSpec.
func clusterBody(name string, spec map[string]any) []byte {
	s := spec
	if s == nil {
		s = minSpec
	}
	b, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     s,
	})
	return b
}

func spotQueueClusterBody(name, region, queueURL string) []byte {
	return clusterBody(name, map[string]any{
		"hostedCluster": map[string]any{
			"release":    map[string]any{"image": ""},
			"networking": map[string]any{},
			"platform": map[string]any{
				"type": "AWS",
				"aws": map[string]any{
					"region":                     region,
					"terminationHandlerQueueURL": queueURL,
				},
			},
		},
	})
}

// decodeErrorMessage decodes a metav1.Status response and returns the message field.
func decodeErrorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var errResp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	msg, _ := errResp["message"].(string)
	return msg
}

func TestClusterHandler_List_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testClusterCR("uuid-1", "cluster-1", testAccountID),
		testClusterCR("uuid-2", "cluster-2", testAccountID),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters", nil)
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

func TestClusterHandler_List_Empty(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters", nil)
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
}

func TestClusterHandler_List_Pagination(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testClusterCR("uuid-c1", "c1", testAccountID),
		testClusterCR("uuid-c2", "c2", testAccountID),
		testClusterCR("uuid-c3", "c3", testAccountID),
		testClusterCR("uuid-foreign", "foreign", "999999999999"),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters?limit=2&offset=1", nil)
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
	items := result["items"].([]any)
	if len(items) != 2 {
		t.Errorf("expected 2 items (offset=1, limit=2 of 3), got %d", len(items))
	}
}

func TestClusterHandler_Create_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	spec := map[string]any{"accountId": "999999999999"}
	for key, value := range minSpec {
		spec[key] = value
	}
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"name":   "my-cluster",
			"labels": map[string]any{seaplanev1alpha1.AccountIDLabel: "999999999999"},
		},
		"spec": spec,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if uid := metaField(result, "uid"); uid == nil || uid == "" {
		t.Error("expected non-empty cluster UID in metadata.uid")
	}
	if name := metaField(result, "name"); name != "my-cluster" {
		t.Errorf("expected metadata.name=my-cluster, got %v", name)
	}

	var stored seaplanev1alpha1.Cluster
	if err := fc.Get(req.Context(), client.ObjectKey{Namespace: "account-" + testAccountID, Name: "my-cluster"}, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.AccountID != testAccountID || stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("stored ownership = (%q, %q), want caller account %q", stored.Spec.AccountID, stored.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
	}
	if len(stored.Labels) != 1 || stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("stored Cluster labels = %v, want only the account label", stored.Labels)
	}
	if _, ok := stored.Labels[seaplanev1alpha1.ClusterUIDLabel]; ok {
		t.Errorf("Cluster must fall back to its database UID for sharding, got cluster UID label %q", stored.Labels[seaplanev1alpha1.ClusterUIDLabel])
	}
}

func TestClusterHandler_Create_RejectsSpotTerminationQueueRegionMismatch(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(spotQueueClusterBody("my-cluster", "us-west-2", queueURL)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "same AWS region") {
		t.Errorf("expected region validation error, got %q", w.Body.String())
	}
}

func TestClusterHandler_Create_RejectsDuplicateSpotTerminationQueueURL(t *testing.T) {
	scheme := newTestScheme()
	existing := testClusterCR("existing", "existing-cluster", testAccountID)
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	existing.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(spotQueueClusterBody("my-cluster", "us-east-1", queueURL)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unique across clusters") {
		t.Errorf("expected uniqueness validation error, got %q", w.Body.String())
	}
}

func TestClusterHandler_Create_WithoutDNSReservationDefersAllocationToOperator(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	spec := map[string]any{"hostedCluster": minSpec["hostedCluster"]}
	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", spec)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when dnsReservationId is omitted, got %d: %s", w.Code, w.Body.String())
	}
	var response public.Cluster
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode Cluster response: %v", err)
	}
	if response.Spec.DNSReservationID != "" {
		t.Errorf("DNSReservationID = %q before operator fallback, want empty", response.Spec.DNSReservationID)
	}

	stored, err := seaplanedb.NewClientFrom(fc, logger).GetCluster(req.Context(), "my-cluster")
	if err != nil {
		t.Fatalf("get created Cluster: %v", err)
	}
	if stored.Spec.DNSReservationID != "" {
		t.Errorf("stored DNSReservationID = %q before operator fallback, want empty", stored.Spec.DNSReservationID)
	}
	if stored.Spec.HostedClusterName != "my-cluster" {
		t.Errorf("stored HostedClusterName = %q, want customer name for automatic DNS", stored.Spec.HostedClusterName)
	}
}

func TestClusterHandler_Create_LongNameWithDNSReservation(t *testing.T) {
	clusterName := "customer-cluster-name-longer-than-eighteen"
	scheme := newTestScheme()
	reservation := testDNSReservationCR(testAccountID)
	reservation.Status.InternalHostedClusterName = "hc-authoritative"
	fc := newClusterTestClientBuilderWithDNSReservation(scheme, reservation).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody(clusterName, nil)))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()
	handler.Create(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for long name with DNS reservation, got %d: %s", w.Code, w.Body.String())
	}

	stored, err := seaplanedb.NewClientFrom(fc, logger).GetCluster(req.Context(), clusterName)
	if err != nil {
		t.Fatalf("get created Cluster: %v", err)
	}
	wantHostedClusterName := reservation.Status.InternalHostedClusterName
	if stored.Spec.HostedClusterName != wantHostedClusterName {
		t.Errorf("HostedClusterName = %q, want reservation-derived name %q", stored.Spec.HostedClusterName, wantHostedClusterName)
	}
}

func TestClusterHandler_Create_LongNameWithoutDNSReservationInvalidName(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
	spec := map[string]any{"hostedCluster": minSpec["hostedCluster"]}
	name := strings.Repeat("a", seaplanev1alpha1.MaxClusterNameLengthWithoutDNSReservation()+1)
	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody(name, spec)))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without DNS reservation for long name, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateInvalidName.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateInvalidName.Code, msg)
	}
}

func TestClusterHandler_Create_SetsCreatorARN(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", nil)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// CreatorARN is a service-set field — not exposed in the public response.
	// Verify it was stored correctly in the internal CRD.
	var list seaplanev1alpha1.ClusterList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("listing clusters from fake client: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 cluster in store, got %d", len(list.Items))
	}
	wantARN := "arn:aws:iam::" + testAccountID + ":user/test"
	if list.Items[0].Spec.CreatorARN != wantARN {
		t.Errorf("expected CreatorARN=%s, got %s", wantARN, list.Items[0].Spec.CreatorARN)
	}
}

func TestClusterHandler_Create_InvalidJSON(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader([]byte("not json")))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestClusterHandler_Create_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"missing name", clusterBody("", map[string]any{"hostedCluster": map[string]any{}})},
		{"empty metadata", []byte(`{}`)},
		{"missing spec key", []byte(`{"metadata":{"name":"my-cluster"}}`)},
		{"null spec", []byte(`{"metadata":{"name":"my-cluster"},"spec":null}`)},
		{"empty spec object", clusterBody("my-cluster", map[string]any{})},
		{"whitespace spec", []byte(`{"metadata":{"name":"my-cluster"},"spec":{ }}`)},
		{"whitespace with newline spec", []byte(`{"metadata":{"name":"my-cluster"},"spec":{
}}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

			req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(tt.body))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestClusterHandler_Create_InvalidName(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
	}{
		{name: "too long", clusterName: strings.Repeat("a", 64)},
		{name: "uppercase", clusterName: "MyCluster"},
		{name: "invalid character", clusterName: "my_cluster"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := newClusterTestClientBuilder(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

			req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody(tt.clusterName, nil)))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateInvalidName.Code) {
				t.Errorf("expected message to contain %s, got %q", ErrClusterCreateInvalidName.Code, msg)
			}
		})
	}
}

// TestClusterHandler_Create_LegacyPath_NoOidcConfig verifies that omitting
// oidcConfigId auto-generates issuerURL from the handler's base URL.
func TestClusterHandler_Create_LegacyPath_NoOidcConfig(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	const baseURL = "https://oidc.example.com"
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), baseURL, 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", nil)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	clusterID, _ := metaField(result, "uid").(string)
	if clusterID == "" {
		t.Fatal("expected non-empty cluster UID in metadata.uid")
	}

	var list seaplanev1alpha1.ClusterList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("listing clusters from fake client: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 cluster in store, got %d", len(list.Items))
	}
	wantIssuerURL := baseURL + "/" + clusterID
	if got := list.Items[0].Spec.HostedCluster.IssuerURL; got != wantIssuerURL {
		t.Errorf("expected issuerURL=%s, got %s", wantIssuerURL, got)
	}
	if got := list.Items[0].Spec.OidcConfigID; got != "" {
		t.Errorf("expected empty oidcConfigId on legacy-path cluster, got %s", got)
	}
	hasCleanupFinalizer := false
	for _, finalizer := range list.Items[0].Finalizers {
		if finalizer == clusterCleanupFinalizer {
			hasCleanupFinalizer = true
			break
		}
	}
	if !hasCleanupFinalizer {
		t.Errorf("new Cluster did not have cleanup finalizer at insertion: %v", list.Items[0].Finalizers)
	}
}

func TestClusterHandler_Create_OidcConfigNotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateOidcConfigNotFound.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateOidcConfigNotFound.Code, msg)
	}
}

// oidcConfigListFailingClient simulates a non-NotFound failure when resolving an
// OidcConfig by UID, to distinguish that case from a genuine 404.
type oidcConfigListFailingClient struct {
	client.Client
}

func (c *oidcConfigListFailingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*seaplanev1alpha1.OidcConfigList); ok {
		return fmt.Errorf("simulated database error")
	}
	return c.Client.List(ctx, list, opts...)
}

func TestClusterHandler_Create_OidcConfigLookupFailure(t *testing.T) {
	scheme := newTestScheme()
	fc := &oidcConfigListFailingClient{Client: newClusterTestClientBuilder(scheme).Build()}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateOidcConfigLookupFailed.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateOidcConfigLookupFailed.Code, msg)
	}
}

func TestClusterHandler_Create_OidcConfigWrongAccount(t *testing.T) {
	otherAccount := "999999999999"
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).WithObjects(
		testReadyOidcConfig(testOidcConfigID, otherAccount, testOidcConfigIssuerURL),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	// The OidcConfig exists but belongs to a different account; it must not
	// be resolvable by a caller in testAccountID.
	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClusterHandler_Create_UnmanagedOidcConfigNotReady(t *testing.T) {
	scheme := newTestScheme()
	pending := testOidcConfigCR(testOidcConfigID, testAccountID, testUnmanagedOidcConfigSpec(testAccountID))
	pending.Status.Phase = seaplanev1alpha1.OidcConfigPhasePending
	fc := newClusterTestClientBuilder(scheme).WithObjects(pending).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateOidcConfigNotReady.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateOidcConfigNotReady.Code, msg)
	}
}

func TestClusterHandler_Create_ManagedOidcConfigPending_Succeeds(t *testing.T) {
	scheme := newTestScheme()
	pending := testOidcConfigCR(testOidcConfigID, testAccountID, testManagedOidcConfigSpec(testAccountID))
	pending.Spec.IssuerUrl = testOidcConfigIssuerURL
	pending.Status.Phase = seaplanev1alpha1.OidcConfigPhasePending
	fc := newClusterTestClientBuilder(scheme).WithObjects(pending).
		WithStatusSubresource(pending).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var list seaplanev1alpha1.ClusterList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("listing clusters from fake client: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 cluster in store, got %d", len(list.Items))
	}
	if got := list.Items[0].Spec.HostedCluster.IssuerURL; got != testOidcConfigIssuerURL {
		t.Errorf("expected issuerURL=%s, got %s", testOidcConfigIssuerURL, got)
	}
	if got := list.Items[0].Spec.OidcConfigID; got != testOidcConfigID {
		t.Errorf("expected oidcConfigId=%s, got %s", testOidcConfigID, got)
	}
}

func TestClusterHandler_Create_OidcConfigError_Rejected(t *testing.T) {
	for _, spec := range []seaplanev1alpha1.OidcConfigSpec{
		testManagedOidcConfigSpec(testAccountID),
		testUnmanagedOidcConfigSpec(testAccountID),
	} {
		scheme := newTestScheme()
		errored := testOidcConfigCR(testOidcConfigID, testAccountID, spec)
		errored.Status.Phase = seaplanev1alpha1.OidcConfigPhaseError
		fc := newClusterTestClientBuilder(scheme).WithObjects(errored).Build()
		logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
		handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

		req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
		req = req.WithContext(testContext(testAccountID))

		w := httptest.NewRecorder()
		handler.Create(w, req)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("type=%s: expected 422, got %d: %s", spec.Type, w.Code, w.Body.String())
		}
		if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateOidcConfigNotReady.Code) {
			t.Errorf("type=%s: expected message to contain %s, got %q", spec.Type, ErrClusterCreateOidcConfigNotReady.Code, msg)
		}
	}
}

func TestClusterHandler_Create_OidcConfigAlreadyInUse(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	existingCluster := testClusterCR("existing-cluster-id", "existing-cluster", testAccountID)
	oidcConfig.Labels = map[string]string{claimedByClusterUIDLabel: string(existingCluster.UID)}
	existingCluster.Spec.OidcConfigID = testOidcConfigID
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfig, existingCluster).
		WithStatusSubresource(oidcConfig).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("new-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateOidcConfigInUse.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateOidcConfigInUse.Code, msg)
	}

	var list seaplanev1alpha1.ClusterList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("listing clusters from fake client: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected the rejected create to leave only the pre-existing cluster, got %d", len(list.Items))
	}
}

func TestClusterHandler_Create_OidcConfigInUseDifferentAccountAllowed(t *testing.T) {
	otherAccount := "999999999999"
	scheme := newTestScheme()
	oidcConfigOther := testReadyOidcConfig(testOidcConfigID, otherAccount, testOidcConfigIssuerURL)
	existingCluster := testClusterCR("existing-cluster-id", "existing-cluster", otherAccount)
	existingCluster.Spec.OidcConfigID = testOidcConfigID
	oidcConfigMine := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfigOther, existingCluster, oidcConfigMine).
		WithStatusSubresource(oidcConfigOther, oidcConfigMine).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("new-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (same oidcConfigId owned by a different account is unrelated), got %d: %s", w.Code, w.Body.String())
	}
}

// Races two real Creates for the same oidcConfigId against a plain fake client (no DB backstop);
// verifies the label CAS in claimOidcConfig alone enforces the 1:1 binding post-migration-004.
func TestClusterHandler_Create_ConcurrentOidcConfigCollision(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfig).
		WithStatusSubresource(oidcConfig).Build()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	var wg sync.WaitGroup
	names := []string{"concurrent-a", "concurrent-b"}
	codes := make([]int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody(names[idx], specWithOidcConfigID(testOidcConfigID))))
			req = req.WithContext(testContext(testAccountID))
			w := httptest.NewRecorder()
			handler.Create(w, req)
			codes[idx] = w.Code
		}(i)
	}

	wg.Wait()

	var created, conflict int
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("expected exactly one 201 and one 409 (ErrClusterCreateOidcConfigInUse) for concurrent creates referencing the same oidcConfigId, got codes %v", codes)
	}
}

func TestClusterHandler_Create_OidcClaimFailureDeletesForOperatorCleanup(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	oidcConfig.Labels = map[string]string{claimedByClusterUIDLabel: "another-cluster-uid"}
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfig).WithStatusSubresource(oidcConfig).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)
	cluster := testClusterCR("rollback-cluster-uid", "rollback-cluster", testAccountID)
	cluster.Spec.OidcConfigID = string(oidcConfig.UID)
	cluster.Finalizers = []string{clusterCleanupFinalizer}
	if err := handler.db.CreateCluster(testContext(testAccountID), cluster); err != nil {
		t.Fatalf("create Cluster: %v", err)
	}
	reservation, err := handler.db.GetDNSReservationByUID(testContext(testAccountID), testDNSReservationUID)
	if err != nil {
		t.Fatalf("get DNS reservation: %v", err)
	}

	_, apiErr := handler.completeClusterCreate(testContext(testAccountID), testAccountID, cluster.Name, cluster, reservation, oidcConfig)
	if apiErr == nil || apiErr.Code != ErrClusterCreateOidcConfigInUse.Code {
		t.Fatalf("completeClusterCreate API error = %v, want %s", apiErr, ErrClusterCreateOidcConfigInUse.Code)
	}
	deletingCluster, err := handler.db.GetCluster(testContext(testAccountID), cluster.Name)
	if err != nil {
		t.Fatalf("get Cluster after failed claim: %v", err)
	}
	if deletingCluster.DeletionTimestamp.IsZero() {
		t.Fatal("failed Cluster create was not marked for deletion")
	}
	if len(deletingCluster.Finalizers) != 1 || deletingCluster.Finalizers[0] != clusterCleanupFinalizer {
		t.Fatalf("cleanup finalizer = %v, want %s", deletingCluster.Finalizers, clusterCleanupFinalizer)
	}

	reservation, err = handler.db.GetDNSReservationByUID(testContext(testAccountID), testDNSReservationUID)
	if err != nil {
		t.Fatalf("get DNS reservation after rollback: %v", err)
	}
	if got := reservation.Labels[claimedByClusterUIDLabel]; got != string(cluster.UID) {
		t.Fatalf("reservation claim = %q, want Cluster UID %q until operator finalizer cleanup", got, cluster.UID)
	}
}

func TestClusterHandler_ClaimDNSReservationRetriesUpdateConflict(t *testing.T) {
	scheme := newTestScheme()
	updateCalls := 0
	fc := newClusterTestClientBuilder(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			reservation, ok := obj.(*seaplanev1alpha1.DNSReservation)
			if !ok {
				return c.Update(ctx, obj, opts...)
			}
			updateCalls++
			if updateCalls == 1 {
				var latest seaplanev1alpha1.DNSReservation
				if err := c.Get(ctx, client.ObjectKeyFromObject(reservation), &latest); err != nil {
					return err
				}
				latest.Finalizers = append(latest.Finalizers, "test.example/controller")
				if err := c.Update(ctx, &latest, opts...); err != nil {
					return err
				}
				return apierrors.NewConflict(seaplanev1alpha1.GroupVersion.WithResource("dnsreservations").GroupResource(), reservation.Name, errors.New("concurrent finalizer update"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)
	clusterUID := "new-cluster-uid"

	if apiErr := handler.claimDNSReservation(testContext(testAccountID), testAccountID, testDNSReservationUID, clusterUID); apiErr != nil {
		t.Fatalf("claimDNSReservation returned error: %v", apiErr)
	}
	if updateCalls != 2 {
		t.Fatalf("reservation update calls = %d, want 2 after retry", updateCalls)
	}

	reservation, err := handler.db.GetDNSReservation(testContext(testAccountID), "test-reservation")
	if err != nil {
		t.Fatalf("get claimed reservation: %v", err)
	}
	if reservation.Labels[claimedByClusterUIDLabel] != clusterUID {
		t.Errorf("claimed-by-cluster-uid = %q, want %q", reservation.Labels[claimedByClusterUIDLabel], clusterUID)
	}
	if len(reservation.Finalizers) != 1 || reservation.Finalizers[0] != "test.example/controller" {
		t.Errorf("controller finalizers were not preserved after retry: %v", reservation.Finalizers)
	}
}

func TestClusterHandler_ClaimDNSReservationReturnsInUseAfterConflictRecheck(t *testing.T) {
	scheme := newTestScheme()
	updateCalls := 0
	fc := newClusterTestClientBuilder(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			reservation, ok := obj.(*seaplanev1alpha1.DNSReservation)
			if !ok {
				return c.Update(ctx, obj, opts...)
			}
			updateCalls++
			if updateCalls == 1 {
				var latest seaplanev1alpha1.DNSReservation
				if err := c.Get(ctx, client.ObjectKeyFromObject(reservation), &latest); err != nil {
					return err
				}
				latest.Labels[claimedByClusterUIDLabel] = "another-cluster-uid"
				if err := c.Update(ctx, &latest, opts...); err != nil {
					return err
				}
				return apierrors.NewConflict(seaplanev1alpha1.GroupVersion.WithResource("dnsreservations").GroupResource(), reservation.Name, errors.New("concurrent claim"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	apiErr := handler.claimDNSReservation(testContext(testAccountID), testAccountID, testDNSReservationUID, "new-cluster-uid")
	if apiErr == nil || apiErr.Code != ErrClusterCreateDNSReservationInUse.Code {
		t.Fatalf("claimDNSReservation error = %v, want %s", apiErr, ErrClusterCreateDNSReservationInUse.Code)
	}
	if updateCalls != 1 {
		t.Fatalf("reservation update calls = %d, want only the conflicting update", updateCalls)
	}
}

func TestClusterHandler_ClaimOidcConfigRetriesUpdateConflict(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	updateCalls := 0
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfig).WithStatusSubresource(oidcConfig).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			config, ok := obj.(*seaplanev1alpha1.OidcConfig)
			if !ok {
				return c.Update(ctx, obj, opts...)
			}
			updateCalls++
			if updateCalls == 1 {
				var latest seaplanev1alpha1.OidcConfig
				if err := c.Get(ctx, client.ObjectKeyFromObject(config), &latest); err != nil {
					return err
				}
				latest.Finalizers = append(latest.Finalizers, "test.example/controller")
				if err := c.Update(ctx, &latest, opts...); err != nil {
					return err
				}
				return apierrors.NewConflict(seaplanev1alpha1.GroupVersion.WithResource("oidcconfigs").GroupResource(), config.Name, errors.New("concurrent finalizer update"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)
	clusterUID := "new-cluster-uid"

	if apiErr := handler.claimOidcConfig(testContext(testAccountID), testAccountID, testOidcConfigID, clusterUID); apiErr != nil {
		t.Fatalf("claimOidcConfig returned error: %v", apiErr)
	}
	if updateCalls != 2 {
		t.Fatalf("OidcConfig update calls = %d, want 2 after retry", updateCalls)
	}

	var claimed seaplanev1alpha1.OidcConfig
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "account-" + testAccountID, Name: testOidcConfigID}, &claimed); err != nil {
		t.Fatalf("get claimed OidcConfig: %v", err)
	}
	if claimed.Labels[claimedByClusterUIDLabel] != clusterUID {
		t.Errorf("claimed-by-cluster-uid = %q, want %q", claimed.Labels[claimedByClusterUIDLabel], clusterUID)
	}
	if len(claimed.Finalizers) != 1 || claimed.Finalizers[0] != "test.example/controller" {
		t.Errorf("controller finalizers were not preserved after retry: %v", claimed.Finalizers)
	}
}

func TestClusterHandler_Create_DerivesIssuerURLAndUpdatesLastUsed(t *testing.T) {
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	fc := newClusterTestClientBuilder(scheme).WithObjects(oidcConfig).
		WithStatusSubresource(oidcConfig).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("my-cluster", specWithOidcConfigID(testOidcConfigID))))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// issuerURL is service-set (not writable by the caller) but remains
	// visible in the response, derived from the referenced OidcConfig.
	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	spec, _ := result["spec"].(map[string]any)
	hostedCluster, _ := spec["hostedCluster"].(map[string]any)
	if got := hostedCluster["issuerURL"]; got != testOidcConfigIssuerURL {
		t.Errorf("expected issuerURL=%s in response, got %v", testOidcConfigIssuerURL, got)
	}

	// ...and it must be persisted on the internal CRD, derived from the OidcConfig.
	var list seaplanev1alpha1.ClusterList
	if err := fc.List(context.Background(), &list); err != nil {
		t.Fatalf("listing clusters from fake client: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 cluster in store, got %d", len(list.Items))
	}
	if got := list.Items[0].Spec.HostedCluster.IssuerURL; got != testOidcConfigIssuerURL {
		t.Errorf("expected issuerURL=%s, got %s", testOidcConfigIssuerURL, got)
	}
	if got, want := list.Items[0].Spec.HostedClusterName, seaplanev1alpha1.HostedClusterNameFromDNSReservation(testDNSReservationCR(testAccountID)); got != want {
		t.Errorf("HostedClusterName = %q, want DNSReservation UID-derived name %q", got, want)
	}

	// The referenced OidcConfig's lastUsedTimestamp must be stamped.
	var updated seaplanev1alpha1.OidcConfig
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(oidcConfig), &updated); err != nil {
		t.Fatalf("getting oidc config: %v", err)
	}
	if updated.Status.LastUsedTimestamp == nil {
		t.Error("expected status.lastUsedTimestamp to be set after cluster creation")
	}
	if updated.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("OIDC account label = %q, want %q", updated.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
	}
}

func TestClusterHandler_Get_Success(t *testing.T) {
	cr := testClusterCR("cluster-123", "test-cluster", testAccountID)
	cr.Status = seaplanev1alpha1.ClusterStatus{
		ObservedGeneration: 1,
		Phase:              "Ready",
		Conditions: []metav1.Condition{
			{Type: "Available", Status: metav1.ConditionTrue, Reason: "AsExpected"},
		},
	}
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).
		WithStatusSubresource(cr).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters/test-cluster", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})

	w := httptest.NewRecorder()
	handler.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	if uid := metaField(result, "uid"); uid != "cluster-123" {
		t.Errorf("expected metadata.uid=cluster-123, got %v", uid)
	}
	if name := metaField(result, "name"); name != "test-cluster" {
		t.Errorf("expected metadata.name=test-cluster, got %v", name)
	}
	// Status is included in GET — no separate /statuses endpoint.
	status, _ := result["status"].(map[string]any)
	if status["phase"] != "Ready" {
		t.Errorf("expected status.phase=Ready, got %v", status["phase"])
	}
}

func TestClusterHandler_Get_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodGet, "/api/v0/clusters/no-such-cluster", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "no-such-cluster"})

	w := httptest.NewRecorder()
	handler.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterGetNotFound.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterGetNotFound.Code, msg)
	}
}

func TestClusterHandler_CrossAccountOperations(t *testing.T) {
	foreign := testClusterCR("foreign-id", "foreign", "999999999999")
	fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(foreign).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	tests := []struct {
		name, method, body string
		invoke             func(http.ResponseWriter, *http.Request)
	}{
		{"get", http.MethodGet, "", handler.Get},
		{"update", http.MethodPut, `{"spec":{"properties":{"changed":"true"}}}`, handler.Update},
		{"delete", http.MethodDelete, "", handler.Delete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertHandlerStatus(t, http.StatusNotFound, tc.method, "/api/v0/clusters/"+foreign.Name, tc.body,
				map[string]string{"id": foreign.Name}, testAccountID, tc.invoke)
		})
	}

	var stored seaplanev1alpha1.Cluster
	if err := fc.Get(context.Background(), client.ObjectKeyFromObject(foreign), &stored); err != nil {
		t.Fatalf("foreign cluster was modified: %v", err)
	}
	if stored.Spec.Properties["changed"] == "true" {
		t.Error("foreign cluster was updated")
	}
}

func TestClusterHandler_Delete_Success(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testClusterCR("cluster-123", "test-cluster", testAccountID),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/clusters/test-cluster", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	// Delete returns a plain map (not a K8s type) with the cluster name.
	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result["cluster_name"] != "test-cluster" {
		t.Errorf("expected cluster_name=test-cluster, got %v", result["cluster_name"])
	}
}

func TestClusterHandler_Delete_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	req := httptest.NewRequest(http.MethodDelete, "/api/v0/clusters/no-such-cluster", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "no-such-cluster"})

	w := httptest.NewRecorder()
	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestClusterHandler_Update_Success(t *testing.T) {
	scheme := newTestScheme()
	cr := testClusterCR("cluster-123", "test-cluster", testAccountID)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{seaplanev1alpha1.AccountIDLabel: "999999999999"},
		},
		"spec": map[string]any{
			"properties": map[string]string{"environment": "test"},
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]any
	_ = json.NewDecoder(w.Body).Decode(&result)

	// Verify the mutable spec field was merged into the response.
	spec, _ := result["spec"].(map[string]any)
	properties, _ := spec["properties"].(map[string]any)
	if properties["environment"] != "test" {
		t.Errorf("expected spec.properties.environment=test, got %v", properties["environment"])
	}

	var stored seaplanev1alpha1.Cluster
	if err := fc.Get(req.Context(), client.ObjectKeyFromObject(cr), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("stored account label = %q, want %q", stored.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
	}
}

func TestClusterHandler_Update_ReplacesAndClearsTags(t *testing.T) {
	cluster := testClusterCR("cluster-123", "test-cluster", testAccountID)
	cluster.Spec.Tags = map[string]string{"owner": "old", "remove": "this"}
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	updateTags := func(tags map[string]string) public.Cluster {
		t.Helper()
		body := mustMarshal(t, map[string]any{"spec": map[string]any{"tags": tags}})
		req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
		req = req.WithContext(testContext(testAccountID))
		req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})
		w := httptest.NewRecorder()
		handler.Update(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var result public.Cluster
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode cluster response: %v", err)
		}
		return result
	}

	result := updateTags(map[string]string{"owner": "new"})
	if len(result.Spec.Tags) != 1 || result.Spec.Tags["owner"] != "new" {
		t.Errorf("response tags = %v, want only owner=new", result.Spec.Tags)
	}
	stored, err := handler.db.GetCluster(testContext(testAccountID), "test-cluster")
	if err != nil {
		t.Fatalf("get updated cluster: %v", err)
	}
	if len(stored.Spec.Tags) != 1 || stored.Spec.Tags["owner"] != "new" {
		t.Errorf("stored tags = %v, want only owner=new", stored.Spec.Tags)
	}

	result = updateTags(map[string]string{})
	if len(result.Spec.Tags) != 0 {
		t.Errorf("response tags after empty-map update = %v, want empty", result.Spec.Tags)
	}
	stored, err = handler.db.GetCluster(testContext(testAccountID), "test-cluster")
	if err != nil {
		t.Fatalf("get cluster after clearing tags: %v", err)
	}
	if len(stored.Spec.Tags) != 0 {
		t.Errorf("stored tags after empty-map update = %v, want empty", stored.Spec.Tags)
	}
}

func TestClusterHandler_Update_RejectsTagsExceedingNodePoolBudget(t *testing.T) {
	cluster := testClusterCR("cluster-123", "test-cluster", testAccountID)
	nodePool := testNodePoolCR("test-cluster.workers", cluster)
	nodePool.Spec.NodePool.ClusterName = cluster.Name
	nodePool.Spec.Tags = map[string]string{
		"pool-one": "value",
		"pool-two": "value",
	}
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, nodePool).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
	tags := make(map[string]string)
	for i := range 25 - 2 {
		tags[fmt.Sprintf("customer-%d", i)] = "value"
	}
	body := mustMarshal(t, map[string]any{"spec": map[string]any{"tags": tags}})
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "test-cluster.workers") {
		t.Errorf("expected NodePool-specific tag budget error, got %q", w.Body.String())
	}
}

func TestClusterHandler_Update_RejectsDuplicateSpotTerminationQueueURL(t *testing.T) {
	scheme := newTestScheme()
	target := testClusterCR("target-uid", "target", testAccountID)
	existing := testClusterCR("existing", "existing-cluster", testAccountID)
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	existing.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target, existing).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"hostedCluster": map[string]any{
				"platform": map[string]any{
					"aws": map[string]any{"terminationHandlerQueueURL": queueURL},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshaling update request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/target", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "target"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unique across clusters") {
		t.Errorf("expected uniqueness validation error, got %q", w.Body.String())
	}
}

func TestClusterHandler_Update_RejectsSpotTerminationQueueRegionMismatchWhenRegionChanges(t *testing.T) {
	scheme := newTestScheme()
	target := testClusterCR("target-uid", "target", testAccountID)
	const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	target.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = queueURL
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"hostedCluster": map[string]any{
				"platform": map[string]any{
					"aws": map[string]any{"region": "us-west-2"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshaling update request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/target", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "target"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "same AWS region") {
		t.Errorf("expected region validation error, got %q", w.Body.String())
	}
}

func TestClusterHandler_Update_SkipsSpotTerminationQueueLookupWhenUnchanged(t *testing.T) {
	scheme := newTestScheme()
	target := testClusterCR("target-uid", "target", testAccountID)
	target.Spec.HostedCluster.Platform.AWS.TerminationHandlerQueueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/spot-termination"
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listOptions := &client.ListOptions{}
			for _, opt := range opts {
				opt.ApplyToList(listOptions)
			}
			if listOptions.Namespace == "" {
				return errors.New("unexpected account-wide list while the termination queue is unchanged")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body, err := json.Marshal(map[string]any{"spec": map[string]any{"displayName": "updated-display"}})
	if err != nil {
		t.Fatalf("marshaling update request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/target", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "target"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestClusterHandler_Update_NamespaceMismatch(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testClusterCR("cluster-123", "test-cluster", testAccountID),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body := mustMarshal(t, map[string]any{
		"metadata": map[string]any{"namespace": "account-999999999999"},
		"spec":     map[string]any{"properties": map[string]string{"environment": "test"}},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if message := decodeErrorMessage(t, w); !strings.Contains(message, ErrClusterUpdateNamespaceMismatch.Code) {
		t.Errorf("error message = %q, want code %s", message, ErrClusterUpdateNamespaceMismatch.Code)
	}
}

func TestClusterHandler_Update_AllowsUnchangedServiceManagedIssuerURL(t *testing.T) {
	scheme := newTestScheme()
	cluster := testClusterCR("cluster-123", "test-cluster", testAccountID)
	cluster.Spec.HostedCluster.IssuerURL = "https://oidc.example.com/issuer"
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	body, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"hostedCluster": map[string]any{"issuerURL": cluster.Spec.HostedCluster.IssuerURL},
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unchanged service-managed issuerURL, got %d: %s", w.Code, w.Body.String())
	}
	var updated seaplanev1alpha1.Cluster
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "account-" + testAccountID, Name: "test-cluster"}, &updated); err != nil {
		t.Fatalf("get updated Cluster: %v", err)
	}
	if updated.Spec.HostedCluster.IssuerURL != cluster.Spec.HostedCluster.IssuerURL {
		t.Errorf("issuerURL = %q, want unchanged %q", updated.Spec.HostedCluster.IssuerURL, cluster.Spec.HostedCluster.IssuerURL)
	}
}

// A null parent clears its mutable children but never the platform-managed ones.
func TestClusterHandler_Update_NullParentKeepsServiceSet(t *testing.T) {
	for _, body := range []string{
		`{"spec":{"hostedCluster":{"configuration":{"proxy":null}}}}`,
		`{"spec":{"hostedCluster":{"configuration":null}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			cr := testClusterCR("uuid-1", "owned", testAccountID)
			cr.Spec.HostedCluster.Configuration = &hypershiftv1beta1.ClusterConfiguration{Proxy: &configv1.ProxySpec{HTTPProxy: "http://proxy", TrustedCA: configv1.ConfigMapNameReference{Name: "platform-ca"}}}
			fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(cr).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
			assertHandlerStatus(t, http.StatusOK, http.MethodPut, "/api/v0/clusters/"+cr.Name,
				body, map[string]string{"id": cr.Name}, testAccountID, handler.Update)

			var stored seaplanev1alpha1.Cluster
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(cr), &stored); err != nil {
				t.Fatal(err)
			}
			config := stored.Spec.HostedCluster.Configuration
			if config == nil || config.Proxy == nil || config.Proxy.TrustedCA.Name != "platform-ca" || config.Proxy.HTTPProxy != "" {
				t.Fatalf("want trustedCA kept and httpProxy cleared, got %+v", config)
			}
		})
	}
}

// Wrong or clearing accountId must not change stored ownership (service-set on update).
// Service-set accountId cannot change on update; clearing it is a change too.
func TestClusterHandler_Update_ProtectsAccountID(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"spec":{"accountId":""}}`, http.StatusUnprocessableEntity},
		{`{"spec":{"accountId":"999999999999"}}`, http.StatusUnprocessableEntity},
		{`{"spec":{"AccountId":"999999999999","properties":{"changed":"true"}}}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cr := testClusterCR("uuid-1", "owned", testAccountID)
			fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(cr).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)
			assertHandlerStatus(t, tc.want, http.MethodPut, "/api/v0/clusters/"+cr.Name,
				tc.body, map[string]string{"id": cr.Name}, testAccountID, handler.Update)
			var stored seaplanev1alpha1.Cluster
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(cr), &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Spec.AccountID != testAccountID {
				t.Errorf("stored account ID = %q, want %q", stored.Spec.AccountID, testAccountID)
			}
		})
	}
}

func TestClusterHandler_Update_NotFound(t *testing.T) {
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterReadPermit), logger)

	body, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"properties": map[string]string{"environment": "x"}},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/no-such", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "no-such"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestClusterHandler_Update_MissingSpec(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{"empty spec object", []byte(`{}`)},
		{"whitespace spec", []byte(`{"spec":{ }}`)},
		{"whitespace with newline spec", []byte(`{"spec":{
}}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				testClusterCR("cluster-123", "test-cluster", testAccountID),
			).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "https://oidc.example.com", 0, clusterAuthorizer(t, clusterReadPermit), logger)

			req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/cluster-123", bytes.NewReader(tt.body))
			req = req.WithContext(testContext(testAccountID))
			req = mux.SetURLVars(req, map[string]string{"id": "cluster-123"})

			w := httptest.NewRecorder()
			handler.Update(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

// TestClusterHandler_Update_RejectsOidcConfigIdChange verifies that
// oidcConfigId cannot be changed from one value to another via update.
func TestClusterHandler_Update_RejectsOidcConfigIdChange(t *testing.T) {
	scheme := newTestScheme()
	cr := testClusterCR("cluster-123", "test-cluster", testAccountID)
	cr.Spec.OidcConfigID = testOidcConfigID
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	body, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"oidcConfigId": "some-other-oidc-config",
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterValidation.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterValidation.Code, msg)
	}
}

// TestClusterHandler_Update_RejectsOidcConfigIdAddition verifies a legacy-path
// cluster cannot adopt the OidcConfig-backed path via update.
func TestClusterHandler_Update_RejectsOidcConfigIdAddition(t *testing.T) {
	scheme := newTestScheme()
	cr := testClusterCR("cluster-123", "test-cluster", testAccountID)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, clusterAdminPermit), logger)

	body, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"oidcConfigId": testOidcConfigID,
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/test-cluster", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "test-cluster"})

	w := httptest.NewRecorder()
	handler.Update(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterValidation.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterValidation.Code, msg)
	}
}

func TestClusterHandler_Create_DuplicateName(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).WithObjects(
		testClusterCR("existing-id", "test-cluster", testAccountID),
	).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("test-cluster", nil)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate name, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateNameConflict.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateNameConflict.Code, msg)
	}
}

func TestClusterHandler_Create_DBError(t *testing.T) {
	scheme := newTestScheme()
	fc := newClusterTestClientBuilder(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return errors.New("simulated datastore failure")
		},
	}).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "https://oidc.example.com", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("test-cluster", nil)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the datastore create fails, got %d: %s", w.Code, w.Body.String())
	}
	if msg := decodeErrorMessage(t, w); !strings.Contains(msg, ErrClusterCreateFailed.Code) {
		t.Errorf("expected message to contain %s, got %q", ErrClusterCreateFailed.Code, msg)
	}
}

func TestClusterHandler_Create_SameNameDifferentAccount(t *testing.T) {
	otherAccount := "999999999999"
	scheme := newTestScheme()
	oidcConfig := testReadyOidcConfig(testOidcConfigID, testAccountID, testOidcConfigIssuerURL)
	fc := newClusterTestClientBuilder(scheme).WithObjects(
		testClusterCR("existing-id", "test-cluster", otherAccount),
		oidcConfig,
	).WithStatusSubresource(oidcConfig).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

	req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("test-cluster", nil)))
	req = req.WithContext(testContext(testAccountID))

	w := httptest.NewRecorder()
	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (same name in different account is allowed), got %d: %s", w.Code, w.Body.String())
	}
}

func TestClusterHandler_Create_NetworkCIDRValidation(t *testing.T) {
	tests := []struct {
		name           string
		networking     map[string]any
		expectError    bool
		errorSubstring string
	}{
		{
			name: "valid network configuration",
			networking: map[string]any{
				"machineNetwork": []map[string]any{
					{"cidr": "10.0.0.0/16"},
				},
				"clusterNetwork": []map[string]any{
					{"cidr": "10.128.0.0/14", "hostPrefix": 23},
				},
				"serviceNetwork": []map[string]any{
					{"cidr": "172.30.0.0/16"},
				},
			},
			expectError: false,
		},
		{
			name: "overlapping machine and service CIDRs",
			networking: map[string]any{
				"machineNetwork": []map[string]any{
					{"cidr": "10.0.0.0/16"},
				},
				"serviceNetwork": []map[string]any{
					{"cidr": "10.0.0.0/24"},
				},
			},
			expectError:    true,
			errorSubstring: "overlap",
		},
		{
			name: "machine CIDR contains HyperShift API address",
			networking: map[string]any{
				"machineNetwork": []map[string]any{
					{"cidr": "172.20.0.0/16"},
				},
			},
			expectError:    true,
			errorSubstring: "172.20.0.1",
		},
		{
			name: "invalid host prefix",
			networking: map[string]any{
				"clusterNetwork": []map[string]any{
					{"cidr": "10.128.0.0/14", "hostPrefix": 22},
				},
			},
			expectError:    true,
			errorSubstring: "Host Prefix",
		},
		{
			name: "CIDR address inconsistent with prefix",
			networking: map[string]any{
				"machineNetwork": []map[string]any{
					{"cidr": "10.0.0.5/16"},
				},
			},
			expectError:    true,
			errorSubstring: "isn't consistent with network prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newTestScheme()
			fc := fake.NewClientBuilder().WithScheme(scheme).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, clusterAuthorizer(t, lifecyclePermit), logger)

			spec := map[string]any{
				"hostedCluster": map[string]any{
					"release":    map[string]any{"image": ""},
					"networking": tt.networking,
					"platform":   map[string]any{"type": "AWS"},
				},
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", bytes.NewReader(clusterBody("test-cluster", spec)))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if tt.expectError {
				body := w.Body.String()
				if w.Code != http.StatusUnprocessableEntity {
					t.Errorf("expected 422, got %d: %s", w.Code, body)
				}
				if !strings.Contains(body, ErrClusterValidation.Code) {
					t.Errorf("expected response to contain %s, got %q", ErrClusterValidation.Code, body)
				}
				// Check the full response body for the error substring
				if tt.errorSubstring != "" && !strings.Contains(body, tt.errorSubstring) {
					t.Errorf("expected response body to contain %q, got %q", tt.errorSubstring, body)
				}
			} else {
				if w.Code != http.StatusCreated {
					t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
