package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func authzResourceRequest(t *testing.T, p *authzProcess, method, path, body, arn, operation string, want int) *APIResponse {
	t.Helper()
	before := authzMetrics(t, p)
	request, err := http.NewRequest(method, p.apiURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Amz-Account-Id", authzAccount)
	request.Header.Set("X-Amz-Caller-Arn", arn)
	request.Header.Set("Content-Type", "application/json")
	response := authzRawRequest(t, request)
	if response.StatusCode != want {
		t.Errorf("%s %s want %d got %d: %s", method, path, want, response.StatusCode, response.Body)
	}
	outcome := "allow"
	stage := ""
	if want == 403 || want == 404 {
		outcome = "deny"
	}
	if want == 500 {
		outcome = "error"
		stage = "evaluation"
	}
	after := authzMetrics(t, p)
	if want == http.StatusNotFound {
		// Account-filtered misses happen before authorization and record no sample.
		if !maps.Equal(before, after) {
			t.Fatal("storage miss recorded authorization metrics")
		}
	} else {
		authzMetricDelta(t, before, after, operation, outcome, stage)
	}
	if want >= 400 {
		authzAssertStatus(t, response, want)
	}
	return response
}

func authzResourceOperations(t *testing.T, p *authzProcess, store client.Client) {
	ctx := context.Background()
	blue := authzStoredClusters[1].name
	red := authzStoredClusters[0].name
	foreignCluster := authzStoredClusters[4].name
	pools := []*v1.NodePool{}
	for _, fixture := range []struct{ cluster, team, account string }{{red, "red", authzAccount}, {blue, "blue", authzAccount}, {foreignCluster, "blue", authzOtherAccount}} {
		var parent v1.Cluster
		if err := store.Get(ctx, client.ObjectKey{Namespace: "account-" + fixture.account, Name: fixture.cluster}, &parent); err != nil {
			t.Fatal(err)
		}
		replicas := int32(3)
		np := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{
			Name: fixture.cluster + ".workers", Namespace: parent.Namespace,
			Labels: map[string]string{
				authzAccountLabel: fixture.account, v1.ClusterUIDLabel: string(parent.UID),
			},
			Annotations:     map[string]string{authzTeamLabel: fixture.team},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&parent, v1.GroupVersion.WithKind("Cluster"))},
		}, Spec: v1.NodePoolSpec{AccountID: fixture.account}}
		np.Spec.NodePool.ClusterName = parent.Name
		np.Spec.NodePool.Replicas = &replicas
		if err := store.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
		pools = append(pools, np)
	}
	for _, fixture := range []struct{ name, team, account string }{{"a-config", "blue", authzAccount}, {"z-config", "red", authzAccount}, {"b-config", "blue", authzAccount}, {"c-config", "red", authzAccount}, {"d-config", "red", authzAccount}, {"a-config", "blue", authzOtherAccount}, {"foreign-only", "blue", authzOtherAccount}} {
		oc := &v1.OidcConfig{ObjectMeta: metav1.ObjectMeta{Name: fixture.name, Namespace: "account-" + fixture.account, Labels: map[string]string{authzAccountLabel: fixture.account}, Annotations: map[string]string{authzTeamLabel: fixture.team}}, Spec: v1.OidcConfigSpec{Type: v1.OidcConfigTypeManaged, AccountID: fixture.account, IssuerUrl: "https://example.com/" + fixture.name}}
		if err := store.Create(ctx, oc); err != nil {
			t.Fatal(err)
		}
		if fixture.account != authzAccount {
			continue
		}
		dns := &v1.DNSReservation{ObjectMeta: metav1.ObjectMeta{Name: fixture.name, Namespace: "account-" + authzAccount, Labels: map[string]string{authzAccountLabel: authzAccount}, Annotations: map[string]string{authzTeamLabel: fixture.team}}}
		if err := store.Create(ctx, dns); err != nil {
			t.Fatal(err)
		}
	}
	// Expand the existing fixtures for full pages, partial pages, and off-page failures.
	for i := 0; i < 3; i++ {
		np := pools[1].DeepCopy()
		np.Name, np.UID, np.ResourceVersion = blue+fmt.Sprintf(".extra-%d", i), "", ""
		np.Annotations = map[string]string{authzTeamLabel: "red"}
		if i == 0 {
			np.Annotations[authzTeamLabel] = "blue"
		}
		if err := store.Create(ctx, np); err != nil {
			t.Fatal(err)
		}
	}
	poolPath := "/api/v0/nodepools/" + blue + ".workers"
	clusterPath := authzClustersPath + "/" + blue
	var configA v1.OidcConfig
	if err := store.Get(ctx, client.ObjectKey{Namespace: "account-" + authzAccount, Name: "a-config"}, &configA); err != nil {
		t.Fatal(err)
	}
	// Exercise the real PostgreSQL label round trip through the retained HTTP fixture.
	for _, path := range []string{clusterPath, poolPath, "/api/v0/oidc_configs/a-config", "/api/v0/dns_reservations/a-config"} {
		t.Run("labels "+path, func(t *testing.T) {
			authzResourceRequest(t, p, "PATCH", path+"/labels", `{"example.com/check":"ok","absent":null}`, authzUser("broad"), "LabelResource", 403)
			response := authzResourceRequest(t, p, "PATCH", path+"/labels", `{"example.com/check":"ok","absent":null}`, authzUser("labels"), "LabelResource", 204)
			if len(response.Body) != 0 {
				t.Fatal("label mutation returned a body")
			}
			authzResourceRequest(t, p, "GET", path+"/labels", "", authzUser("labels"), "ListLabelsForResource", 403)
			response = authzResourceRequest(t, p, "GET", path+"/labels", "", authzUser("broad"), "ListLabelsForResource", 200)
			var labels map[string]string
			if err := json.Unmarshal(response.Body, &labels); err != nil || labels["example.com/check"] != "ok" {
				t.Fatalf("labels=%v error=%v", labels, err)
			}
			response = authzResourceRequest(t, p, "GET", path, "", authzUser("wildcard"), map[string]string{clusterPath: "DescribeCluster", poolPath: "DescribeNodePool", "/api/v0/oidc_configs/a-config": "DescribeOIDCConfig", "/api/v0/dns_reservations/a-config": "DescribeDNSReservation"}[path], 200)
			var object struct{ Metadata metav1.ObjectMeta }
			if err := json.Unmarshal(response.Body, &object); err != nil || object.Metadata.Annotations["example.com/check"] != "ok" {
				t.Fatalf("public tag round trip failed: %s %v", response.Body, err)
			}
			authzResourceRequest(t, p, "PATCH", path+"/labels", `{"example.com/check":null}`, authzUser("labels"), "UnlabelResource", 204)
		})
	}
	for _, tc := range []struct{ method, path, body, action string }{
		{"GET", authzClustersPath, "", "ListClusters"}, {"GET", clusterPath, "", "DescribeCluster"}, {"POST", authzClustersPath, `{"metadata":{"name":"denied-cluster"},"spec":{"oidcConfigId":"` + string(configA.UID) + `"}}`, "CreateCluster"}, {"PUT", clusterPath, `{"spec":{"properties":{"state":"denied"}}}`, "UpdateCluster"}, {"PATCH", clusterPath, `{"spec":{"properties":{"state":"denied"}}}`, "UpdateCluster"}, {"DELETE", clusterPath, "", "DeleteCluster"},
		{"GET", "/api/v0/nodepools", "", "ListNodePools"}, {"GET", poolPath, "", "DescribeNodePool"}, {"POST", "/api/v0/nodepools", `{"metadata":{"name":"` + blue + `.denied"},"spec":{"labels":{"state":"denied"}}}`, "CreateNodePool"}, {"PUT", poolPath, `{"spec":{"labels":{"state":"denied"}}}`, "UpdateNodePool"}, {"PATCH", poolPath, `{"spec":{"labels":{"state":"denied"}}}`, "UpdateNodePool"}, {"DELETE", poolPath, "", "DeleteNodePool"},
		{"GET", "/api/v0/oidc_configs", "", "ListOIDCConfigs"}, {"GET", "/api/v0/oidc_configs/a-config", "", "DescribeOIDCConfig"}, {"POST", "/api/v0/oidc_configs", `{"metadata":{"name":"denied-config"},"spec":{"type":"unmanaged","issuerUrl":"https://example.com/denied","secretArn":"secret","installerRoleArn":"role"}}`, "CreateOIDCConfig"}, {"DELETE", "/api/v0/oidc_configs/a-config", "", "DeleteOIDCConfig"},
		{"GET", "/api/v0/dns_reservations", "", "ListDNSReservations"}, {"GET", "/api/v0/dns_reservations/a-config", "", "DescribeDNSReservation"}, {"POST", "/api/v0/dns_reservations", `{"metadata":{"name":"denied-zone"}}`, "CreateDNSReservation"}, {"DELETE", "/api/v0/dns_reservations/a-config", "", "DeleteDNSReservation"},
	} {
		t.Run("default deny "+tc.action+tc.method, func(t *testing.T) {
			authzResourceRequest(t, p, tc.method, tc.path, tc.body, authzUser("bob"), tc.action, 403)
		})
	}
	var deniedOC v1.OidcConfig
	if err := store.Get(ctx, client.ObjectKey{Namespace: "account-" + authzAccount, Name: "a-config"}, &deniedOC); err != nil || deniedOC.Labels[v1.ClaimedByClusterUIDLabel] != "" {
		t.Fatalf("denied CreateCluster claimed OIDC: %+v %v", deniedOC, err)
	}

	for _, tc := range []struct{ path, list, describe, listOnly, describeOnly, scoped string }{
		{"nodepools", "ListNodePools", "DescribeNodePool", "resource-list-only", "resource-describe-only", "resource-scoped"},
		{"oidc_configs", "ListOIDCConfigs", "DescribeOIDCConfig", "resource-list-only", "resource-describe-only", "resource-scoped"},
		{"dns_reservations", "ListDNSReservations", "DescribeDNSReservation", "resource-list-only", "resource-describe-only", "resource-scoped"},
	} {
		t.Run("collection "+tc.path, func(t *testing.T) {
			// List counts all five fixtures, each in full; Describe is not checked per item.
			for _, check := range []struct {
				principal        string
				detailed, status int
			}{{tc.listOnly, 5, 200}, {tc.describeOnly, 0, 403}, {tc.scoped, 5, 200}} {
				response := authzResourceRequest(t, p, "GET", "/api/v0/"+tc.path, "", authzUser(check.principal), tc.list, check.status)
				if check.status == 200 {
					var body struct {
						Total int
						Items []json.RawMessage
					}
					err := json.Unmarshal(response.Body, &body)
					detailed := 0
					for _, item := range body.Items {
						if strings.Contains(string(item), authzTeamLabel) {
							detailed++
						}
					}
					if err != nil || body.Total != 5 || len(body.Items) != 5 || detailed != check.detailed {
						t.Errorf("wrong list details: %s %v", response.Body, err)
					}
				}
			}
			for _, offset := range []int{0, 2, 4, 5, 9} {
				for _, principal := range []string{tc.scoped, "wildcard"} {
					response := authzResourceRequest(t, p, "GET", fmt.Sprintf("/api/v0/%s?limit=2&offset=%d", tc.path, offset), "", authzUser(principal), tc.list, 200)
					var page struct {
						Total, Limit, Offset int
						Items                []struct{ Metadata metav1.ObjectMeta }
					}
					if err := json.Unmarshal(response.Body, &page); err != nil {
						t.Fatal(err)
					}
					want := min(2, max(0, 5-offset))
					if page.Total != 5 || len(page.Items) != want || page.Limit != 2 || page.Offset != offset {
						t.Fatalf("wrong page: %s", response.Body)
					}
					for _, item := range page.Items {
						isBlue := item.Metadata.Name == "a-config" || item.Metadata.Name == "b-config" || item.Metadata.Name == blue+".workers" || item.Metadata.Name == blue+".extra-0"
						hasDetails := len(item.Metadata.Labels) != 0
						if item.Metadata.Namespace != "account-"+authzAccount || !hasDetails || (isBlue && item.Metadata.Annotations[authzTeamLabel] != "blue") {
							t.Fatalf("wrong page ownership/summary/detail: %+v", item)
						}
					}
				}
			}
		})
	}
	// The qualified name selects the pool; clusterId cannot redirect Describe to another parent.
	authzResourceRequest(t, p, "GET", poolPath, "", authzUser("resource-scoped"), "DescribeNodePool", 200)
	authzResourceRequest(t, p, "GET", "/api/v0/nodepools/"+red+".workers", "", authzUser("resource-scoped"), "DescribeNodePool", 403)
	authzResourceRequest(t, p, "GET", "/api/v0/nodepools/"+red+".workers?clusterId="+authzStoredClusters[1].id, "", authzUser("resource-scoped"), "DescribeNodePool", 403)

	for _, parent := range []struct {
		name   string
		status int
	}{{blue, 200}, {red, 403}} {
		authzResourceRequest(t, p, "GET", "/api/v0/nodepools/"+parent.name+".workers", "", authzUser("parent-scoped"), "DescribeNodePool", parent.status)
	}
	t.Run("wildcard account isolation", func(t *testing.T) {
		missingPool := pools[1].DeepCopy()
		missingPool.Name, missingPool.UID, missingPool.ResourceVersion = blue+".missing-tag", "", ""
		delete(missingPool.Annotations, authzTeamLabel)
		missingOIDC := &v1.OidcConfig{ObjectMeta: metav1.ObjectMeta{Name: "missing-tag", Namespace: missingPool.Namespace, Labels: map[string]string{authzAccountLabel: authzAccount}}}
		for _, obj := range []client.Object{missingPool, missingOIDC} {
			if err := store.Create(ctx, obj); err != nil {
				t.Fatal(err)
			}
			if err := store.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Delete(ctx, obj); err != nil {
					t.Error(err)
				}
			})
		}
		wildcard := authzUser("wildcard")
		// Request labels cannot fill in a missing stored tag.
		for _, missing := range []struct {
			path, action string
		}{{"nodepools/" + missingPool.Name, "DescribeNodePool"}, {"oidc_configs/missing-tag", "DescribeOIDCConfig"}} {
			authzResourceRequest(t, p, "GET", "/api/v0/"+missing.path, `{"metadata":{"labels":{"example.com/team":"blue"}}}`, authzUser("resource-scoped"), missing.action, 403)
		}
		for _, tc := range []struct {
			path, action, body string
			object             client.Object
		}{
			{"nodepools/" + pools[2].Name, "NodePool", `{"spec":{"labels":{"state":"foreign-change"}}}`, pools[2]},
			{"oidc_configs/foreign-only", "OIDCConfig", "", &v1.OidcConfig{ObjectMeta: metav1.ObjectMeta{Name: "foreign-only", Namespace: "account-" + authzOtherAccount}}},
		} {
			if err := store.Get(ctx, client.ObjectKeyFromObject(tc.object), tc.object); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{"GET", "PUT", "PATCH", "DELETE"} {
				if (method == "PUT" || method == "PATCH") && tc.action == "OIDCConfig" {
					continue
				} // OIDC has no update route.
				action := map[string]string{"GET": "Describe", "PUT": "Update", "PATCH": "Update", "DELETE": "Delete"}[method] + tc.action
				authzResourceRequest(t, p, method, "/api/v0/"+tc.path, tc.body, wildcard, action, 404)
				saved := tc.object.DeepCopyObject().(client.Object)
				if err := store.Get(ctx, client.ObjectKeyFromObject(saved), saved); err != nil || !reflect.DeepEqual(saved, tc.object) {
					t.Fatalf("foreign object changed: %v / %v", saved, err)
				}
			}
		}
		// Equal names must select the account-local incarnation.
		response := authzResourceRequest(t, p, "GET", "/api/v0/oidc_configs/a-config", "", wildcard, "DescribeOIDCConfig", 200)
		var namesake publicResourceID
		if err := json.Unmarshal(response.Body, &namesake); err != nil || namesake.Metadata.UID != string(configA.UID) {
			t.Fatalf("wrong OIDC namesake: %s %v", response.Body, err)
		}
		// Contradictory stored ownership fails closed locally and is invisible to the other account.
		missingPool.Labels[authzAccountLabel] = authzOtherAccount
		if err := store.Update(ctx, missingPool); err != nil {
			t.Fatal(err)
		}
		for _, account := range []string{authzAccount, authzOtherAccount} {
			requester := NewAPIClient(p.apiURL)
			requester.CallerARN = "arn:aws:iam::" + account + ":user/wildcard"
			before := authzMetrics(t, p)
			response, err := requester.Get("/api/v0/nodepools/"+missingPool.Name, account)
			if err != nil {
				t.Fatal(err)
			}
			if account == authzAccount {
				authzAssertStatus(t, response, http.StatusInternalServerError)
				authzMetricDelta(t, before, authzMetrics(t, p), "DescribeNodePool", "error", "entity_validation")
			} else {
				authzAssertStatus(t, response, http.StatusNotFound)
			}
		}
		authzResourceRequest(t, p, "POST", "/api/v0/nodepools", fmt.Sprintf(`{"metadata":{"name":%q}}`, foreignCluster+".new"), wildcard, "", 404)
		var created v1.NodePool
		if err := store.Get(ctx, client.ObjectKey{Namespace: "account-" + authzAccount, Name: foreignCluster + ".new"}, &created); err == nil {
			t.Fatal("pool created under foreign parent")
		}
	})
	// Each specialized update and the mandatory base must allow; omitted replicas remain unchanged.
	for _, principal := range []string{"base-updater", "special-updater"} {
		authzResourceRequest(t, p, "PUT", clusterPath, `{"spec":{"hostedCluster":{"release":{"image":"new"}}}}`, authzUser(principal), "UpdateCluster", 403)
		authzResourceRequest(t, p, "PATCH", clusterPath, `{"spec":{"hostedCluster":{"release":{"image":"new"}}}}`, authzUser(principal), "UpdateCluster", 403)
		for _, method := range []string{"PUT", "PATCH"} {
			authzResourceRequest(t, p, method, poolPath, `{"spec":{"nodePool":{"replicas":0,"release":{"image":"new"}}}}`, authzUser(principal), "UpdateNodePool", 403)
		}
	}
	lifecycle := authzUser("lifecycle")
	authzResourceRequest(t, p, "PATCH", poolPath, `{"spec":{"labels":{"state":"changed"}}}`, lifecycle, "UpdateNodePool", 200)
	var saved v1.NodePool
	if err := store.Get(ctx, client.ObjectKeyFromObject(pools[1]), &saved); err != nil || saved.Spec.NodePool.Replicas == nil || *saved.Spec.NodePool.Replicas != 3 {
		t.Fatalf("omitted replicas changed: %+v %v", saved, err)
	}
	authzResourceRequest(t, p, "PUT", poolPath, `{"spec":{"nodePool":{"replicas":0,"release":{"image":"new"}}}}`, lifecycle, "UpdateNodePool", 200)
	authzResourceRequest(t, p, "PUT", clusterPath, `{"spec":{"properties":{"state":"changed"}}}`, lifecycle, "UpdateCluster", 200)
	authzResourceRequest(t, p, "PATCH", clusterPath, `{"spec":{"hostedCluster":{"release":{"image":"new"}}}}`, lifecycle, "UpdateCluster", 200)

	// Actual HTTP create/get/delete for all customer families and global service registration.
	createCluster := authzResourceRequest(t, p, "POST", authzClustersPath, `{"metadata":{"name":"http-created","annotations":{"example.com/team":"blue"}},"spec":{"properties":{"state":"created"}}}`, lifecycle, "CreateCluster", 201)
	var cluster publicResourceID
	_ = json.Unmarshal(createCluster.Body, &cluster)
	if cluster.Metadata.UID == "" {
		t.Fatal("create lacked UID")
	}
	authzResourceRequest(t, p, "GET", authzClustersPath+"/http-created", "", lifecycle, "DescribeCluster", 200)
	authzResourceRequest(t, p, "POST", "/api/v0/nodepools", `{"metadata":{"name":"http-created.pool","annotations":{"example.com/team":"blue","seaplane.io/account-id":"222222222222"}},"spec":{"labels":{"state":"created"}}}`, lifecycle, "CreateNodePool", 201)
	authzResourceRequest(t, p, "GET", "/api/v0/nodepools/http-created.pool", "", lifecycle, "DescribeNodePool", 200)
	authzAssertLabels(t, store, &v1.NodePool{}, "http-created.pool")
	authzAssertLabels(t, store, &v1.Cluster{}, "http-created")
	authzResourceRequest(t, p, "DELETE", "/api/v0/nodepools/http-created.pool", "", lifecycle, "DeleteNodePool", 202)
	authzResourceRequest(t, p, "DELETE", authzClustersPath+"/http-created", "", lifecycle, "DeleteCluster", 202)
	createOC := authzResourceRequest(t, p, "POST", "/api/v0/oidc_configs", `{"metadata":{"name":"http-created","annotations":{"example.com/team":"blue","seaplane.io/claimed-by-cluster-uid":"forged"}},"spec":{"type":"unmanaged","issuerUrl":"https://example.com/http-created","secretArn":"secret","installerRoleArn":"role"}}`, lifecycle, "CreateOIDCConfig", 201)
	var config publicResourceID
	if err := json.Unmarshal(createOC.Body, &config); err != nil || config.Metadata.UID == "" {
		t.Fatalf("OIDC create lacked UID: %s %v", createOC.Body, err)
	}
	authzResourceRequest(t, p, "GET", "/api/v0/oidc_configs/http-created", "", lifecycle, "DescribeOIDCConfig", 200)
	authzAssertLabels(t, store, &v1.OidcConfig{}, "http-created")
	authzResourceRequest(t, p, "DELETE", "/api/v0/oidc_configs/http-created", "", lifecycle, "DeleteOIDCConfig", 202)
	authzResourceRequest(t, p, "POST", "/api/v0/dns_reservations", `{"metadata":{"name":"http-created","annotations":{"example.com/team":"blue","seaplane.io/claimed-by-cluster-uid":"forged"}}}`, lifecycle, "CreateDNSReservation", 201)
	authzResourceRequest(t, p, "GET", "/api/v0/dns_reservations/http-created", "", lifecycle, "DescribeDNSReservation", 200)
	authzAssertLabels(t, store, &v1.DNSReservation{}, "http-created")
	authzResourceRequest(t, p, "DELETE", "/api/v0/dns_reservations/http-created", "", lifecycle, "DeleteDNSReservation", 202)

	// Six context fields are available, and forwarding headers cannot impersonate the trusted IP.
	for _, source := range []string{"192.0.2.50", ""} {
		req, err := http.NewRequest("GET", p.apiURL+clusterPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Account-Id", authzAccount)
		req.Header.Set("X-Amz-Caller-Arn", authzUser("context"))
		req.Header.Set("X-Amz-Source-Ip", source)
		req.Header.Set("X-Forwarded-For", "192.0.2.50")
		req.Header.Set("User-Agent", "local-authz-http")
		response := authzRawRequest(t, req)
		want := 200
		if source == "" {
			want = 403
		}
		if response.StatusCode != want {
			t.Errorf("trusted context want %d got %d: %s", want, response.StatusCode, response.Body)
		}
	}
}

// Stored ownership and labels must survive candidate validation for every customer kind.
func authzAssertLabels(t *testing.T, store client.Client, obj client.Object, name string) {
	t.Helper()
	if err := store.Get(context.Background(), client.ObjectKey{Namespace: "account-" + authzAccount, Name: name}, obj); err != nil {
		t.Fatal(err)
	}
	if obj.GetLabels()[authzAccountLabel] != authzAccount || obj.GetAnnotations()[authzTeamLabel] != "blue" {
		t.Fatalf("create lost trusted labels or tags: %v %v", obj.GetLabels(), obj.GetAnnotations())
	}
}

type publicResourceID struct{ Metadata struct{ UID string } }
