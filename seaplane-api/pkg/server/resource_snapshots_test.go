package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/clients/seaplanedb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/middleware"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const resourceTestAccount = "123456789012"
const resourceTestClusterID = "550e8400-e29b-41d4-a716-446655440000"

func resourceServer(t *testing.T, customer []string, hooks interceptor.Funcs, objects ...client.Object) (*Server, client.Client) {
	t.Helper()
	policies, attachments := []any{}, []any{}
	for i, content := range customer {
		id := fmt.Sprintf("customer-%d", i)
		policies = append(policies, map[string]any{"id": id, "ownerAccountID": resourceTestAccount, "content": content})
		attachments = append(attachments, map[string]any{"id": id, "policyID": id, "principalARN": "arn:aws:iam::" + resourceTestAccount + ":user/test", "scope": "global"})
	}
	bundle := map[string]any{"formatVersion": 1, "registeredAccounts": []string{resourceTestAccount}, "policies": policies, "attachments": attachments}
	content, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authz.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	authorizer, err := authz.LoadConfig(path, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// seaplane-db assigns the UID at create time; controller-runtime's fake does not.
	create := hooks.Create
	hooks.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetUID(types.UID(uuid.NewString()))
		if create != nil {
			return create(ctx, c, obj, opts...)
		}
		return c.Create(ctx, obj, opts...)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithIndex(&v1.OidcConfig{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).
		WithIndex(&v1.DNSReservation{}, "metadata.uid", func(obj client.Object) []string {
			return []string{string(obj.GetUID())}
		}).WithInterceptorFuncs(hooks).Build()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	// Existing route cases verify the real server binds the hash, including default denials.
	t.Cleanup(func() {
		decoder := json.NewDecoder(&logs)
		for decoder.More() {
			var entry struct {
				Message    string `json:"msg"`
				BundleHash string `json:"bundle_hash"`
			}
			if err := decoder.Decode(&entry); err != nil {
				t.Error(err)
				return
			}
			if (entry.Message == "request authorization denied" || entry.Message == "request authorization failed") && entry.BundleHash != authorizer.BundleHash() {
				t.Errorf("authorization log bundle_hash=%q, want %q", entry.BundleHash, authorizer.BundleHash())
			}
		}
	})
	cfg := config.NewConfig()
	cfg.Regional.AWSRegion = "us-east-1"
	cfg.Server.MaxRequestBodyBytes = 1 << 20 // 1 MiB — match the upstream hardcoded limit
	srv, err := New(cfg, seaplanedb.NewClientFrom(fc, logger), authorizer, logger)
	if err != nil {
		t.Fatal(err)
	}
	return srv, fc
}
func resourceRequest(srv *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v0/"+path, strings.NewReader(body))
	r.Header.Set(middleware.HeaderAccountID, resourceTestAccount)
	r.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::"+resourceTestAccount+":user/test")
	w := httptest.NewRecorder()
	srv.apiServer.Handler.ServeHTTP(w, r)
	return w
}
func resourceFixtures() []client.Object {
	labels := map[string]string{v1.AccountIDLabel: resourceTestAccount}
	tags := map[string]string{"team": "blue"}
	parent := &v1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "account-" + resourceTestAccount, UID: types.UID(resourceTestClusterID), Labels: labels, Annotations: tags}}
	pool := &v1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent.workers", Namespace: parent.Namespace, UID: "pool-uid",
			Labels:          map[string]string{v1.AccountIDLabel: resourceTestAccount, v1.ClusterUIDLabel: resourceTestClusterID},
			Annotations:     map[string]string{"team": "blue"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(parent, v1.GroupVersion.WithKind("Cluster"))},
		},
		Spec: v1.NodePoolSpec{NodePool: v1.NodePoolSpecPassthrough{ClusterName: parent.Name}},
	}
	return []client.Object{
		parent,
		pool,
		&v1.OidcConfig{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: parent.Namespace, UID: "config-uid", Labels: labels, Annotations: tags}},
		&v1.DNSReservation{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: parent.Namespace, UID: "zone-uid", Labels: labels, Annotations: tags}},
	}
}
func actionPermit(action string) string {
	return `permit(principal, action == Seaplane::Action::"` + action + `", resource);`
}

func TestImplementedRoutesActionSpecific(t *testing.T) {
	for _, tc := range []struct {
		action, group, method, path, body string
		status                            int
	}{
		{"ListClusters", "ReadOnly", "GET", "clusters", "", 200}, {"DescribeCluster", "ReadOnly", "GET", "clusters/parent", "", 200}, {"CreateCluster", "ClusterAdmin", "POST", "clusters", `{"metadata":{"name":"new"},"spec":{"properties":{"team":"new"}}}`, 201}, {"UpdateCluster", "ClusterAdmin", "PUT", "clusters/parent", `{"spec":{"properties":{"team":"new"}}}`, 200}, {"UpdateCluster", "ClusterAdmin", "PATCH", "clusters/parent", `{"spec":{"properties":{"team":"new"}}}`, 200}, {"DeleteCluster", "ClusterAdmin", "DELETE", "clusters/parent", "", 202},
		{"ListNodePools", "ReadOnly", "GET", "nodepools", "", 200}, {"DescribeNodePool", "ReadOnly", "GET", "nodepools/parent.workers", "", 200}, {"CreateNodePool", "NodePoolAdmin", "POST", "nodepools", `{"metadata":{"name":"parent.new"},"spec":{"nodePool":{"clusterName":"parent"}}}`, 201}, {"UpdateNodePool", "NodePoolAdmin", "PUT", "nodepools/parent.workers", `{"spec":{"labels":{"team":"new"}}}`, 200}, {"UpdateNodePool", "NodePoolAdmin", "PATCH", "nodepools/parent.workers", `{"spec":{"labels":{"team":"new"}}}`, 200}, {"DeleteNodePool", "NodePoolAdmin", "DELETE", "nodepools/parent.workers", "", 202},
		{"ListOIDCConfigs", "ReadOnly", "GET", "oidc_configs", "", 200}, {"DescribeOIDCConfig", "ReadOnly", "GET", "oidc_configs/config", "", 200}, {"CreateOIDCConfig", "OIDCConfigAdmin", "POST", "oidc_configs", `{"metadata":{"name":"new"},"spec":{"type":"unmanaged","issuerUrl":"https://example.com","secretArn":"secret","installerRoleArn":"role"}}`, 201}, {"DeleteOIDCConfig", "OIDCConfigAdmin", "DELETE", "oidc_configs/config", "", 202},
		{"ListDNSReservations", "ReadOnly", "GET", "dns_reservations", "", 200}, {"DescribeDNSReservation", "ReadOnly", "GET", "dns_reservations/zone", "", 200}, {"CreateDNSReservation", "DNSReservationAdmin", "POST", "dns_reservations", `{"metadata":{"name":"new"}}`, 201}, {"DeleteDNSReservation", "DNSReservationAdmin", "DELETE", "dns_reservations/zone", "", 202},
		{"ListLabelsForResource", "ReadOnly", "GET", "clusters/parent/labels", "", 200},
		{"LabelResource", "LabelAdmin", "PATCH", "clusters/parent/labels", `{"new":"value"}`, 204},
		{"UnlabelResource", "LabelAdmin", "PATCH", "clusters/parent/labels", `{"team":null}`, 204},
		{"ListLabelsForResource", "ReadOnly", "GET", "nodepools/parent.workers/labels", "", 200},
		{"LabelResource", "LabelAdmin", "PATCH", "nodepools/parent.workers/labels", `{"new":"value"}`, 204},
		{"UnlabelResource", "LabelAdmin", "PATCH", "nodepools/parent.workers/labels", `{"team":null}`, 204},
		{"ListLabelsForResource", "ReadOnly", "GET", "oidc_configs/config/labels", "", 200},
		{"LabelResource", "LabelAdmin", "PATCH", "oidc_configs/config/labels", `{"new":"value"}`, 204},
		{"UnlabelResource", "LabelAdmin", "PATCH", "oidc_configs/config/labels", `{"team":null}`, 204},
		{"ListLabelsForResource", "ReadOnly", "GET", "dns_reservations/zone/labels", "", 200},
		{"LabelResource", "LabelAdmin", "PATCH", "dns_reservations/zone/labels", `{"new":"value"}`, 204},
		{"UnlabelResource", "LabelAdmin", "PATCH", "dns_reservations/zone/labels", `{"team":null}`, 204},
	} {
		for _, grant := range []string{"matching", "wrong action", "unbound principal", "runtime error", "group", "AllActions", "permit + forbid"} {
			t.Run(fmt.Sprintf("%s %s grant=%s", tc.method, tc.path, grant), func(t *testing.T) {
				canAccess := grant == "matching" || grant == "group" || grant == "AllActions"
				action := tc.action
				if grant == "wrong action" {
					action = "CreateCluster"
					if tc.action == action {
						action = "DescribeCluster"
					}
				}
				policies := []string{actionPermit(action)}
				switch grant {
				case "group", "AllActions":
					group := tc.group
					if grant == "AllActions" {
						group = grant
					}
					policies = []string{`permit(principal, action in Seaplane::Action::"` + group + `", resource);`}
				case "permit + forbid":
					policies = append(policies, strings.Replace(actionPermit(action), "permit(", "forbid(", 1))
				}
				if grant == "runtime error" {
					policies = append(policies, `forbid(principal, action == Seaplane::Action::"`+action+`", resource) when { 9223372036854775807 + 1 == 0 };`)
				}
				writes := 0
				hooks := interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
						writes++
						return c.Create(ctx, o, opts...)
					},
					Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
						writes++
						return c.Update(ctx, o, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
						writes++
						return c.Delete(ctx, o, opts...)
					},
				}
				srv, _ := resourceServer(t, policies, hooks, resourceFixtures()...)
				r := httptest.NewRequest(tc.method, "/api/v0/"+tc.path, strings.NewReader(tc.body))
				r.Header.Set(middleware.HeaderAccountID, resourceTestAccount)
				principal := "test"
				if grant == "unbound principal" {
					principal = "no-grants"
				}
				r.Header.Set(middleware.HeaderCallerARN, "arn:aws:iam::"+resourceTestAccount+":user/"+principal)
				before := authzMetricSamples(t)
				w := httptest.NewRecorder()
				srv.apiServer.Handler.ServeHTTP(w, r)
				want := tc.status
				if !canAccess {
					want = 403
				}
				if grant == "runtime error" {
					want = 500
				}
				assertRouteResponse(t, w, before, action, want)
				expectedWrites := 0
				if canAccess && tc.method != "GET" {
					expectedWrites = 1
				}
				if writes != expectedWrites {
					t.Fatalf("writes=%d want=%d", writes, expectedWrites)
				}
				// List permission alone returns full objects, labels included.
				if strings.HasPrefix(tc.action, "List") && tc.action != "ListLabelsForResource" && canAccess {
					var b struct{ Total int }
					if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || b.Total != 1 || !strings.Contains(w.Body.String(), "labels") {
						t.Fatalf("List must return full objects: %s (%v)", w.Body.String(), err)
					}
				}
			})
		}
	}
}

// Check route status and keep runtime-error sanitization and metric assertions together.
func assertRouteResponse(t *testing.T, w *httptest.ResponseRecorder, before map[string]float64, action string, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("want %d got %d: %s", want, w.Code, w.Body.String())
	}
	if want != http.StatusInternalServerError {
		return
	}
	var status metav1.Status
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Kind != "Status" || status.Code != 500 || status.Message != "AUTHZ-FAILED-001: Authorization failed" || status.Details != nil || strings.Contains(w.Body.String(), `"items"`) {
		t.Fatalf("unsafe runtime error response: %s (%v)", w.Body.String(), err)
	}
	after := authzMetricSamples(t)
	for _, key := range []string{"authz_requests_total/operation=" + action + "/outcome=error", "authz_duration_seconds/operation=" + action + "/outcome=error", "authz_failures_total/operation=" + action + "/stage=evaluation"} {
		if after[key]-before[key] != 1 {
			t.Fatalf("runtime failure sample %s increased by %v, want 1", key, after[key]-before[key])
		}
	}
}

func TestEveryChangedUpdateActionBeforeWrite(t *testing.T) {
	// Check individual changes and their combination, dropping each required grant in turn.
	for _, tc := range []struct {
		path, body string
		extras     []string
		status     int
	}{
		{"clusters/parent", `{"spec":{"hostedCluster":{"release":{"image":"new"}}}}`, []string{"UpdateClusterVersion"}, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"configuration":{"proxy":{"httpProxy":"http://new"}}}}}`, []string{"UpdateClusterConfig"}, 200},
		{"clusters/parent", `{"spec":{"properties":{"team":"new"},"hostedCluster":{"release":{"image":"new"},"configuration":{"proxy":{"httpProxy":"http://new"}}}}}`, []string{"UpdateClusterVersion", "UpdateClusterConfig"}, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"configuration":null}}}`, []string{"UpdateClusterConfig"}, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"release":null}}}`, nil, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"configuration":{"proxy":{"httpProxy":"http://old"}}}}}`, nil, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"release":{"image":"old"}}}}`, nil, 200},
		{"clusters/parent", `{"spec":{"controlPlaneUpgradePolicy":{"updateType":"user","scheduleType":"manual"}}}`, []string{"UpdateClusterVersion"}, 200},
		{"clusters/parent", `{"spec":{"hostedCluster":{"Release":{"image":"new"}}}}`, []string{"UpdateClusterVersion"}, 200},
		{"clusters/parent", `{"spec":{"accountId":"222222222222","hostedCluster":{"release":{"image":"new"}}}}`, []string{"UpdateClusterVersion"}, 422},
		{"nodepools/parent.workers", `{"spec":{"labels":{"team":"new"},"nodePool":{"replicas":0,"release":{"image":"new"}}}}`, []string{"ScaleNodePool", "UpdateNodePoolVersion"}, 200},
		{"nodepools/parent.workers", `{"spec":{"nodePool":{"replicas":null}}}`, []string{"ScaleNodePool"}, 200},
		{"nodepools/parent.workers", `{"spec":{"nodePool":{"replicas":3}}}`, nil, 200},
		{"nodepools/parent.workers", `{"spec":{"nodePool":{"autoScaling":{"min":1,"max":5}}}}`, []string{"ScaleNodePool"}, 200},
		{"nodepools/parent.workers", `{"spec":{"nodePool":{"Replicas":5}}}`, []string{"ScaleNodePool"}, 200},
		{"nodepools/parent.workers", `{"spec":{"accountId":"222222222222","nodePool":{"replicas":5}}}`, []string{"ScaleNodePool"}, 422},
	} {
		base := "UpdateNodePool"
		if strings.HasPrefix(tc.path, "clusters/") {
			base = "UpdateCluster"
		}
		missingActions := append([]string{"", base}, tc.extras...)
		if tc.status == 422 {
			missingActions = []string{""}
		}
		for _, method := range []string{"PUT", "PATCH"} {
			for _, missing := range missingActions {
				t.Run(method+tc.path+" "+tc.body+" missing="+missing, func(t *testing.T) {
					objects := resourceFixtures()
					if err := seaplanedb.MergeSpecJSON(&objects[0].(*v1.Cluster).Spec, []byte(`{"hostedCluster":{"release":{"image":"old"},"configuration":{"proxy":{"httpProxy":"http://old"}}}}`)); err != nil {
						t.Fatal(err)
					}
					replicas := int32(3)
					objects[1].(*v1.NodePool).Spec.NodePool.Replicas = &replicas
					permits := []string{}
					actions := append([]string{base}, tc.extras...)
					for _, action := range actions {
						if action != missing {
							permits = append(permits, actionPermit(action))
						}
					}
					writes := 0
					srv, store := resourceServer(t, permits, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
						writes++
						return c.Update(ctx, o, opts...)
					}}, objects...)
					w := resourceRequest(srv, method, tc.path, tc.body)
					want, wantWrites := tc.status, 1
					if missing != "" {
						want = 403
					}
					if want != 200 {
						wantWrites = 0
					}
					if w.Code != want {
						t.Fatalf("want %d got %d: %s", want, w.Code, w.Body.String())
					}
					if writes != wantWrites {
						t.Fatalf("writes=%d want=%d", writes, wantWrites)
					}
					if missing == "" && strings.Contains(tc.body, "http://new") {
						var saved v1.Cluster
						if err := store.Get(context.Background(), client.ObjectKeyFromObject(objects[0]), &saved); err != nil || saved.Spec.HostedCluster.Configuration == nil || saved.Spec.HostedCluster.Configuration.Proxy == nil || saved.Spec.HostedCluster.Configuration.Proxy.HTTPProxy != "http://new" {
							t.Fatalf("configuration did not persist: %+v %v", saved.Spec.HostedCluster.Configuration, err)
						}
					}
					if missing == "" && strings.Contains(tc.body, `"release":null`) {
						var saved v1.Cluster
						if err := store.Get(context.Background(), client.ObjectKeyFromObject(objects[0]), &saved); err != nil || saved.Spec.HostedCluster.Release.Image != "old" {
							t.Fatalf("null changed release: %+v %v", saved.Spec.HostedCluster.Release, err)
						}
					}
					if missing != "" {
						var np v1.NodePool
						if err := store.Get(context.Background(), client.ObjectKeyFromObject(objects[1]), &np); err != nil || *np.Spec.NodePool.Replicas != 3 {
							t.Fatalf("denial changed stored candidate: %+v %v", np, err)
						}
					}
				})
			}
		}
	}
}
