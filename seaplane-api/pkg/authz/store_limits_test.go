//go:build integration

package authz_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredPaginationAndBatches(t *testing.T) {
	store, _, _, _ := localStores(t)
	caller := "arn:aws:iam::111111111111:user/alice"
	// More than 100 distinct references crosses both query and BatchGet limits.
	for i := range 101 {
		content := `permit(principal == ?principal, action, resource);`
		if i == 100 {
			content = `forbid(principal == ?principal, action, resource);`
		}
		policy, err := store.CreatePolicy(t.Context(), fmt.Sprintf("policy-%03d", i), content)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: caller, Scope: authz.GlobalScope}); err != nil {
			t.Fatal(err)
		}
	}
	policies, err := store.ListPolicies(t.Context())
	if err != nil || len(policies) != 101 {
		t.Fatalf("policies = %d, %v", len(policies), err)
	}
	bindings, err := store.ListAttachments(t.Context())
	if err != nil || len(bindings) != 101 {
		t.Fatalf("attachments = %d, %v", len(bindings), err)
	}
	id := authz.Identity{AccountID: account, CallerARN: caller}
	resolved, err := store.Resolve(t.Context(), id)
	if err != nil || len(resolved) != 101 {
		t.Fatalf("resolve = %d, %v", len(resolved), err)
	}
	prepared, err := store.Prepare(t.Context(), id, authz.RequestContext{RequestTime: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := prepared.Check(t.Context(), authz.DescribeCluster, authz.Resource{Kind: authz.Cluster, ID: "one", AccountID: account})
	if err != nil || decision.Allowed || len(decision.Provenance) != 1 || decision.Provenance[0].PolicyID != "policy-100" {
		t.Fatalf("lost forbid: %+v, %v", decision, err)
	}
}
