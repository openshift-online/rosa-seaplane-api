//go:build integration

package authz_test

import (
	"errors"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// TestStoredTemplates roundtrips supported templates and rejects invalid policies with ErrValidation.
func TestStoredTemplates(t *testing.T) {
	store, _, _, _ := localStores(t)
	for _, content := range []string{
		`permit(principal in ?principal, action in Seaplane::Action::"ReadOnly", resource);`,
		`permit(principal in ?principal, action in Seaplane::Action::"RegionalAttachmentAdmin", resource);`,
		`// leading comment
		@description("?principal is literal here")
		forbid(principal == ?principal, action == Seaplane::Action::"DeleteCluster", resource);`,
		`permit(principal, action, resource) when { context.userAgent == "?principal" };`,
	} {
		policy, err := store.CreatePolicy(t.Context(), "template", content)
		if err != nil {
			t.Fatalf("template %q: %v", content, err)
		}
		got, err := store.GetPolicy(t.Context(), authz.PolicyRef{ID: policy.ID, Kind: policy.Kind})
		if err != nil || got.Content != content {
			t.Fatalf("roundtrip = %+v, %v", got, err)
		}
		if err := store.DeletePolicy(t.Context(), policy.ID, policy.Revision); err != nil {
			t.Fatal(err)
		}
	}
	for _, content := range []string{
		`permit(?principal, action, resource);`,
		`permit(principal in ?resource, action, resource);`,
		`permit(principal, action, resource == ?resource);`,
		`permit(principal in ?principal, action, resource) when { ?principal == principal };`,
		`permit(principal in ?principal, action, resource); forbid(principal, action, resource);`,
		`permit(principal in ?principal, action == Seaplane::Action::"UnknownAction", resource);`,
		`permit(principal, action == Seaplane::Action::"Missing", resource);`,
	} {
		if _, err := store.CreatePolicy(t.Context(), "invalid", content); !errors.Is(err, authz.ErrValidation) {
			t.Fatalf("invalid store policy %q = %v", content, err)
		}
	}
}
