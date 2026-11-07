package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gorilla/mux"
	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

func TestNewRenderedNodePoolHandlerRequiresAuthorizer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewRenderedNodePoolHandler did not panic with a nil authorizer")
		}
	}()
	NewRenderedNodePoolHandler(nil, nil, slog.Default())
}

func TestRenderedNodePoolHandler_ListAndGet(t *testing.T) {
	const clusterID = "550e8400-e29b-41d4-a716-446655440000"
	cluster := testClusterCR(clusterID, "rendered-cluster", testAccountID)
	nodePool := testNodePoolCR("rendered-cluster.workers", cluster)
	rendered := &hypershiftv1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name: nodePool.Name, Namespace: nodePool.Namespace,
		Labels: map[string]string{"seaplane.io/cluster-id": clusterID, "seaplane.io/nodepool-id": "rendered-nodepool-uid"},
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(cluster, nodePool, rendered).Build()
	handler := NewRenderedNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, `permit(principal, action in Seaplane::Action::"AllActions", resource);`), logger)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools", nil).WithContext(testContext(testAccountID))
	listResponse := httptest.NewRecorder()
	handler.List(listResponse, listReq)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("rendered list status = %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var list map[string]any
	if err := json.NewDecoder(listResponse.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if int(list["total"].(float64)) != 1 || len(list["items"].([]any)) != 1 {
		t.Fatalf("rendered list = %#v, want one account-scoped item", list)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/rendered-cluster.workers", nil).WithContext(testContext(testAccountID))
	getReq = mux.SetURLVars(getReq, map[string]string{"id": nodePool.Name})
	getResponse := httptest.NewRecorder()
	handler.Get(getResponse, getReq)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("rendered get status = %d: %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestRenderedNodePoolHandler_GetMissingSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewRenderedNodePoolHandler(
		seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(renderedTestScheme()).Build(), logger),
		clusterAuthorizer(t, clusterAdminPermit), logger,
	)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/missing", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "missing"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing rendered NodePool status = %d, want 404", response.Code)
	}
}

func TestRenderedNodePoolHandler_GetDenied(t *testing.T) {
	const clusterID = "550e8400-e29b-41d4-a716-446655440000"
	cluster := testClusterCR(clusterID, "rendered-cluster", testAccountID)
	nodePool := testNodePoolCR("rendered-cluster.workers", cluster)
	rendered := &hypershiftv1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name:      nodePool.Name,
		Namespace: nodePool.Namespace,
		Labels:    map[string]string{"seaplane.io/cluster-id": clusterID, "seaplane.io/nodepool-id": "rendered-nodepool-uid"},
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(cluster, nodePool, rendered).Build()
	handler := NewRenderedNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/"+nodePool.Name, nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": nodePool.Name},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied rendered NodePool status = %d, want 403", response.Code)
	}
}

func TestRenderedNodePoolHandler_GetMissingSourceNodePool(t *testing.T) {
	rendered := &hypershiftv1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name: "rendered-cluster.workers", Namespace: "account-" + testAccountID,
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(rendered).Build()
	handler := NewRenderedNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterAdminPermit), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/rendered-cluster.workers", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "rendered-cluster.workers"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing source NodePool status = %d, want 404", response.Code)
	}
}

func TestRenderedNodePoolHandler_GetSourceBackendError(t *testing.T) {
	rendered := &hypershiftv1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{
		Name: "rendered-cluster.workers", Namespace: "account-" + testAccountID,
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	getCalls := 0
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(rendered).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			getCalls++
			if getCalls == 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			return errors.New("source backend unavailable")
		},
	}).Build()
	handler := NewRenderedNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterAdminPermit), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/rendered-cluster.workers", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "rendered-cluster.workers"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("source backend error status = %d, want 500", response.Code)
	}
}

func TestRenderedNodePoolHandler_ListDenied(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewRenderedNodePoolHandler(
		seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(renderedTestScheme()).Build(), logger),
		clusterAuthorizer(t), logger,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools", nil).WithContext(testContext(testAccountID))
	response := httptest.NewRecorder()
	handler.List(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied rendered NodePool list status = %d, want 403", response.Code)
	}
}

func TestRenderedNodePoolHandler_BackendErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("database unavailable")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("database unavailable")
		},
	}).Build()
	handler := NewRenderedNodePoolHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterAdminPermit), logger)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools", nil).WithContext(testContext(testAccountID))
	listResponse := httptest.NewRecorder()
	handler.List(listResponse, listReq)
	if listResponse.Code != http.StatusInternalServerError {
		t.Fatalf("rendered NodePool list error status = %d, want 500", listResponse.Code)
	}

	getReq := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderednodepools/my-cluster.workers", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "my-cluster.workers"},
	)
	getResponse := httptest.NewRecorder()
	handler.Get(getResponse, getReq)
	if getResponse.Code != http.StatusInternalServerError {
		t.Fatalf("rendered NodePool get error status = %d, want 500", getResponse.Code)
	}
}
