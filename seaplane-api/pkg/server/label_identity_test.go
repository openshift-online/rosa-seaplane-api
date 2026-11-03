package server

import (
	"context"
	"testing"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestLabelSnapshotIdentity(t *testing.T) {
	for _, route := range []string{"clusters/parent", "nodepools/parent.workers", "oidc_configs/config", "dns_reservations/zone"} {
		for _, method := range []string{"GET", "PATCH"} {
			t.Run(route+method, func(t *testing.T) {
				objects := resourceFixtures()
				for _, obj := range objects {
					obj.SetNamespace("account-222222222222")
				}
				srv, _ := resourceServer(t, []string{`permit(principal,action,resource);`}, interceptor.Funcs{}, objects...)
				if w := resourceRequest(srv, method, route+"/labels", `{"team":"red"}`); w.Code != 404 {
					t.Fatalf("foreign resource: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	for _, state := range []string{"missing parent", "replaced parent", "wrong parent label", "replacement UID", "deleted during write", "self revoke", "cannot escalate"} {
		t.Run(state, func(t *testing.T) {
			objects := resourceFixtures()
			path := "nodepools/parent.workers/labels"
			writes := 0
			hooks := interceptor.Funcs{}
			policy := `permit(principal,action in Seaplane::Action::"LabelAdmin",resource) when {resource.hasTag("team") && resource.getTag("team")=="blue"};`
			body := `{"team":"red"}`
			want := 404
			switch state {
			case "missing parent":
				objects = objects[1:]
			case "replaced parent":
				objects[0].SetUID("replacement")
			case "wrong parent label":
				objects[1].GetLabels()[v1.ClusterUIDLabel] = "forged"
			case "cannot escalate":
				objects[1].GetAnnotations()["team"] = "red"
				body = `{"team":"blue"}`
				want = 403
			case "self revoke":
				want = 204
			case "deleted during write":
				want = 409
				hooks.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					if err := c.Delete(ctx, obj); err != nil {
						return err
					}
					return c.Update(ctx, obj, opts...)
				}
			case "replacement UID":
				want = 409
				hooks.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					replacement := obj.DeepCopyObject().(client.Object)
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), replacement); err != nil {
						return err
					}
					replacement.SetUID("replacement")
					if err := c.Update(ctx, replacement); err != nil {
						return err
					}
					return apierrors.NewConflict(v1.GroupVersion.WithResource("nodepools").GroupResource(), obj.GetName(), nil)
				}
			}
			srv, store := resourceServer(t, []string{policy}, hooks, objects...)
			w := resourceRequest(srv, "PATCH", path, body)
			if w.Code != want {
				t.Fatalf("status=%d want %d body=%s", w.Code, want, w.Body.String())
			}
			if (state == "replacement UID" || state == "deleted during write") && writes != 1 {
				t.Fatalf("retargeted writes=%d", writes)
			}
			if state == "self revoke" {
				w = resourceRequest(srv, "PATCH", path, `{"team":"blue"}`)
				if w.Code != 403 {
					t.Fatalf("post-change access not revoked: %d", w.Code)
				}
				var np v1.NodePool
				if err := store.Get(context.Background(), client.ObjectKeyFromObject(objects[1]), &np); err != nil || np.Annotations["team"] != "red" {
					t.Fatalf("tags=%v error=%v", np.Annotations, err)
				}
			}
		})
	}
}
