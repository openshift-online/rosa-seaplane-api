package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
)

const dnsTestAccountID = "123456789012"

func clusterDNSRequestContext() context.Context {
	ctx := context.WithValue(context.Background(), middleware.ContextKeyAccountID, dnsTestAccountID)
	return context.WithValue(ctx, middleware.ContextKeyCallerARN, "arn:aws:iam::"+dnsTestAccountID+":user/test")
}

func clusterDNSRequestBody(dns map[string]any, reservationID string) []byte {
	spec := map[string]any{
		"hostedCluster": map[string]any{
			"release":    map[string]any{"image": testReleaseImage},
			"networking": map[string]any{},
			"platform":   map[string]any{"type": "AWS"},
			"dns":        dns,
		},
	}
	if reservationID != "" {
		spec["dnsReservationId"] = reservationID
	}
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "dns-test-cluster"},
		"spec":     spec,
	})
	return body
}

func newClusterDNSFakeClient(t *testing.T, objects []runtime.Object, funcs interceptor.Funcs) (*seaplanedb.Client, client.Client) {
	t.Helper()
	if funcs.Create == nil {
		funcs.Create = func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if cluster, ok := obj.(*seaplanev1alpha1.Cluster); ok && cluster.UID == "" {
				cluster.UID = types.UID("cluster-uid")
			}
			return underlying.Create(ctx, obj, opts...)
		}
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithIndex(&seaplanev1alpha1.DNSReservation{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithRuntimeObjects(objects...).
		WithStatusSubresource(&seaplanev1alpha1.DNSReservation{}).
		WithInterceptorFuncs(funcs).
		Build()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return seaplanedb.NewClientFrom(fakeClient, logger), fakeClient
}

func clusterDNSReservation() *seaplanev1alpha1.DNSReservation {
	reservation := &seaplanev1alpha1.DNSReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "0-abc12345",
			Namespace: "account-" + dnsTestAccountID,
			UID:       types.UID("550e8400-e29b-41d4-a716-446655440000"),
		},
	}
	reservation.Status.Phase = seaplanev1alpha1.DNSReservationPhaseReady
	reservation.Status.BaseDomain = "abc12345.0.example.com"
	reservation.Status.InternalHostedClusterName = seaplanev1alpha1.HostedClusterNameFromDNSReservation(reservation)
	return reservation
}

func newClusterDNSHandler(t *testing.T, objects []runtime.Object, funcs interceptor.Funcs) (*ClusterHandler, client.Client) {
	t.Helper()
	db, fakeClient := newClusterDNSFakeClient(t, objects, funcs)
	handler := NewClusterHandler(db, "https://oidc.example.com", 0, clusterAuthorizer(t, lifecyclePermit), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return handler, fakeClient
}

func TestClusterHandlerCreateRejectsCustomerBaseDomain(t *testing.T) {
	for _, baseDomain := range []string{"customer.example.net", "custom.0.example.com"} {
		t.Run(baseDomain, func(t *testing.T) {
			handler, _ := newClusterDNSHandler(t, nil, interceptor.Funcs{})
			request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(clusterDNSRequestBody(map[string]any{"baseDomain": baseDomain}, "")))).WithContext(clusterDNSRequestContext())
			response := httptest.NewRecorder()
			handler.Create(response, request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
		})
	}
}

func TestClusterHandlerCreateAcceptsBaseDomainPrefix(t *testing.T) {
	handler, fakeClient := newClusterDNSHandler(t, nil, interceptor.Funcs{})
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(clusterDNSRequestBody(map[string]any{"baseDomainPrefix": "ingress"}, "")))).WithContext(clusterDNSRequestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}

	var storedCluster seaplanev1alpha1.Cluster
	clusterKey := client.ObjectKey{Namespace: "account-" + dnsTestAccountID, Name: "dns-test-cluster"}
	if err := fakeClient.Get(context.Background(), clusterKey, &storedCluster); err != nil {
		t.Fatalf("get created cluster: %v", err)
	}
	if storedCluster.Spec.HostedCluster.DNS.BaseDomain != "" {
		t.Fatalf("cluster base domain = %q, want empty", storedCluster.Spec.HostedCluster.DNS.BaseDomain)
	}
	if prefix := storedCluster.Spec.HostedCluster.DNS.BaseDomainPrefix; prefix == nil || *prefix != "ingress" {
		t.Fatalf("cluster base domain prefix = %v, want ingress", prefix)
	}
}

func TestClusterHandlerCreateLeavesGeneratedDNSUnclaimed(t *testing.T) {
	handler, _ := newClusterDNSHandler(t, nil, interceptor.Funcs{})
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(clusterDNSRequestBody(map[string]any{}, "")))).WithContext(clusterDNSRequestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
}

func TestClusterHandlerCreateRejectsCustomerZoneIDs(t *testing.T) {
	handler, _ := newClusterDNSHandler(t, nil, interceptor.Funcs{})
	body := clusterDNSRequestBody(map[string]any{
		"publicZoneID":  "Z0123456789PUBLIC",
		"privateZoneID": "Z0123456789PRIVATE",
	}, "")
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(body))).WithContext(clusterDNSRequestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
}

func TestClusterHandlerCreateClaimsDNSReservationByID(t *testing.T) {
	reservation := clusterDNSReservation()
	handler, fakeClient := newClusterDNSHandler(t, []runtime.Object{reservation}, interceptor.Funcs{})

	body := clusterDNSRequestBody(map[string]any{}, string(reservation.UID))
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(body))).WithContext(clusterDNSRequestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}

	var storedReservation seaplanev1alpha1.DNSReservation
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(reservation), &storedReservation); err != nil {
		t.Fatalf("get DNS reservation: %v", err)
	}
	if owner := storedReservation.Labels[claimedByClusterUIDLabel]; owner != "cluster-uid" {
		t.Fatalf("DNS reservation owner = %q, want cluster-uid", owner)
	}

	var storedCluster seaplanev1alpha1.Cluster
	clusterKey := client.ObjectKey{Namespace: "account-" + dnsTestAccountID, Name: "dns-test-cluster"}
	if err := fakeClient.Get(context.Background(), clusterKey, &storedCluster); err != nil {
		t.Fatalf("get created cluster: %v", err)
	}
	if storedCluster.Spec.DNSReservationID != string(reservation.UID) {
		t.Fatalf("cluster DNSReservationID = %q, want %q", storedCluster.Spec.DNSReservationID, reservation.UID)
	}
}

func TestClusterHandlerCreateFailureLeavesDNSReservationUnclaimed(t *testing.T) {
	reservation := clusterDNSReservation()
	funcs := interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*seaplanev1alpha1.Cluster); ok {
			return errors.New("cluster create failed")
		}
		return underlying.Create(ctx, obj, opts...)
	}}
	handler, fakeClient := newClusterDNSHandler(t, []runtime.Object{reservation}, funcs)
	body := clusterDNSRequestBody(map[string]any{}, string(reservation.UID))
	request := httptest.NewRequest(http.MethodPost, "/api/v0/clusters", strings.NewReader(string(body))).WithContext(clusterDNSRequestContext())
	response := httptest.NewRecorder()
	handler.Create(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("Create status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}

	var storedReservation seaplanev1alpha1.DNSReservation
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(reservation), &storedReservation); err != nil {
		t.Fatalf("get DNS reservation: %v", err)
	}
	if owner := storedReservation.Labels[claimedByClusterUIDLabel]; owner != "" {
		t.Fatalf("DNS reservation was claimed before Cluster creation succeeded: owner %q", owner)
	}
}
