package server

import (
	"context"
	"slices"
	"testing"

	v1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPersistentRetryFreezesGrants(t *testing.T) {
	for _, team := range []string{"blue", "red"} {
		t.Run(team, func(t *testing.T) {
			objects := resourceFixtures()
			key := client.ObjectKeyFromObject(objects[0])
			var management *authz.Service
			admin := authztest.Admin(resourceTestAccount)
			writes := 0
			var retryVersion string
			hooks := interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes++
				if writes > 1 {
					if obj.GetResourceVersion() != retryVersion || obj.GetAnnotations()["competitor"] != "wrote" {
						t.Fatalf("retry used stale resource facts: version=%q tags=%v", obj.GetResourceVersion(), obj.GetAnnotations())
					}
					return c.Update(ctx, obj, opts...)
				}
				attachments, err := management.ListAttachments(ctx, admin, authz.GlobalScope)
				i := slices.IndexFunc(attachments, func(a authz.StoredAttachment) bool { return a.Policy.ID == "grant-0" })
				if err != nil || i < 0 {
					t.Fatalf("attachments=%v err=%v", attachments, err)
				}
				attachment := attachments[i]
				if _, err := management.DeleteAttachment(ctx, admin, attachment.Scope, attachment.ID, attachment.Revision); err != nil {
					t.Fatal(err)
				}
				var competitor v1.Cluster
				if err := c.Get(ctx, key, &competitor); err != nil {
					return err
				}
				competitor.Annotations["team"], competitor.Annotations["competitor"] = team, "wrote"
				if err := c.Update(ctx, &competitor); err != nil {
					return err
				}
				retryVersion = competitor.ResourceVersion
				return apierrors.NewConflict(v1.GroupVersion.WithResource("clusters").GroupResource(), obj.GetName(), nil)
			}}
			policy := `permit(principal, action == Seaplane::Action::"UpdateCluster", resource) when { resource.hasTag("team") && resource.getTag("team") == "blue" };`
			srv, store, grants := resourceServerWithManagement(t, []string{policy}, hooks, objects...)
			management = grants
			const body = `{"spec":{"properties":{"change":"requested"}}}`
			before := authzMetricSamples(t)
			w := resourceRequest(srv, "PUT", "clusters/parent", body)
			want, wantWrites, outcome := 200, 2, "allow"
			if team == "red" {
				want, wantWrites, outcome = 403, 1, "deny"
			}
			if w.Code != want || writes != wantWrites {
				t.Fatalf("status=%d writes=%d want=%d/%d body=%s", w.Code, writes, want, wantWrites, w.Body)
			}
			assertAuthzOutcome(t, before, authzMetricSamples(t), "UpdateCluster", outcome)
			var saved v1.Cluster
			if err := store.Get(t.Context(), key, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Annotations["team"] != team || saved.Annotations["competitor"] != "wrote" || (saved.Spec.Properties["change"] == "requested") != (team == "blue") {
				t.Fatalf("retry lost checked state: tags=%v properties=%v", saved.Annotations, saved.Spec.Properties)
			}
			before = authzMetricSamples(t)
			w = resourceRequest(srv, "PUT", "clusters/parent", body)
			if w.Code != 403 || writes != wantWrites {
				t.Fatalf("revoked grant survived into next request: status=%d writes=%d body=%s", w.Code, writes, w.Body)
			}
			assertAuthzOutcome(t, before, authzMetricSamples(t), "UpdateCluster", "deny")
			var after v1.Cluster
			if err := store.Get(t.Context(), key, &after); err != nil || after.ResourceVersion != saved.ResourceVersion {
				t.Fatalf("denied request changed resource: version=%q want=%q err=%v", after.ResourceVersion, saved.ResourceVersion, err)
			}
		})
	}
}
