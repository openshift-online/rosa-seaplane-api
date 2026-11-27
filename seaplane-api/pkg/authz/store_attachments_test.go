//go:build integration

package authz_test

import (
	"errors"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredAttachments(t *testing.T) {
	east, west, _, _ := localStores(t)
	ctx := t.Context()
	read, err := east.CreatePolicy(ctx, "read", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	spec := authz.AttachmentSpec{PrincipalARN: "arn:aws:iam::111111111111:user/alice", Policy: authz.PolicyRef{ID: read.ID, Kind: read.Kind}, Scope: authz.GlobalScope}
	global, err := east.CreateAttachment(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	westGlobal, err := west.CreateAttachment(ctx, spec)
	if err != nil {
		t.Fatalf("global attachment create = %v", err)
	}
	if err := west.DeleteAttachment(ctx, authz.GlobalScope, westGlobal.ID, westGlobal.Revision); err != nil {
		t.Fatalf("global attachment delete = %v", err)
	}
	spec.Scope = authz.RegionalScope
	eastOnly, err := east.CreateAttachment(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	westOnly, err := west.CreateAttachment(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		store *authz.Store
		local string
	}{{east, eastOnly.ID}, {west, westOnly.ID}} {
		bindings, err := scenario.store.ListAttachments(ctx)
		if err != nil || len(bindings) != 2 {
			t.Fatalf("list = %+v, %v", bindings, err)
		}
		ids := map[string]bool{}
		for _, binding := range bindings {
			ids[binding.ID] = true
		}
		if !ids[global.ID] || !ids[scenario.local] {
			t.Fatalf("wrong scope union: %+v", bindings)
		}
	}
	if err := east.DeleteAttachment(ctx, authz.GlobalScope, global.ID, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	if err := east.DeleteAttachment(ctx, authz.GlobalScope, global.ID, global.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := east.GetAttachment(ctx, authz.GlobalScope, global.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("deleted = %v", err)
	}
	spec.PrincipalARN = "arn:aws:iam::222222222222:user/other"
	if _, err := east.CreateAttachment(ctx, spec); err == nil {
		t.Fatal("foreign principal accepted")
	}
	policies, err := east.ListPolicies(ctx)
	if err != nil || len(policies) != 1 {
		t.Fatalf("policies = %+v, %v", policies, err)
	}
}
