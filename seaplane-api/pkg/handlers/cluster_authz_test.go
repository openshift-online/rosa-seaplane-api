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
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
)

const (
	lifecyclePermit       = `permit(principal, action in Seaplane::Action::"AllActions", resource);`
	clusterAuthzRegion    = "us-east-1"
	clusterReadPermit     = `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`
	clusterAdminPermit    = `permit(principal, action in Seaplane::Action::"AllActions", resource);`
	clusterListPermit     = `permit(principal, action == Seaplane::Action::"ListClusters", resource);`
	clusterDescribePermit = `permit(principal, action == Seaplane::Action::"DescribeCluster", resource);`
	clusterLabelPermit    = `permit(principal, action == Seaplane::Action::"DescribeCluster", resource)
when { resource.hasTag("example.com/team") && resource.getTag("example.com/team") == "blue" };`
	clusterFailingForbid = `forbid(principal, action == Seaplane::Action::"DescribeCluster", resource)
when { resource.hasTag("example.com/fault") && resource.getTag("example.com/fault") == "fault-detail-secret" && 9223372036854775807 + 1 == 0 };`
)

func clusterAuthorizer(t *testing.T, policies ...string) *authz.Authorizer {
	t.Helper()
	bundle := map[string]any{"formatVersion": 1, "registeredAccounts": []string{testAccountID}, "policies": []any{}, "attachments": []any{}}
	for i, policy := range policies {
		id := fmt.Sprintf("grant-%d", i)
		bundle["policies"] = append(bundle["policies"].([]any), map[string]any{"id": id, "ownerAccountID": testAccountID, "content": policy})
		bundle["attachments"] = append(bundle["attachments"].([]any), map[string]any{"id": id, "policyID": id, "principalARN": "arn:aws:iam::" + testAccountID + ":user/test", "scope": "global"})
	}
	content, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authz.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := authz.LoadConfig(path, clusterAuthzRegion)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Retries retain the same policy/context snapshot while checking fresh resource entities.
func TestRetryPreparationFrozen(t *testing.T) {
	r := httptest.NewRequest(http.MethodPatch, "/", nil).WithContext(testContext(testAccountID))
	r, finish := withAuthzOutcome(r)
	defer finish()
	a := clusterAuthorizer(t, lifecyclePermit)
	first, err := prepareResourceAuthz(r, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareResourceAuthz(r, a)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("retry replaced the frozen authorization context")
	}
}

func clusterAuthzFixture(t *testing.T, fc client.Client, a *authz.Authorizer) (*ClusterHandler, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("region", clusterAuthzRegion)
	h := NewClusterHandler(seaplanedb.NewClientFrom(fc, logger), "", 0, a, logger)
	return h, testAuthzMetrics(t), &logs
}

// testAuthzMetrics points authz.DefaultMetrics at a fresh registry for one test.
func testAuthzMetrics(t *testing.T) *prometheus.Registry {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := authz.NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	original := authz.DefaultMetrics
	authz.DefaultMetrics = metrics
	t.Cleanup(func() { authz.DefaultMetrics = original })
	return registry
}

func assertNoAuthzMetrics(t *testing.T, registry *prometheus.Registry) {
	t.Helper()
	if families, err := registry.Gather(); err != nil || len(families) != 0 {
		t.Fatalf("want no authorization outcome, got %v (%v)", families, err)
	}
}

// The fake still applies real storage selectors. Only its output order is controlled.
func clusterOrderedStore(clusters []*seaplanev1alpha1.Cluster, beforeRead func(context.Context) error) client.WithWatch {
	objects := make([]client.Object, 0, len(clusters))
	for _, cr := range clusters {
		objects = append(objects, cr)
	}
	return fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if beforeRead != nil {
				if err := beforeRead(ctx); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, obj client.ObjectList, opts ...client.ListOption) error {
			if beforeRead != nil {
				if err := beforeRead(ctx); err != nil {
					return err
				}
			}
			if err := c.List(ctx, obj, opts...); err != nil {
				return err
			}
			list := obj.(*seaplanev1alpha1.ClusterList)
			scoped := make(map[client.ObjectKey]seaplanev1alpha1.Cluster, len(list.Items))
			for _, cr := range list.Items {
				scoped[client.ObjectKeyFromObject(&cr)] = cr
			}
			list.Items = nil
			for _, cr := range clusters {
				if stored, exists := scoped[client.ObjectKeyFromObject(cr)]; exists {
					list.Items = append(list.Items, stored)
				}
			}
			return nil
		},
	}).Build()
}

func clusterAuthzRequest(h *ClusterHandler, operation authz.Action, query, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v0/clusters"+query, nil).WithContext(testContext(testAccountID))
	r = mux.SetURLVars(r, map[string]string{"id": id})
	w := httptest.NewRecorder()
	if operation == authz.ListClusters {
		h.List(w, r)
	} else {
		h.Get(w, r)
	}
	return w
}

func assertClusterAuthzStatus(t *testing.T, w *httptest.ResponseRecorder, want int, message string) {
	t.Helper()
	var status metav1.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid Status body: %v, %s", err, w.Body.String())
	}
	if w.Code != want || status.Code != int32(want) || status.Kind != "Status" || status.APIVersion != "v1" || status.Status != metav1.StatusFailure || status.Message != message || status.Details != nil {
		t.Fatalf("want safe %d Status %q without details, got HTTP %d %+v", want, message, w.Code, status)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("unexpected Content-Type: %v", w.Header())
	}
}

func assertClusterAuthzMetrics(t *testing.T, registry *prometheus.Registry, operation authz.Action, outcome authz.Outcome, stage authz.Stage) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var requests, samples, failures float64
	for _, family := range families {
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["operation"] != string(operation) {
				t.Errorf("unexpected operation: %v", labels)
			}
			switch family.GetName() {
			case "authz_requests_total":
				requests += metric.GetCounter().GetValue()
				if len(labels) != 2 || labels["outcome"] != string(outcome) {
					t.Errorf("unexpected request labels: %v", labels)
				}
			case "authz_duration_seconds":
				samples += float64(metric.GetHistogram().GetSampleCount())
				if len(labels) != 2 || labels["outcome"] != string(outcome) || metric.GetHistogram().GetSampleSum() <= 0 {
					t.Errorf("unexpected duration: %v, %+v", labels, metric.GetHistogram())
				}
			case "authz_failures_total":
				failures += metric.GetCounter().GetValue()
				if len(labels) != 2 || labels["stage"] != string(stage) {
					t.Errorf("unexpected failure labels: %v", labels)
				}
			default:
				t.Errorf("unexpected collector: %s", family.GetName())
			}
		}
	}
	wantFailures := float64(0)
	if outcome == authz.OutcomeError {
		wantFailures = 1
	}
	if requests != 1 || samples != 1 || failures != wantFailures {
		t.Fatalf("want one request/sample and %v failures, got %v/%v/%v", wantFailures, requests, samples, failures)
	}
}

func TestClusterAuthz_NoGrants(t *testing.T) {
	for _, operation := range []authz.Action{authz.ListClusters, authz.DescribeCluster} {
		t.Run(string(operation), func(t *testing.T) {
			reads := 0
			fc := clusterOrderedStore([]*seaplanev1alpha1.Cluster{testClusterCR("owned-id", "owned", testAccountID)}, func(context.Context) error {
				reads++
				return nil
			})
			h, registry, logs := clusterAuthzFixture(t, fc, clusterAuthorizer(t))
			w := clusterAuthzRequest(h, operation, "", "owned")
			assertClusterAuthzStatus(t, w, http.StatusForbidden, "AUTHZ-DENIED-001: Access denied")
			assertClusterAuthzMetrics(t, registry, operation, authz.OutcomeDeny, authz.StageNone)
			assertDenialLog(t, logs, operation, operation)
			wantReads := 0
			if operation == authz.DescribeCluster {
				wantReads = 1
			}
			if reads != wantReads {
				t.Fatalf("storage reads = %d, want %d", reads, wantReads)
			}
		})
	}
}

// assertDenialLog checks a 403 is explained at INFO: request operation, failing action, caller.
func assertDenialLog(t *testing.T, logs *bytes.Buffer, operation, action authz.Action) {
	t.Helper()
	for _, want := range []string{`"level":"INFO"`, `"msg":"request authorization denied"`, `"operation":"` + string(operation) + `"`, `"action":"` + string(action) + `"`, `"caller_arn":"arn:aws:iam::` + testAccountID + `:user/test"`, `"provenance":`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("denial log lacks %s: %s", want, logs.String())
		}
	}
}

// A denied changed-field action names itself, and a forbid names its policy and attachment.
func TestClusterAuthz_DeniedActionLogged(t *testing.T) {
	a := clusterAuthorizer(t, `permit(principal, action == Seaplane::Action::"UpdateCluster", resource);`, `forbid(principal, action == Seaplane::Action::"UpdateClusterVersion", resource);`)
	h, registry, logs := clusterAuthzFixture(t, fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), a)
	r := httptest.NewRequest(http.MethodPut, "/api/v0/clusters/owned-id", nil).WithContext(testContext(testAccountID))
	w := httptest.NewRecorder()
	if authorizeResource(w, r, h.authorizer, h.logger, authz.UpdateCluster, h.clusterResource(testClusterCR("owned-id", "owned", testAccountID)), authz.UpdateClusterVersion) {
		t.Fatal("forbidden changed-field action allowed")
	}
	assertClusterAuthzStatus(t, w, http.StatusForbidden, "AUTHZ-DENIED-001: Access denied")
	assertClusterAuthzMetrics(t, registry, authz.UpdateCluster, authz.OutcomeDeny, authz.StageNone)
	assertDenialLog(t, logs, authz.UpdateCluster, authz.UpdateClusterVersion)
	if !strings.Contains(logs.String(), "grant-1") || strings.Contains(logs.String(), "forbid(") {
		t.Fatalf("denial log must name the forbid by ID, never its content: %s", logs.String())
	}
}

// A List grant alone shows every account cluster as ID, name, and state; paging counts them all.
func TestClusterAuthz_ListVisibility(t *testing.T) {
	first := testClusterCR("first", "one", testAccountID)
	first.Status.Phase = seaplanev1alpha1.ClusterPhaseReady
	second := testClusterCR("second", "two", testAccountID)
	foreign := testClusterCR("foreign", "foreign", "999999999999")
	all := []*seaplanev1alpha1.Cluster{first, second, foreign}
	for _, tc := range []struct {
		name                         string
		policies                     []string
		objects                      []*seaplanev1alpha1.Cluster
		query                        string
		ids                          []string
		total, limit, offset, status int
	}{
		{"list grant only", []string{clusterListPermit}, all, "", []string{"first", "second"}, 2, 50, 0, http.StatusOK},
		{"describe forbid still lists", []string{clusterListPermit, `forbid(principal, action == Seaplane::Action::"DescribeCluster", resource);`}, all, "", []string{"first", "second"}, 2, 50, 0, http.StatusOK},
		{"second page", []string{clusterListPermit}, all, "?limit=1&offset=1", []string{"second"}, 2, 1, 1, http.StatusOK},
		{"offset beyond list", []string{clusterListPermit}, all, "?limit=1&offset=2", []string{}, 2, 1, 2, http.StatusOK},
		{"empty storage", []string{clusterListPermit}, nil, "", []string{}, 0, 50, 0, http.StatusOK},
		{"describe grant only", []string{clusterDescribePermit}, all, "", nil, 0, 50, 0, http.StatusForbidden},
		{"collection forbid", []string{clusterReadPermit, `forbid(principal, action == Seaplane::Action::"ListClusters", resource);`}, all, "", nil, 0, 50, 0, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			fc := clusterOrderedStore(tc.objects, func(context.Context) error { reads++; return nil })
			h, registry, _ := clusterAuthzFixture(t, fc, clusterAuthorizer(t, tc.policies...))
			w := clusterAuthzRequest(h, authz.ListClusters, tc.query, "")
			if tc.status == http.StatusForbidden {
				assertClusterAuthzStatus(t, w, tc.status, "AUTHZ-DENIED-001: Access denied")
				assertClusterAuthzMetrics(t, registry, authz.ListClusters, authz.OutcomeDeny, authz.StageNone)
				if reads != 0 {
					t.Fatal("collection denial reached storage")
				}
				return
			}
			var body struct {
				Items  []public.Cluster `json:"items"`
				Total  int              `json:"total"`
				Limit  int              `json:"limit"`
				Offset int              `json:"offset"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(body.Items))
			for _, item := range body.Items {
				ids = append(ids, string(item.UID))
				// List returns full objects; Describe is not checked per item.
				if item.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
					t.Fatalf("list item lacks full details: %+v", item)
				}
			}
			if w.Code != http.StatusOK || !reflect.DeepEqual(ids, tc.ids) || body.Items == nil || body.Total != tc.total || body.Limit != tc.limit || body.Offset != tc.offset || reads != 1 {
				t.Fatalf("want ids=%v total=%d limit=%d offset=%d, got HTTP %d ids=%v body=%s reads=%d", tc.ids, tc.total, tc.limit, tc.offset, w.Code, ids, w.Body.String(), reads)
			}
			assertClusterAuthzMetrics(t, registry, authz.ListClusters, authz.OutcomeAllow, authz.StageNone)
		})
	}
}

func TestClusterAuthz_TrustedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, label, tag string
		policies         []string
		status           int
	}{
		{"metadata grants despite tags", "blue", "red", []string{clusterLabelPermit}, http.StatusOK},
		{"tags cannot grant", "red", "blue", []string{clusterLabelPermit}, http.StatusForbidden},
		{"absent metadata label", "", "blue", []string{clusterLabelPermit}, http.StatusForbidden},
		{"UID identity and label ownership", "blue", "red", []string{`permit(principal, action == Seaplane::Action::"DescribeCluster", resource == Seaplane::Cluster::"123456789012/us-east-1/fleetdb-uid-not-cluster-id") when { resource.account == "123456789012" && resource.region == "us-east-1" };`}, http.StatusOK},
		{"list grant cannot describe", "blue", "blue", []string{clusterListPermit}, http.StatusForbidden},
		{"describe grant", "red", "blue", []string{clusterDescribePermit}, http.StatusOK},
		{"forbid overrides permit", "blue", "blue", []string{clusterReadPermit, `forbid(principal, action == Seaplane::Action::"DescribeCluster", resource);`}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := testClusterCR("stable-id", "untrusted-name", testAccountID)
			cr.UID = types.UID("fleetdb-uid-not-cluster-id")
			cr.Spec.AccountID = "999999999999"
			cr.Spec.HostedCluster.Platform.AWS.Region = "us-west-2"
			cr.Spec.Tags = map[string]string{"example.com/team": tc.tag}
			if tc.label != "" {
				cr.Annotations = map[string]string{"example.com/team": tc.label}
			}
			h, registry, _ := clusterAuthzFixture(t, clusterOrderedStore([]*seaplanev1alpha1.Cluster{cr}, nil), clusterAuthorizer(t, tc.policies...))
			r := httptest.NewRequest(http.MethodGet, "/api/v0/clusters/untrusted-name?example.com/team=blue&accountId=999999999999&region=us-west-2", bytes.NewBufferString(`{"metadata":{"uid":"spoof-id","labels":{"example.com/team":"blue","seaplane.io/account-id":"999999999999"}},"spec":{"tags":{"example.com/team":"blue"}}}`)).WithContext(testContext(testAccountID))
			r.Header.Set("X-Resource-Labels", `{"example.com/team":"blue"}`)
			r.Header.Set("X-Account-ID", "999999999999")
			r = mux.SetURLVars(r, map[string]string{"id": "untrusted-name"})
			w := httptest.NewRecorder()
			h.Get(w, r)
			if tc.status == http.StatusForbidden {
				assertClusterAuthzStatus(t, w, tc.status, "AUTHZ-DENIED-001: Access denied")
				assertClusterAuthzMetrics(t, registry, authz.DescribeCluster, authz.OutcomeDeny, authz.StageNone)
				return
			}
			var body public.Cluster
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || string(body.UID) != "fleetdb-uid-not-cluster-id" || body.Labels[seaplanev1alpha1.AccountIDLabel] != testAccountID {
				t.Fatalf("stored UID and label ownership not used: %d %s", w.Code, w.Body.String())
			}
			assertClusterAuthzMetrics(t, registry, authz.DescribeCluster, authz.OutcomeAllow, authz.StageNone)
		})
	}
}

func TestClusterAuthz_ForeignAndMissing(t *testing.T) {
	foreign := testClusterCR("foreign", "foreign", "999999999999")
	foreign.Spec.AccountID = testAccountID
	fc := clusterOrderedStore([]*seaplanev1alpha1.Cluster{foreign}, nil)
	for _, id := range []string{"foreign", "missing"} {
		t.Run(id, func(t *testing.T) {
			h, registry, _ := clusterAuthzFixture(t, fc, clusterAuthorizer(t, clusterReadPermit))
			w := clusterAuthzRequest(h, authz.DescribeCluster, "", id)
			assertClusterAuthzStatus(t, w, http.StatusNotFound, ErrClusterGetNotFound.Code+": "+ErrClusterGetNotFound.Message)
			assertNoAuthzMetrics(t, registry)
		})
	}
}

func TestClusterAuthz_CanceledPreparation(t *testing.T) {
	for _, operation := range []authz.Action{authz.ListClusters, authz.DescribeCluster} {
		t.Run(string(operation), func(t *testing.T) {
			reads := 0
			fc := clusterOrderedStore([]*seaplanev1alpha1.Cluster{testClusterCR("owned", "owned", testAccountID)}, func(context.Context) error { reads++; return nil })
			h, registry, logs := clusterAuthzFixture(t, fc, clusterAuthorizer(t, clusterReadPermit))
			ctx, cancel := context.WithCancel(testContext(testAccountID))
			cancel()
			r := httptest.NewRequest(http.MethodGet, "/api/v0/clusters", nil).WithContext(ctx)
			r = mux.SetURLVars(r, map[string]string{"id": "owned"})
			w := httptest.NewRecorder()
			if operation == authz.ListClusters {
				h.List(w, r)
			} else {
				h.Get(w, r)
			}
			assertClusterAuthzStatus(t, w, http.StatusInternalServerError, "AUTHZ-FAILED-001: Authorization failed")
			assertClusterAuthzMetrics(t, registry, operation, authz.OutcomeError, authz.StageResolution)
			if reads != 0 || !bytes.Contains(logs.Bytes(), []byte("context canceled")) || !bytes.Contains(logs.Bytes(), []byte(`"cause":`)) || !bytes.Contains(logs.Bytes(), []byte(`"region":"us-east-1"`)) {
				t.Fatalf("canceled preparation reached storage or lost cause/region: reads=%d logs=%s", reads, logs.String())
			}
		})
	}
}

func TestClusterAuthz_LoadingErrors(t *testing.T) {
	for _, operation := range []authz.Action{authz.ListClusters, authz.DescribeCluster} {
		t.Run(string(operation), func(t *testing.T) {
			fc := clusterOrderedStore(nil, func(context.Context) error { return errors.New("storage-detail-secret") })
			h, registry, logs := clusterAuthzFixture(t, fc, clusterAuthorizer(t, clusterReadPermit))
			w := clusterAuthzRequest(h, operation, "", "owned")
			want := ErrClusterList
			if operation == authz.DescribeCluster {
				want = ErrClusterGetFailed
			}
			assertClusterAuthzStatus(t, w, want.HTTPStatus, want.Code+": "+want.Message)
			assertClusterAuthzMetrics(t, registry, operation, authz.OutcomeError, authz.StageResourceLoading)
			if !bytes.Contains(logs.Bytes(), []byte("storage-detail-secret")) {
				t.Fatalf("storage cause not logged: %s", logs.String())
			}
		})
	}
}

func TestClusterAuthz_StoredResourceFaults(t *testing.T) {
	for _, operation := range []authz.Action{authz.DescribeCluster} {
		for _, fault := range []string{"foreign ownership", "invalid UID"} {
			t.Run(string(operation)+"/"+fault, func(t *testing.T) {
				cr := testClusterCR("owned", "owned", testAccountID)
				switch fault {
				case "foreign ownership":
					cr.Labels[seaplanev1alpha1.AccountIDLabel] = "999999999999"
				case "invalid UID":
					cr.UID = "../escape"
				}
				// Corrupt storage material must fail closed even if a storage adapter breaks its selector contract.
				fc := fake.NewClientBuilder().WithScheme(newTestScheme()).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
						*obj.(*seaplanev1alpha1.Cluster) = *cr.DeepCopy()
						return nil
					},
					List: func(_ context.Context, _ client.WithWatch, obj client.ObjectList, _ ...client.ListOption) error {
						obj.(*seaplanev1alpha1.ClusterList).Items = []seaplanev1alpha1.Cluster{*cr.DeepCopy()}
						return nil
					},
				}).Build()
				h, registry, logs := clusterAuthzFixture(t, fc, clusterAuthorizer(t, clusterReadPermit))
				w := clusterAuthzRequest(h, operation, "", "owned")
				assertClusterAuthzStatus(t, w, http.StatusInternalServerError, "AUTHZ-FAILED-001: Authorization failed")
				assertClusterAuthzMetrics(t, registry, operation, authz.OutcomeError, authz.StageEntityValidation)
				if !bytes.Contains(logs.Bytes(), []byte(`"cause":`)) {
					t.Fatalf("resource failure cause not logged: %s", logs.String())
				}
			})
		}
	}
}

func TestClusterAuthz_LateEvaluationError(t *testing.T) {
	first := testClusterCR("first", "first", testAccountID)
	late := testClusterCR("late", "late", testAccountID)
	late.Annotations = map[string]string{"example.com/fault": "fault-detail-secret"}
	for _, operation := range []authz.Action{authz.DescribeCluster} {
		t.Run(string(operation), func(t *testing.T) {
			h, registry, logs := clusterAuthzFixture(t, clusterOrderedStore([]*seaplanev1alpha1.Cluster{first, late}, nil), clusterAuthorizer(t, clusterReadPermit, clusterFailingForbid))
			w := clusterAuthzRequest(h, operation, "?limit=1", "late")
			assertClusterAuthzStatus(t, w, http.StatusInternalServerError, "AUTHZ-FAILED-001: Authorization failed")
			assertClusterAuthzMetrics(t, registry, operation, authz.OutcomeError, authz.StageEvaluation)
			if !bytes.Contains(logs.Bytes(), []byte("attachment/grant-1")) || !bytes.Contains(logs.Bytes(), []byte(`"BundleHash":`)) || !bytes.Contains(logs.Bytes(), []byte("cedar evaluation diagnostics")) || !bytes.Contains(logs.Bytes(), []byte(`"diagnostics":[{`)) {
				t.Fatalf("evaluation failure lost detailed diagnostics/provenance: %s", logs.String())
			}
			// A list reports its request operation but names the item check that failed.
			if !bytes.Contains(logs.Bytes(), []byte(`"msg":"request authorization failed"`)) || !bytes.Contains(logs.Bytes(), []byte(`"operation":"`+string(operation)+`"`)) || !bytes.Contains(logs.Bytes(), []byte(`"action":"DescribeCluster"`)) {
				t.Fatalf("failure log lacks message, operation, or failing action: %s", logs.String())
			}
		})
	}
}
