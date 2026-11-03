//go:build integration

package authz_test

import (
	"errors"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredCatalog(t *testing.T) {
	store, _, client, cfg := localStores(t)
	ctx := t.Context()
	managed, err := authz.PublishManagedPolicy(ctx, client, cfg.PolicyTable, "read", `permit(principal in ?principal, action in Seaplane::Action::"ReadOnly", resource);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authz.PublishManagedPolicy(ctx, client, cfg.PolicyTable, "read", managed.Content); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("catalog replacement = %v", err)
	}
	if _, err := store.UpdatePolicy(ctx, managed.ID, managed.Revision, `forbid(principal, action, resource);`); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("managed update = %v", err)
	}
	if err := store.DeletePolicy(ctx, managed.ID, managed.Revision); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("managed delete = %v", err)
	}
	got, err := store.GetPolicy(ctx, authz.PolicyRef{ID: "read", Kind: authz.ManagedPolicy})
	if err != nil || got != managed {
		t.Fatalf("catalog changed: %+v, %v", got, err)
	}
	bindings, err := store.CreateAttachment(ctx, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: "read", Kind: authz.ManagedPolicy}, PrincipalARN: "arn:aws:iam::111111111111:user/alice", Scope: authz.GlobalScope})
	if err != nil || bindings.Policy.Kind != authz.ManagedPolicy {
		t.Fatalf("catalog attachment = %+v, %v", bindings, err)
	}
	policies, err := store.ListPolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].Kind != authz.ManagedPolicy {
		t.Fatalf("catalog list = %+v, %v", policies, err)
	}
}
