package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

func testNodePoolCR(npName string, parent *seaplanev1alpha1.Cluster) *seaplanev1alpha1.NodePool {
	return &seaplanev1alpha1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: parent.Namespace,
			UID:       types.UID(npName + "-uid"),
			Labels: map[string]string{
				seaplanev1alpha1.AccountIDLabel: parent.Labels[seaplanev1alpha1.AccountIDLabel],
				clusterUIDLabel:                 string(parent.UID),
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, seaplanev1alpha1.GroupVersion.WithKind("Cluster"))},
		},
		Spec: seaplanev1alpha1.NodePoolSpec{
			AccountID: parent.Spec.AccountID,
			NodePool:  seaplanev1alpha1.NodePoolSpecPassthrough{ClusterName: parent.Name},
		},
	}
}

func newTestNodePoolHandler(t *testing.T, objects ...client.Object) *NodePoolHandler {
	t.Helper()
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	return NewNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, `permit(principal, action in Seaplane::Action::"AllActions", resource);`), logger)
}

func TestNodePoolHandler_List_PaginationIsolation(t *testing.T) {
	const ownCluster1 = "550e8400-e29b-41d4-a716-446655440000"
	const ownCluster2 = "550e8400-e29b-41d4-a716-446655440001"
	const foreignCluster = "550e8400-e29b-41d4-a716-446655440002"
	first := testClusterCR(ownCluster1, "own-1", testAccountID)
	second := testClusterCR(ownCluster2, "own-2", testAccountID)
	foreign := testClusterCR(foreignCluster, "foreign", "999999999999")
	handler := newTestNodePoolHandler(t,
		first, second, foreign, testNodePoolCR("foreign.workers", foreign),
		testNodePoolCR("own-1.workers", first), testNodePoolCR("own-2.workers", second),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/nodepools?limit=1&offset=1", nil)
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Items []any `json:"items"`
		Total int   `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 || len(response.Items) != 1 {
		t.Errorf("got total=%d items=%d, want total=2 items=1", response.Total, len(response.Items))
	}
}

func TestNodePoolHandler_Create_SetsAccountOwnership(t *testing.T) {
	const clusterID = "550e8400-e29b-41d4-a716-446655440000"
	handler := newTestNodePoolHandler(t, testClusterCR(clusterID, "parent", testAccountID))
	body := mustMarshal(t, map[string]any{
		"metadata": map[string]any{
			"name":      "parent.workers",
			"namespace": "account-" + testAccountID,
			"labels": map[string]any{
				seaplanev1alpha1.AccountIDLabel: "999999999999",
			},
		},
		"spec": map[string]any{"nodePool": map[string]any{"replicas": 2}, "accountId": "999999999999"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	stored, err := handler.db.GetNodePool(req.Context(), "parent.workers")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.AccountID != testAccountID || stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
		t.Errorf("stored ownership = (%q, %q), want caller account %q", stored.Spec.AccountID, stored.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
	}
	if stored.Labels[clusterUIDLabel] != clusterID {
		t.Errorf("cluster UID shard label = %q, want parent UID %q", stored.Labels[clusterUIDLabel], clusterID)
	}
}

func TestNodePoolHandler_CrossAccountOperations(t *testing.T) {
	const clusterID = "550e8400-e29b-41d4-a716-446655440000"
	parent := testClusterCR(clusterID, "foreign", "999999999999")
	foreign := testNodePoolCR("foreign.workers", parent)
	foreign.Namespace = "account-" + testAccountID
	foreign.Labels[seaplanev1alpha1.AccountIDLabel] = testAccountID
	handler := newTestNodePoolHandler(t, parent, foreign)

	tests := []struct {
		name, method, path, body string
		invoke                   func(http.ResponseWriter, *http.Request)
	}{
		{"create", http.MethodPost, "/api/v0/nodepools", `{"metadata":{"name":"foreign.new"},"spec":{"nodePool":{}}}`, handler.Create},
		{"get", http.MethodGet, "/api/v0/nodepools/foreign.workers", "", handler.Get},
		{"update", http.MethodPut, "/api/v0/nodepools/foreign.workers", `{"spec":{"labels":{"changed":"true"}}}`, handler.Update},
		{"delete", http.MethodDelete, "/api/v0/nodepools/foreign.workers", "", handler.Delete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertHandlerStatus(t, http.StatusNotFound, tc.method, tc.path, tc.body,
				map[string]string{"id": foreign.Name}, testAccountID, tc.invoke)
		})
	}
}

func TestNodePoolHandler_SameAccountOperations(t *testing.T) {
	const clusterID = "550e8400-e29b-41d4-a716-446655440000"
	tests := []struct {
		name, method, body string
		wantStatus         int
		invoke             func(*NodePoolHandler, http.ResponseWriter, *http.Request)
	}{
		{"get", http.MethodGet, "", http.StatusOK, func(h *NodePoolHandler, w http.ResponseWriter, r *http.Request) { h.Get(w, r) }},
		{"update", http.MethodPut, `{"metadata":{"labels":{"seaplane.io/account-id":"999999999999"}},"spec":{"labels":{"changed":"true"}}}`, http.StatusOK, func(h *NodePoolHandler, w http.ResponseWriter, r *http.Request) { h.Update(w, r) }},
		{"delete", http.MethodDelete, "", http.StatusAccepted, func(h *NodePoolHandler, w http.ResponseWriter, r *http.Request) { h.Delete(w, r) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := testClusterCR(clusterID, "parent", testAccountID)
			handler := newTestNodePoolHandler(t, parent, testNodePoolCR("parent.workers", parent))
			req := httptest.NewRequest(tc.method, "/api/v0/nodepools/parent.workers", bytes.NewBufferString(tc.body))
			req = req.WithContext(testContext(testAccountID))
			req = mux.SetURLVars(req, map[string]string{"id": "parent.workers"})
			w := httptest.NewRecorder()

			tc.invoke(handler, w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("expected %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.name == "update" {
				stored, err := handler.db.GetNodePool(req.Context(), "parent.workers")
				if err != nil {
					t.Fatal(err)
				}
				if stored.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
					t.Errorf("stored account label = %q, want %q", stored.Labels[seaplanev1alpha1.AccountIDLabel], testAccountID)
				}
			}
		})
	}
}

// TestNodePoolHandler_Update_MissingSpec verifies that PUT /nodepools/{id} with
// an absent or empty spec is rejected with 400 before any DB write.
func TestNodePoolHandler_Update_MissingSpec(t *testing.T) {
	parent := testClusterCR("parent-uid", "test-cluster", testAccountID)
	np := testNodePoolCR("test-cluster.workers", parent)
	handler := newTestNodePoolHandler(t, parent, np)

	cases := []struct {
		name string
		body []byte
	}{
		{"no spec key", mustMarshal(t, map[string]any{})},
		{"empty spec object", mustMarshal(t, map[string]any{"spec": map[string]any{}})},
		{"whitespace spec", []byte(`{"spec":{ }}`)},
		{"whitespace with newline spec", []byte(`{"spec":{
}}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/test-cluster.workers", bytes.NewReader(tc.body))
			req = req.WithContext(testContext(testAccountID))
			req = mux.SetURLVars(req, map[string]string{"id": "test-cluster.workers"})

			w := httptest.NewRecorder()
			handler.Update(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
			var errResp map[string]any
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			msg, _ := errResp["message"].(string)
			if msg == "" {
				t.Errorf("expected non-empty error message")
			}
		})
	}
}

func TestNodePoolHandler_Update_AllowsUnchangedServiceManagedManagement(t *testing.T) {
	autoRepair := true
	replicas := int32(2)
	parent := testClusterCR("parent-uid", "my-cluster", testAccountID)
	nodePool := testNodePoolCR("my-cluster.workers", parent)
	nodePool.Spec = seaplanev1alpha1.NodePoolSpec{
		AutoRepair: &autoRepair,
		NodePool: seaplanev1alpha1.NodePoolSpecPassthrough{
			ClusterName: "my-cluster",
			Replicas:    &replicas,
			Management: hypershiftv1beta1.NodePoolManagement{
				AutoRepair: true,
			},
		},
	}

	scheme := newTestScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, nodePool).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, lifecyclePermit), logger)
	request := seaplanedb.InternalToPublicNodePool(nodePool)
	updatedReplicas := int32(3)
	request.Spec.NodePool.Replicas = &updatedReplicas

	req := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+nodePool.Name, bytes.NewReader(mustMarshal(t, request)))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": nodePool.Name})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var updated seaplanev1alpha1.NodePool
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: nodePool.Namespace, Name: nodePool.Name}, &updated); err != nil {
		t.Fatalf("get updated NodePool: %v", err)
	}
	if got := updated.Spec.NodePool.Replicas; got == nil || *got != updatedReplicas {
		t.Errorf("replicas = %v, want %d", got, updatedReplicas)
	}
	if !updated.Spec.NodePool.Management.AutoRepair {
		t.Error("round-trip update changed service-managed management.autoRepair")
	}
}

func TestNodePoolHandler_Update_ReplacesRootTags(t *testing.T) {
	cluster := testClusterCR("550e8400-e29b-41d4-a716-446655440000", "test-cluster", testAccountID)
	cluster.Spec.Tags = map[string]string{"environment": "test"}
	nodePool := testNodePoolCR("test-cluster.workers", cluster)
	nodePool.Spec.Tags = map[string]string{"owner": "old", "remove": "this"}
	nodePool.Spec.NodePool = seaplanev1alpha1.NodePoolSpecPassthrough{
		ClusterName: cluster.Name,
		Platform: hypershiftv1beta1.NodePoolPlatform{
			Type: hypershiftv1beta1.AWSPlatform,
			AWS: &hypershiftv1beta1.AWSNodePoolPlatform{
				InstanceType: "m5.xlarge",
			},
		},
	}
	handler := newTestNodePoolHandler(t, cluster, nodePool)
	request := seaplanedb.InternalToPublicNodePool(nodePool)
	request.Spec.Tags = map[string]string{"owner": "new"}

	req := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+nodePool.Name, bytes.NewReader(mustMarshal(t, request)))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": nodePool.Name})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response public.NodePool
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Spec.Tags) != 1 || response.Spec.Tags["owner"] != "new" {
		t.Errorf("response tags = %v, want only owner=new", response.Spec.Tags)
	}

	stored, err := handler.db.GetNodePool(testContext(testAccountID), nodePool.Name)
	if err != nil {
		t.Fatalf("get updated NodePool: %v", err)
	}
	if len(stored.Spec.Tags) != 1 || stored.Spec.Tags["owner"] != "new" {
		t.Errorf("stored tags = %v, want only owner=new", stored.Spec.Tags)
	}
}

func TestNodePoolHandler_List_ClusterIDParameterFiltersByParentUID(t *testing.T) {
	clusterUID := "550e8400-e29b-41d4-a716-446655440000"
	parent := testClusterCR(clusterUID, "my-cluster", testAccountID)
	other := testClusterCR("550e8400-e29b-41d4-a716-446655440001", "other-cluster", testAccountID)
	matching := testNodePoolCR("my-cluster.workers", parent)
	nonmatching := testNodePoolCR("other-cluster.workers", other)
	handler := newTestNodePoolHandler(t, parent, other, matching, nonmatching)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/nodepools?clusterId="+clusterUID, nil)
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.List(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Total != 1 || len(response.Items) != 1 {
		t.Fatalf("expected only the matching NodePool, got %+v", response)
	}
}

func TestNodePoolHandler_List_InvalidSelector(t *testing.T) {
	const clusterUID = "550e8400-e29b-41d4-a716-446655440000"
	tests := []struct {
		name        string
		query       string
		wantErrCode string
	}{
		{
			name:        "invalid cluster UID",
			query:       "clusterUID=not-a-uuid",
			wantErrCode: ErrNodePoolListInvalidClusterUID.Code,
		},
		{
			name:        "malformed label selector",
			query:       "labelSelector=%3D",
			wantErrCode: ErrNodePoolListInvalidLabelSelector.Code,
		},
		{
			name:        "unsupported label selector key",
			query:       "labelSelector=other%3Dvalue",
			wantErrCode: ErrNodePoolListInvalidLabelSelector.Code,
		},
		{
			name:        "unsupported label selector operator",
			query:       "labelSelector=seaplane.io%2Fcluster-uid!%3D" + clusterUID,
			wantErrCode: ErrNodePoolListInvalidLabelSelector.Code,
		},
		{
			name:        "label selector conflicts with cluster UID",
			query:       "clusterUID=" + clusterUID + "&labelSelector=seaplane.io%2Fcluster-uid%3D550e8400-e29b-41d4-a716-446655440001",
			wantErrCode: ErrNodePoolListInvalidLabelSelector.Code,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newTestNodePoolHandler(t)
			registry := testAuthzMetrics(t)
			req := httptest.NewRequest(http.MethodGet, "/api/v0/nodepools?"+tt.query, nil)
			req = req.WithContext(testContext(testAccountID))
			w := httptest.NewRecorder()

			handler.List(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if message := decodeErrorMessage(t, w); !strings.Contains(message, tt.wantErrCode) {
				t.Errorf("error message = %q, want code %s", message, tt.wantErrCode)
			}
			assertNoAuthzMetrics(t, registry)
		})
	}
}

// Service-set fields cannot change on update; clearing one is a change too.
// rosa edit machinepool echoes the stored values from Get.
func TestNodePoolHandler_Update_ProtectsServiceSet(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"spec":{"accountId":"999999999999","labels":{"changed":"true"}}}`, http.StatusUnprocessableEntity},
		{`{"spec":{"AccountId":"","labels":{"changed":"true"}}}`, http.StatusUnprocessableEntity},
		{`{"spec":{"nodePool":{"replicas":3,"management":{"upgradeType":"","autoRepair":false}}}}`, http.StatusUnprocessableEntity},
		{`{"spec":{"nodePool":{"replicas":3,"management":{"upgradeType":"Replace","autoRepair":true}}}}`, http.StatusOK},
		{`{"spec":{"nodePool":{"management":{"upgradeType":"InPlace","autoRepair":true}}}}`, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.body, func(t *testing.T) {
			parent := testClusterCR("parent-uid", "owned", testAccountID)
			np := testNodePoolCR("owned.workers", parent)
			replicas := int32(2)
			np.Spec.NodePool.Replicas = &replicas
			np.Spec.NodePool.Management = hypershiftv1beta1.NodePoolManagement{UpgradeType: hypershiftv1beta1.UpgradeTypeReplace, AutoRepair: true}
			fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent, np).Build()
			logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
			handler := NewNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, lifecyclePermit), logger)
			assertHandlerStatus(t, tc.want, http.MethodPut,
				"/api/v0/nodepools/"+np.Name, tc.body,
				map[string]string{"id": np.Name}, testAccountID, handler.Update)
			var stored seaplanev1alpha1.NodePool
			if err := fc.Get(context.Background(), client.ObjectKeyFromObject(np), &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Spec.AccountID != testAccountID || stored.Spec.NodePool.Management != np.Spec.NodePool.Management {
				t.Errorf("service-set values changed: accountId=%q management=%+v", stored.Spec.AccountID, stored.Spec.NodePool.Management)
			}
			if tc.want == http.StatusOK && strings.Contains(tc.body, `"replicas":3`) && *stored.Spec.NodePool.Replicas != 3 {
				t.Errorf("replicas = %d, want 3", *stored.Spec.NodePool.Replicas)
			}
		})
	}
}

func TestNodePoolHandler_Get_Success(t *testing.T) {
	parent := testClusterCR("parent-uid", "my-cluster", testAccountID)
	nodepool := testNodePoolCR("my-cluster.workers", parent)
	handler := newTestNodePoolHandler(t, parent, nodepool)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/nodepools/"+nodepool.Name, nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": nodepool.Name})
	w := httptest.NewRecorder()

	handler.Get(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	metadata, _ := response["metadata"].(map[string]any)
	if got := metadata["name"]; got != nodepool.Name {
		t.Errorf("metadata.name = %v, want %q", got, nodepool.Name)
	}
}

func TestNodePoolHandler_Get_NotFound(t *testing.T) {
	handler := newTestNodePoolHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/nodepools/missing.workers", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "missing.workers"})
	w := httptest.NewRecorder()

	handler.Get(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNodePoolHandler_Delete_Success(t *testing.T) {
	parent := testClusterCR("parent-uid", "my-cluster", testAccountID)
	nodepool := testNodePoolCR("my-cluster.workers", parent)
	handler := newTestNodePoolHandler(t, parent, nodepool)
	req := httptest.NewRequest(http.MethodDelete, "/api/v0/nodepools/"+nodepool.Name, nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": nodepool.Name})
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response["nodepool_name"]; got != nodepool.Name {
		t.Errorf("nodepool_name = %v, want %q", got, nodepool.Name)
	}
}

func TestNodePoolHandler_Delete_NotFound(t *testing.T) {
	handler := newTestNodePoolHandler(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v0/nodepools/missing.workers", nil)
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": "missing.workers"})
	w := httptest.NewRecorder()

	handler.Delete(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNodePoolHandler_Update_NamespaceMismatch(t *testing.T) {
	parent := testClusterCR("parent-uid", "test-cluster", testAccountID)
	nodePool := testNodePoolCR("test-cluster.workers", parent)
	handler := newTestNodePoolHandler(t, parent, nodePool)
	body := mustMarshal(t, map[string]any{
		"metadata": map[string]any{"namespace": "account-999999999999"},
		"spec":     map[string]any{"nodePool": map[string]any{"replicas": 3}},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v0/nodepools/"+nodePool.Name, bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	req = mux.SetURLVars(req, map[string]string{"id": nodePool.Name})
	w := httptest.NewRecorder()

	handler.Update(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if message := decodeErrorMessage(t, w); !strings.Contains(message, ErrNodePoolUpdateInvalidNamespace.Code) {
		t.Errorf("error message = %q, want code %s", message, ErrNodePoolUpdateInvalidNamespace.Code)
	}
}

// TestNodePoolHandler_Create_MissingMetadata verifies that POST /nodepools with
// missing or malformed metadata fields is rejected before any DB lookup.
func TestNodePoolHandler_Create_MissingMetadata(t *testing.T) {
	handler := newTestNodePoolHandler(t) // no pre-seeded objects needed

	cases := []struct {
		name         string
		body         map[string]any
		wantStatus   int
		wantCodeFrag string // substring of the platform error code in message
	}{
		{
			name:         "missing name",
			body:         map[string]any{"metadata": map[string]any{"namespace": "account-" + testAccountID}},
			wantStatus:   http.StatusBadRequest,
			wantCodeFrag: ErrNodePoolCreateMissingFields.Code,
		},
		{
			name:         "name is not dotted",
			body:         map[string]any{"metadata": map[string]any{"name": "my-pool", "namespace": "account-" + testAccountID}},
			wantStatus:   http.StatusBadRequest,
			wantCodeFrag: ErrNodePoolCreateInvalidName.Code,
		},
		{
			name:         "name has too many components",
			body:         map[string]any{"metadata": map[string]any{"name": "cluster.pool.extra", "namespace": "account-" + testAccountID}},
			wantStatus:   http.StatusBadRequest,
			wantCodeFrag: ErrNodePoolCreateInvalidName.Code,
		},
		{
			name:         "namespace belongs to another account",
			body:         map[string]any{"metadata": map[string]any{"name": "my-cluster.workers", "namespace": "account-999999999999"}},
			wantStatus:   http.StatusBadRequest,
			wantCodeFrag: ErrNodePoolCreateInvalidNamespace.Code,
		},
		{
			name:         "legacy cluster namespace rejected",
			body:         map[string]any{"metadata": map[string]any{"name": "my-cluster.workers", "namespace": "cluster-550e8400-e29b-41d4-a716-446655440000"}},
			wantStatus:   http.StatusBadRequest,
			wantCodeFrag: ErrNodePoolCreateInvalidNamespace.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", bytes.NewReader(mustMarshal(t, tc.body)))
			req = req.WithContext(testContext(testAccountID))

			w := httptest.NewRecorder()
			handler.Create(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("expected %d, got %d body=%s", tc.wantStatus, w.Code, w.Body.String())
			}
			var errResp map[string]any
			if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
				t.Fatalf("response body is not valid JSON: %v", err)
			}
			msg, _ := errResp["message"].(string)
			if msg == "" {
				t.Errorf("expected non-empty error message")
			}
			if tc.wantCodeFrag != "" && !containsCode(msg, tc.wantCodeFrag) {
				t.Errorf("message %q does not contain code %s", msg, tc.wantCodeFrag)
			}
		})
	}
}

func TestNodePoolHandler_Create_ResolvesParentFromDottedName(t *testing.T) {
	scheme := newTestScheme()
	cluster := testClusterCR("550e8400-e29b-41d4-a716-446655440000", "my-cluster", testAccountID)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, lifecyclePermit), logger)
	body := mustMarshal(t, map[string]any{
		"metadata": map[string]any{"name": "my-cluster.workers"},
		"spec": map[string]any{"nodePool": map[string]any{
			"clusterName": "client-supplied-parent",
			"release":     map[string]any{"image": "quay.io/openshift-release-dev/ocp-release:4.17.0"},
			"platform":    map[string]any{"type": "AWS"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var stored seaplanev1alpha1.NodePool
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "account-" + testAccountID, Name: "my-cluster.workers"}, &stored); err != nil {
		t.Fatalf("get created NodePool: %v", err)
	}
	if got := stored.Spec.NodePool.ClusterName; got != "my-cluster" {
		t.Errorf("stored parent ClusterName = %q, want %q", got, "my-cluster")
	}
	if got := stored.Spec.EC2InstanceMetadataHttpTokens; got != seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired {
		t.Errorf("stored ec2InstanceMetadataHttpTokens = %q, want %q", got, seaplanev1alpha1.EC2InstanceMetadataHttpTokensRequired)
	}
	if got := stored.Labels[clusterUIDLabel]; got != string(cluster.UID) {
		t.Errorf("cluster UID label = %q, want %q", got, cluster.UID)
	}
	owner := metav1.GetControllerOf(&stored)
	if owner == nil || owner.Name != cluster.Name || owner.UID != cluster.UID {
		t.Errorf("controller ownerReference = %+v, want Cluster %s UID %s", owner, cluster.Name, cluster.UID)
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if spec, ok := response["spec"].(map[string]any); ok {
		if _, exists := spec["clusterId"]; exists {
			t.Error("response must not expose spec.clusterId")
		}
		if got := spec["ec2InstanceMetadataHttpTokens"]; got != "required" {
			t.Errorf("response ec2InstanceMetadataHttpTokens = %v, want required", got)
		}
	}
}

func TestNodePoolHandler_Create_AcceptsOptionalEC2InstanceMetadataHttpTokens(t *testing.T) {
	scheme := newTestScheme()
	cluster := &seaplanev1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Name:      "my-cluster",
		Namespace: "account-" + testAccountID,
		UID:       types.UID("550e8400-e29b-41d4-a716-446655440000"),
		Labels:    map[string]string{seaplanev1alpha1.AccountIDLabel: testAccountID},
	}}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, lifecyclePermit), logger)
	body := mustMarshal(t, map[string]any{
		"metadata": map[string]any{"name": "my-cluster.optional-workers"},
		"spec": map[string]any{
			"EC2InstanceMetadataHttpTokens": "optional",
			"nodePool": map[string]any{
				"clusterName": "my-cluster",
				"release":     map[string]any{"image": "quay.io/openshift-release-dev/ocp-release:4.17.0"},
				"platform":    map[string]any{"type": "AWS"},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v0/nodepools", bytes.NewReader(body))
	req = req.WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var stored seaplanev1alpha1.NodePool
	if err := fc.Get(context.Background(), types.NamespacedName{Namespace: "account-" + testAccountID, Name: "my-cluster.optional-workers"}, &stored); err != nil {
		t.Fatalf("get created NodePool: %v", err)
	}
	if got := stored.Spec.EC2InstanceMetadataHttpTokens; got != seaplanev1alpha1.EC2InstanceMetadataHttpTokensOptional {
		t.Errorf("stored ec2InstanceMetadataHttpTokens = %q, want %q", got, seaplanev1alpha1.EC2InstanceMetadataHttpTokensOptional)
	}
}

// containsCode reports whether the metav1.Status message contains the given code prefix.
func containsCode(message, code string) bool {
	return len(message) >= len(code) && message[:len(code)] == code
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}
