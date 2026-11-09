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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

func renderedTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = seaplanev1alpha1.AddToScheme(scheme)
	_ = hypershiftv1beta1.AddToScheme(scheme)
	return scheme
}

func TestNewRenderedHostedClusterHandlerRequiresAuthorizer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewRenderedHostedClusterHandler did not panic with a nil authorizer")
		}
	}()
	NewRenderedHostedClusterHandler(nil, nil, slog.Default())
}

func TestRenderedHostedClusterHandler_ListAndGet(t *testing.T) {
	scheme := renderedTestScheme()
	cluster := testClusterCR("rendered-cluster-uid", "rendered-cluster", testAccountID)
	rendered := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "rendered-cluster", Namespace: cluster.Namespace,
		Labels: map[string]string{"seaplane.io/cluster-id": string(cluster.UID)},
	}}
	foreign := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: "account-999999999999",
	}}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, rendered, foreign).Build()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterReadPermit), logger)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters", nil).WithContext(testContext(testAccountID))
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

	getReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/rendered-cluster", nil).WithContext(testContext(testAccountID))
	getReq = mux.SetURLVars(getReq, map[string]string{"id": "rendered-cluster"})
	getResponse := httptest.NewRecorder()
	handler.Get(getResponse, getReq)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("rendered get status = %d: %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestRenderedHostedClusterHandler_GetMissingSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewRenderedHostedClusterHandler(
		seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(renderedTestScheme()).Build(), logger),
		clusterAuthorizer(t, clusterReadPermit), logger,
	)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/missing", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "missing"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing rendered HostedCluster status = %d, want 404", response.Code)
	}
}

func TestRenderedHostedClusterHandler_GetDenied(t *testing.T) {
	cluster := testClusterCR("rendered-cluster-uid", "rendered-cluster", testAccountID)
	rendered := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name:      cluster.Name,
		Namespace: cluster.Namespace,
		Labels:    map[string]string{"seaplane.io/cluster-id": string(cluster.UID)},
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(cluster, rendered).Build()
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/"+cluster.Name, nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": cluster.Name},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied rendered HostedCluster status = %d, want 403", response.Code)
	}
}

func TestRenderedHostedClusterHandler_GetMissingSourceCluster(t *testing.T) {
	rendered := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "rendered-cluster", Namespace: "account-" + testAccountID,
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(rendered).Build()
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterReadPermit), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/rendered-cluster", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "rendered-cluster"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing source Cluster status = %d, want 404", response.Code)
	}
}

func TestRenderedHostedClusterHandler_GetSourceBackendError(t *testing.T) {
	rendered := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "rendered-cluster", Namespace: "account-" + testAccountID,
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
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterReadPermit), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/rendered-cluster", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "rendered-cluster"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("source backend error status = %d, want 500", response.Code)
	}
}

func TestRenderedHostedClusterHandler_ListDenied(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	handler := NewRenderedHostedClusterHandler(
		seaplanedb.NewClientFrom(fake.NewClientBuilder().WithScheme(renderedTestScheme()).Build(), logger),
		clusterAuthorizer(t), logger,
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters", nil).WithContext(testContext(testAccountID))
	response := httptest.NewRecorder()
	handler.List(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("denied rendered HostedCluster list status = %d, want 403", response.Code)
	}
}

func TestRenderedHostedClusterHandler_BackendErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("database unavailable")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("database unavailable")
		},
	}).Build()
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterReadPermit), logger)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters", nil).WithContext(testContext(testAccountID))
	listResponse := httptest.NewRecorder()
	handler.List(listResponse, listReq)
	if listResponse.Code != http.StatusInternalServerError {
		t.Fatalf("rendered HostedCluster list error status = %d, want 500", listResponse.Code)
	}

	getReq := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/my-cluster", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "my-cluster"},
	)
	getResponse := httptest.NewRecorder()
	handler.Get(getResponse, getReq)
	if getResponse.Code != http.StatusInternalServerError {
		t.Fatalf("rendered HostedCluster get error status = %d, want 500", getResponse.Code)
	}
}

func TestRenderedHostedClusterHandler_GetIsAccountScoped(t *testing.T) {
	rendered := &hypershiftv1beta1.HostedCluster{ObjectMeta: metav1.ObjectMeta{
		Name:      "shared-name",
		Namespace: "account-999999999999",
		Labels:    map[string]string{"seaplane.io/cluster-id": "foreign-cluster-uid"},
	}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	fc := fake.NewClientBuilder().WithScheme(renderedTestScheme()).WithObjects(rendered).Build()
	handler := NewRenderedHostedClusterHandler(seaplanedb.NewClientFrom(fc, logger), clusterAuthorizer(t, clusterReadPermit), logger)
	req := mux.SetURLVars(
		httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters/shared-name", nil).WithContext(testContext(testAccountID)),
		map[string]string{"id": "shared-name"},
	)
	response := httptest.NewRecorder()
	handler.Get(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-account rendered HostedCluster status = %d, want 404", response.Code)
	}
}

func TestWriteRenderedListSortsBeforePagination(t *testing.T) {
	items := []struct {
		Name string `json:"name"`
	}{
		{Name: "cluster-c"}, {Name: "cluster-a"}, {Name: "cluster-b"},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters?limit=2", nil)
	response := httptest.NewRecorder()
	writeRenderedList(response, request, items, func(item struct {
		Name string `json:"name"`
	}) string {
		return item.Name
	}, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("rendered list status = %d, want 200", response.Code)
	}
	var body struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 || body.Items[0].Name != "cluster-a" || body.Items[1].Name != "cluster-b" {
		t.Fatalf("rendered page = %#v, want cluster-a, cluster-b", body.Items)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v0/renderedhostedclusters?limit=200&offset=10", nil)
	response = httptest.NewRecorder()
	writeRenderedList(response, request, items, func(item struct {
		Name string `json:"name"`
	}) string {
		return item.Name
	}, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	body = struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 0 {
		t.Fatalf("out-of-range rendered page = %#v, want empty", body.Items)
	}
}
