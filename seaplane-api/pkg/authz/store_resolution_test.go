//go:build integration

package authz_test

import (
	"errors"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredDecisions(t *testing.T) {
	east, west, _, _ := localStores(t)
	ctx := t.Context()
	read, err := east.CreatePolicy(ctx, "read", `permit(principal in ?principal, action in Seaplane::Action::"ReadOnly", resource);`)
	if err != nil {
		t.Fatal(err)
	}
	deny, err := east.CreatePolicy(ctx, "deny", `forbid(principal == ?principal, action == Seaplane::Action::"DescribeCluster", resource);`)
	if err != nil {
		t.Fatal(err)
	}
	role := "arn:aws:iam::111111111111:role/team/developer"
	session := "arn:aws:sts::111111111111:assumed-role/developer/alice"
	roleBinding, err := east.CreateAttachment(ctx, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: read.ID, Kind: read.Kind}, PrincipalARN: role, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	sessionBinding, err := east.CreateAttachment(ctx, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: deny.ID, Kind: deny.Kind}, PrincipalARN: session, Scope: authz.RegionalScope})
	if err != nil {
		t.Fatal(err)
	}
	request := authz.RequestContext{RequestTime: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	resource := authz.Resource{Kind: authz.Cluster, ID: "cluster-one", AccountID: account}
	for _, scenario := range []struct {
		store   *authz.Store
		caller  string
		allowed bool
	}{{east, session, false}, {west, session, true}, {east, "arn:aws:sts::111111111111:assumed-role/developer/bob", true}, {east, "arn:aws:iam::111111111111:user/unattached", false}} {
		prepared, err := scenario.store.Prepare(ctx, authz.Identity{AccountID: account, CallerARN: scenario.caller}, request)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := prepared.Check(ctx, authz.DescribeCluster, resource)
		if err != nil || decision.Allowed != scenario.allowed {
			t.Fatalf("%s: %+v, %v", scenario.caller, decision, err)
		}
	}
	resolved, err := east.Resolve(ctx, authz.Identity{AccountID: account, CallerARN: session})
	if err != nil || len(resolved) != 2 {
		t.Fatalf("resolve = %+v, %v", resolved, err)
	}
	if err := east.DeleteAttachment(ctx, authz.RegionalScope, sessionBinding.ID, sessionBinding.Revision); err != nil {
		t.Fatal(err)
	}
	resolved, err = east.Resolve(ctx, authz.Identity{AccountID: account, CallerARN: session})
	if err != nil || len(resolved) != 1 || resolved[0].Attachment.ID != roleBinding.ID {
		t.Fatalf("lookup row delete = %+v, %v", resolved, err)
	}
	if err := east.DeletePolicy(ctx, read.ID, read.Revision); err != nil {
		t.Fatal(err)
	}
	resolved, err = east.Resolve(ctx, authz.Identity{AccountID: account, CallerARN: session})
	if !errors.Is(err, authz.ErrNotFound) || resolved != nil {
		t.Fatalf("missing policy returned partial result: %+v, %v", resolved, err)
	}
}
