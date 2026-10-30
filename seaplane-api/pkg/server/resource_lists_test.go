package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// List permission alone returns every item in full; Describe is not checked per item.
func TestResourceCollectionsListFull(t *testing.T) {
	for _, tc := range []struct {
		path, list, describe string
		index                int
	}{
		{"clusters", "ListClusters", "DescribeCluster", 0}, {"nodepools", "ListNodePools", "DescribeNodePool", 1}, {"oidc_configs", "ListOIDCConfigs", "DescribeOIDCConfig", 2}, {"dns_reservations", "ListDNSReservations", "DescribeDNSReservation", 3},
	} {
		for _, mode := range []string{"list-only", "describe-only"} {
			t.Run(tc.path+"/"+mode, func(t *testing.T) {
				objects := resourceFixtures()
				second := objects[tc.index].DeepCopyObject().(client.Object)
				second.SetName("second")
				second.SetUID(types.UID(string(objects[tc.index].GetUID()) + "-second"))
				objects = append(objects, second)
				policy := map[string]string{"list-only": actionPermit(tc.list), "describe-only": actionPermit(tc.describe)}[mode]
				srv, _ := resourceServer(t, []string{policy}, interceptor.Funcs{}, objects...)
				w := resourceRequest(srv, "GET", tc.path, "")
				if mode == "describe-only" {
					if w.Code != 403 || strings.Contains(w.Body.String(), `"items"`) {
						t.Fatalf("want 403 without items, got %d: %s", w.Code, w.Body.String())
					}
					return
				}
				var body struct {
					Total int
					Items []json.RawMessage
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.Total != 2 || len(body.Items) != 2 {
					t.Fatalf("want every item: %d %s (%v)", w.Code, w.Body.String(), err)
				}
				for _, item := range body.Items {
					if !strings.Contains(string(item), `"team"`) {
						t.Fatalf("list item lacks full details: %s", item)
					}
				}
			})
		}
	}
}

func TestNodePoolNamePinsParent(t *testing.T) {
	const secondID = "550e8400-e29b-41d4-a716-446655440001"
	objects := resourceFixtures()
	parent := objects[0].DeepCopyObject().(*v1.Cluster)
	parent.Name, parent.UID = "second-parent", types.UID(secondID)
	second := objects[1].DeepCopyObject().(*v1.NodePool)
	second.Name, second.UID = "second-parent.workers", "second-pool-uid"
	second.Spec.NodePool.ClusterName = parent.Name
	second.Labels[v1.ClusterUIDLabel] = secondID
	second.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(parent, v1.GroupVersion.WithKind("Cluster"))}
	objects = append(objects, second, parent)
	hooks := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if pools, ok := list.(*v1.NodePoolList); ok {
			sort.Slice(pools.Items, func(i, j int) bool { return pools.Items[i].Name < pools.Items[j].Name })
		}
		return nil
	}}
	policy := `permit(principal,action == Seaplane::Action::"DescribeNodePool",resource in Seaplane::Cluster::"123456789012/us-east-1/` + secondID + `");`
	srv, _ := resourceServer(t, []string{policy}, hooks, objects...)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"parent.workers", 403},
		{"parent.workers?clusterId=" + resourceTestClusterID, 403},
		{"parent.workers?clusterId=" + secondID, 403},
		{"second-parent.workers", 200},
	} {
		w := resourceRequest(srv, "GET", "nodepools/"+tc.path, "")
		if w.Code != tc.want {
			t.Fatalf("path %s want %d got %d: %s", tc.path, tc.want, w.Code, w.Body.String())
		}
	}
	// List permission returns every pool in full, whatever the parent grants.
	srv, _ = resourceServer(t, []string{actionPermit("ListNodePools"), policy}, hooks, objects...)
	w := resourceRequest(srv, "GET", "nodepools", "")
	var result struct {
		Items []struct {
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
		Total int
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	var detailed []string
	for _, item := range result.Items {
		if item.Metadata.Labels != nil {
			detailed = append(detailed, item.Metadata.Name)
		}
	}
	if w.Code != 200 || result.Total != 2 || len(result.Items) != 2 || len(detailed) != 2 {
		t.Fatalf("list must return every pool in full: %s", w.Body.String())
	}
}

// List no longer loads each pool's parent cluster, so a parent lookup cannot fail it.
func TestNodePoolListSkipsParentLookups(t *testing.T) {
	objects := resourceFixtures()
	lookups := 0
	hooks := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*v1.Cluster); ok {
			lookups++
			return fmt.Errorf("parent lookup failed")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	srv, _ := resourceServer(t, []string{`permit(principal,action in Seaplane::Action::"ReadOnly",resource);`}, hooks, objects...)
	w := resourceRequest(srv, "GET", "nodepools?limit=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items"`) || lookups != 0 {
		t.Fatalf("list loaded parents or failed: %d lookups=%d %s", w.Code, lookups, w.Body.String())
	}
}

func TestCreatesUseValidatedCandidateLabelsAndOwnership(t *testing.T) {
	for _, tc := range []struct{ action, path, body string }{
		{"CreateCluster", "clusters", `{"metadata":{"name":"new","annotations":LABELS},"spec":{"properties":{"team":"new"}}}`},
		{"CreateNodePool", "nodepools?clusterId=foreign", `{"metadata":{"name":"parent.new","annotations":LABELS},"spec":{"nodePool":{"clusterName":"parent"}}}`},
		{"CreateOIDCConfig", "oidc_configs", `{"metadata":{"name":"new","annotations":LABELS},"spec":{"type":"unmanaged","issuerUrl":"https://example.com","secretArn":"secret","installerRoleArn":"role"}}`},
		{"CreateDNSReservation", "dns_reservations", `{"metadata":{"name":"new","annotations":LABELS}}`},
	} {
		// System labels, even syntactically invalid ones, are discarded; customer syntax errors reject the create.
		for _, check := range []struct {
			labels, grant string
			status        int
		}{
			{`{"team":"blue","seaplane.io/account-id":"222222222222","seaplane.io/cluster-namespace":"forged","seaplane.io/bad key":"bad value"}`, "blue", 201},
			{`{"team":"blue","seaplane.io/account-id":"222222222222"}`, "red", 403},
			{`{"bad key":"blue"}`, "blue", 422},
			{`{"team":"bad value"}`, "blue", 422},
		} {
			t.Run(tc.action+check.labels+check.grant, func(t *testing.T) {
				writes := 0
				policy := `permit(principal,action == Seaplane::Action::"` + tc.action + `",resource) when { resource.account == "123456789012" && resource.hasTag("team") && resource.getTag("team") == "` + check.grant + `" };`
				srv, _ := resourceServer(t, []string{policy}, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++
					if obj.GetAnnotations()["team"] != "blue" || obj.GetLabels()["team"] != "" || obj.GetLabels()[v1.AccountIDLabel] != resourceTestAccount {
						t.Errorf("customer labels not stored as tags, or forged ownership: %v %v", obj.GetLabels(), obj.GetAnnotations())
					}
					return c.Create(ctx, obj, opts...)
				}}, resourceFixtures()...)
				w := resourceRequest(srv, "POST", tc.path, strings.Replace(tc.body, "LABELS", check.labels, 1))
				wantWrites := 0
				if check.status == 201 {
					wantWrites = 1
				}
				if w.Code != check.status || writes != wantWrites {
					t.Fatalf("status=%d writes=%d, want %d/%d: %s", w.Code, writes, check.status, wantWrites, w.Body.String())
				}
			})
		}
	}
}

func TestDeniedClusterCreateDoesNotClaimOIDC(t *testing.T) {
	objects := resourceFixtures()
	oc := objects[2].(*v1.OidcConfig)
	oc.Spec.Type = v1.OidcConfigTypeManaged
	oc.Spec.IssuerUrl = "https://example.com/config"
	writes := 0
	hooks := interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		writes++
		return fmt.Errorf("unexpected OIDC write")
	}, Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		writes++
		return fmt.Errorf("unexpected create")
	}}
	srv, store := resourceServer(t, nil, hooks, objects...)
	w := resourceRequest(srv, "POST", "clusters", `{"metadata":{"name":"new"},"spec":{"oidcConfigId":"config-uid"}}`)
	if w.Code != 403 || writes != 0 {
		t.Fatalf("denied Create claimed OIDC: %d writes=%d %s", w.Code, writes, w.Body.String())
	}
	var stored v1.OidcConfig
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(oc), &stored); err != nil || stored.Labels[v1.ClaimedByClusterUIDLabel] != "" {
		t.Fatalf("claim changed: %+v %v", stored, err)
	}
}

// Create checks CreateCluster before any lookup, then Describe on each referenced
// object before claiming it, so a forbid on the reference holds.
func TestClusterCreateAuthorizesReferences(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		policies   []string
		want       int
	}{
		{"no create grant hides reference existence", `{"dnsReservationId":"missing"}`, nil, 403},
		{"create alone cannot claim OIDC config", `{"oidcConfigId":"config-uid"}`, []string{actionPermit("CreateCluster")}, 403},
		{"create alone cannot claim DNS reservation", `{"dnsReservationId":"zone-uid"}`, []string{actionPermit("CreateCluster")}, 403},
		{"describe on the OIDC config allows the claim", `{"oidcConfigId":"config-uid"}`, []string{actionPermit("CreateCluster"), actionPermit("DescribeOIDCConfig")}, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := resourceFixtures()
			oc := objects[2].(*v1.OidcConfig)
			oc.Spec.Type = v1.OidcConfigTypeManaged
			oc.Spec.IssuerUrl = "https://example.com/config"
			dns := objects[3].(*v1.DNSReservation)
			dns.Status.Phase, dns.Status.BaseDomain, dns.Status.InternalHostedClusterName = v1.DNSReservationPhaseReady, "example.com", "new"
			writes := 0
			hooks := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				writes++
				return c.Create(ctx, o, opts...)
			}}
			srv, _ := resourceServer(t, tc.policies, hooks, objects...)
			w := resourceRequest(srv, "POST", "clusters", `{"metadata":{"name":"new"},"spec":`+tc.spec+`}`)
			if w.Code != tc.want || (tc.want == 403 && writes != 0) {
				t.Fatalf("want %d (writes on deny 0), got %d writes=%d: %s", tc.want, w.Code, writes, w.Body.String())
			}
		})
	}
}

// Updates check the base action against the stored object before returning validation details.
func TestUpdateAuthorizesBeforeValidation(t *testing.T) {
	srv, _ := resourceServer(t, nil, interceptor.Funcs{}, resourceFixtures()...)
	w := resourceRequest(srv, "PATCH", "clusters/parent", `{"spec":{"accountId":"222222222222"}}`)
	if w.Code != 403 {
		t.Fatalf("caller without UpdateCluster got %d instead of 403: %s", w.Code, w.Body.String())
	}
}

// OIDC create checks CreateOIDCConfig before probing the global issuer index,
// so a caller without it cannot learn which issuers are already registered.
func TestOidcCreateAuthorizesBeforeIssuerProbe(t *testing.T) {
	taken := &v1.Index{ObjectMeta: metav1.ObjectMeta{Namespace: v1.OidcIssuerReservationsNamespace, Name: v1.IssuerURLIndexName("https://example.com")}}
	srv, _ := resourceServer(t, nil, interceptor.Funcs{}, append(resourceFixtures(), taken)...)
	w := resourceRequest(srv, "POST", "oidc_configs", `{"metadata":{"name":"new"},"spec":{"type":"unmanaged","issuerUrl":"https://example.com","secretArn":"secret","installerRoleArn":"role"}}`)
	if w.Code != 403 {
		t.Fatalf("caller without CreateOIDCConfig got %d instead of 403: %s", w.Code, w.Body.String())
	}
}

// NodePool Get loads its parent once; a stale parent binding still reads as not found.
func TestNodePoolGetLoadsParentOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		parentUID types.UID
		status    int
	}{{"owned", types.UID(resourceTestClusterID), 200}, {"stale parent", "old-uid", 404}} {
		t.Run(tc.name, func(t *testing.T) {
			objects := resourceFixtures()
			pool := objects[1].(*v1.NodePool)
			pool.OwnerReferences[0].UID = tc.parentUID
			pool.Labels[v1.ClusterUIDLabel] = string(tc.parentUID)
			reads := 0
			hooks := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isCluster := obj.(*v1.Cluster); isCluster {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			}}
			srv, _ := resourceServer(t, []string{actionPermit("DescribeNodePool")}, hooks, objects...)
			if w := resourceRequest(srv, "GET", "nodepools/parent.workers", ""); w.Code != tc.status || reads != 1 {
				t.Fatalf("status=%d parent reads=%d, want %d/1: %s", w.Code, reads, tc.status, w.Body.String())
			}
		})
	}
}
