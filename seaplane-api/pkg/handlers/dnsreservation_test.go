package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

const dnsReservationTestAccountID = "123456789012"
const dnsReservationTestAccountLabel = seaplanev1alpha1.AccountIDLabel

func newDNSReservationTestHandler(t *testing.T, objects ...*seaplanev1alpha1.DNSReservation) (*DNSReservationHandler, *seaplanedb.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := seaplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Seaplane scheme: %v", err)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(toRuntimeObjects(objects)...).Build()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := seaplanedb.NewClientFrom(fc, logger)
	return NewDNSReservationHandler(db, clusterAuthorizer(t, lifecyclePermit), logger), db
}

func toRuntimeObjects(objects []*seaplanev1alpha1.DNSReservation) []client.Object {
	result := make([]client.Object, len(objects))
	for i := range objects {
		result[i] = objects[i]
	}
	return result
}

func TestDNSReservationHandlerCreateNameOnly(t *testing.T) {
	handler, db := newDNSReservationTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v0/dns_reservations", bytes.NewBufferString(`{"metadata":{"name":"zone-a"}}`))
	req = req.WithContext(testContext(dnsReservationTestAccountID))
	w := httptest.NewRecorder()

	handler.Create(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var response public.DNSReservation
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Name != "zone-a" {
		t.Errorf("metadata.name = %q, want zone-a", response.Name)
	}
	if response.Namespace != "account-"+dnsReservationTestAccountID {
		t.Errorf("metadata.namespace = %q", response.Namespace)
	}
	if response.Status.Phase != public.DNSReservationPhasePending {
		t.Errorf("status.phase = %q, want Pending", response.Status.Phase)
	}
	stored, err := db.GetDNSReservation(req.Context(), "zone-a")
	if err != nil {
		t.Fatalf("get stored reservation: %v", err)
	}
	if stored.Labels[dnsReservationTestAccountLabel] != dnsReservationTestAccountID {
		t.Errorf("trusted account label = %q", stored.Labels[dnsReservationTestAccountLabel])
	}
}

func TestDNSReservationHandlerGetAndListAreAccountScoped(t *testing.T) {
	reservation := &seaplanev1alpha1.DNSReservation{ObjectMeta: metav1.ObjectMeta{
		Name:      "zone-a",
		Namespace: "account-" + dnsReservationTestAccountID,
		UID:       types.UID("dns-uid-a"),
		Labels:    map[string]string{dnsReservationTestAccountLabel: dnsReservationTestAccountID},
	}, Status: seaplanev1alpha1.DNSReservationStatus{
		Phase:      seaplanev1alpha1.DNSReservationPhaseReady,
		BaseDomain: "a1b2.0.example.com",
	}}
	other := reservation.DeepCopy()
	other.Namespace = "account-999999999999"
	other.UID = "dns-uid-other"
	handler, _ := newDNSReservationTestHandler(t, reservation, other)

	getReq := httptest.NewRequest(http.MethodGet, "/api/v0/dns_reservations/zone-a", nil)
	getReq = getReq.WithContext(testContext(dnsReservationTestAccountID))
	getReq = mux.SetURLVars(getReq, map[string]string{"id": "zone-a"})
	getResponse := httptest.NewRecorder()
	handler.Get(getResponse, getReq)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("expected Get 200, got %d: %s", getResponse.Code, getResponse.Body.String())
	}
	var got public.DNSReservation
	if err := json.Unmarshal(getResponse.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode Get response: %v", err)
	}
	if got.Status.BaseDomain != "a1b2.0.example.com" {
		t.Errorf("status.baseDomain = %q", got.Status.BaseDomain)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v0/dns_reservations", nil)
	listReq = listReq.WithContext(testContext(dnsReservationTestAccountID))
	listResponse := httptest.NewRecorder()
	handler.List(listResponse, listReq)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("expected List 200, got %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var list struct {
		Items []*public.DNSReservation `json:"items"`
		Total int                      `json:"total"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode List response: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Namespace != "account-"+dnsReservationTestAccountID {
		t.Errorf("account-scoped list = %+v", list)
	}
}

func TestDNSReservationHandlerDeleteClaimedConflictAndUnclaimedSuccess(t *testing.T) {
	claimed := &seaplanev1alpha1.DNSReservation{ObjectMeta: metav1.ObjectMeta{
		Name:      "claimed-zone",
		Namespace: "account-" + dnsReservationTestAccountID,
		UID:       types.UID("dns-uid-claimed"),
		Labels: map[string]string{
			dnsReservationTestAccountLabel: dnsReservationTestAccountID,
			claimedByClusterUIDLabel:       "cluster-uid-a",
		},
	}}
	unclaimed := &seaplanev1alpha1.DNSReservation{ObjectMeta: metav1.ObjectMeta{
		Name:      "free-zone",
		Namespace: "account-" + dnsReservationTestAccountID,
		UID:       types.UID("dns-uid-free"),
		Labels:    map[string]string{dnsReservationTestAccountLabel: dnsReservationTestAccountID},
	}}
	handler, db := newDNSReservationTestHandler(t, claimed, unclaimed)

	claimedReq := httptest.NewRequest(http.MethodDelete, "/api/v0/dns_reservations/claimed-zone", nil)
	claimedReq = claimedReq.WithContext(testContext(dnsReservationTestAccountID))
	claimedReq = mux.SetURLVars(claimedReq, map[string]string{"id": claimed.Name})
	claimedResponse := httptest.NewRecorder()
	handler.Delete(claimedResponse, claimedReq)
	if claimedResponse.Code != http.StatusConflict {
		t.Fatalf("expected claimed delete 409, got %d: %s", claimedResponse.Code, claimedResponse.Body.String())
	}

	freeReq := httptest.NewRequest(http.MethodDelete, "/api/v0/dns_reservations/free-zone", nil)
	freeReq = freeReq.WithContext(testContext(dnsReservationTestAccountID))
	freeReq = mux.SetURLVars(freeReq, map[string]string{"id": unclaimed.Name})
	freeResponse := httptest.NewRecorder()
	handler.Delete(freeResponse, freeReq)
	if freeResponse.Code != http.StatusAccepted {
		t.Fatalf("expected unclaimed delete 202, got %d: %s", freeResponse.Code, freeResponse.Body.String())
	}
	if _, err := db.GetDNSReservation(testContext(testAccountID), unclaimed.Name); !seaplanedb.IsNotFound(err) {
		t.Errorf("unclaimed reservation should be deleted, got err=%v", err)
	}
}
