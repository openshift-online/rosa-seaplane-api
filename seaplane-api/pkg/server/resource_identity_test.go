package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Create checks precede UID assignment; subsequent checks identify the stored incarnation.
func TestCustomerResourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		kind, path, name, body, createID, storedID string
	}{
		{
			kind: "Cluster", path: "clusters", name: "new", createID: "new", storedID: "database-uid",
			body: `{"metadata":{"name":"new","uid":"database-uid"},"spec":{"hostedCluster":{"release":{"image":"` + resourceTestReleaseImage + `"},"networking":{},"platform":{"type":"AWS"}}}}`,
		},
		{
			kind: "NodePool", path: "nodepools", name: "parent.new",
			createID: resourceTestClusterID + "/parent.new", storedID: resourceTestClusterID + "/database-uid",
			body: `{"metadata":{"name":"parent.new","uid":"database-uid"},"spec":{"nodePool":{"replicas":1}}}`,
		},
		{
			kind: "OIDCConfig", path: "oidc_configs", name: "new", createID: "new", storedID: "database-uid",
			body: `{"metadata":{"name":"new","uid":"database-uid"},"spec":{"type":"unmanaged","issuerUrl":"https://example.com/new","secretArn":"secret","installerRoleArn":"role"}}`,
		},
	} {
		for _, grant := range []string{"candidate name", "future UID"} {
			t.Run(tc.kind+"/"+grant, func(t *testing.T) {
				createID := tc.createID
				if grant == "future UID" {
					createID = tc.storedID
				}
				policy := func(action, id string) string {
					return fmt.Sprintf(`permit(principal, action == Seaplane::Action::"%s%s", resource == Seaplane::%s::"123456789012/us-east-1/%s");`, action, tc.kind, tc.kind, id)
				}
				writes := 0
				srv, _ := resourceServer(t, []string{policy("Create", createID), policy("Describe", tc.storedID)}, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						writes++
						obj.SetUID("database-uid")
						return c.Create(ctx, obj, opts...)
					},
				}, resourceFixtures()...)
				response := resourceRequest(srv, http.MethodPost, tc.path, tc.body)
				if grant == "future UID" {
					if response.Code != http.StatusForbidden || writes != 0 {
						t.Fatalf("UID supplied by the caller authorized creation: status=%d writes=%d body=%s", response.Code, writes, response.Body.String())
					}
					return
				}
				if response.Code != http.StatusCreated || writes != 1 {
					t.Fatalf("name-authorized create: status=%d writes=%d body=%s", response.Code, writes, response.Body.String())
				}
				response = resourceRequest(srv, http.MethodGet, tc.path+"/"+tc.name, "")
				if response.Code != http.StatusOK {
					t.Fatalf("stored-UID-authorized read: status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}
