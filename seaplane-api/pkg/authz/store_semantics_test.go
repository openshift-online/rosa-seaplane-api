//go:build integration

package authz_test

import (
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredBindingSemantics(t *testing.T) {
	for _, scenario := range []struct {
		name, content, principal, caller, team, userAgent string
		allowed                                           bool
	}{
		{"role membership", `permit(principal in ?principal, action == Seaplane::Action::"DescribeCluster", resource);`, "arn:aws:iam::111111111111:role/developer", "arn:aws:sts::111111111111:assumed-role/developer/alice", "", "", true},
		{"role equality stays equality", `permit(principal == ?principal, action == Seaplane::Action::"DescribeCluster", resource);`, "arn:aws:iam::111111111111:role/developer", "arn:aws:sts::111111111111:assumed-role/developer/alice", "", "", false},
		{"user equality", `permit(principal == ?principal, action == Seaplane::Action::"DescribeCluster", resource);`, "arn:aws:iam::111111111111:user/alice", "arn:aws:iam::111111111111:user/alice", "", "", true},
		{"conditions preserved", `@note("?principal") permit(principal in ?principal, action == Seaplane::Action::"DescribeCluster", resource) when { resource.hasTag("Team") && resource.getTag("Team") == "blue\"team" } unless { context.userAgent == "blocked" };`, "arn:aws:iam::111111111111:role/developer", "arn:aws:sts::111111111111:assumed-role/developer/alice", "blue\"team", "", true},
		{"unless still denies", `permit(principal in ?principal, action == Seaplane::Action::"DescribeCluster", resource) unless { context.userAgent == "blocked" };`, "arn:aws:iam::111111111111:role/developer", "arn:aws:sts::111111111111:assumed-role/developer/alice", "", "blocked", false},
		{"conditions still deny", `permit(principal in ?principal, action == Seaplane::Action::"DescribeCluster", resource) when { resource.hasTag("Team") && resource.getTag("Team") == "blue" };`, "arn:aws:iam::111111111111:role/developer", "arn:aws:sts::111111111111:assumed-role/developer/alice", "red", "", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, _, _, _ := localStores(t)
			policy, err := store.CreatePolicy(t.Context(), "policy", scenario.content)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: scenario.principal, Scope: authz.GlobalScope}); err != nil {
				t.Fatal(err)
			}
			prepared, err := store.Prepare(t.Context(), authz.Identity{AccountID: account, CallerARN: scenario.caller}, authz.RequestContext{UserAgent: scenario.userAgent, RequestTime: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := prepared.Check(t.Context(), authz.DescribeCluster, authz.Resource{Kind: authz.Cluster, ID: "one", AccountID: account, Labels: map[string]string{"Team": scenario.team}})
			if err != nil || decision.Allowed != scenario.allowed {
				t.Fatalf("decision = %+v, %v", decision, err)
			}
		})
	}
}

func TestStoredAmbiguousRoles(t *testing.T) {
	store, _, _, _ := localStores(t)
	policy, err := store.CreatePolicy(t.Context(), "read", `permit(principal in ?principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"one", "two"} {
		if _, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: "arn:aws:iam::111111111111:role/" + path + "/developer", Scope: authz.GlobalScope}); err != nil {
			t.Fatal(err)
		}
	}
	if resolved, err := store.Resolve(t.Context(), authz.Identity{AccountID: account, CallerARN: "arn:aws:sts::111111111111:assumed-role/developer/alice"}); err == nil || resolved != nil {
		t.Fatalf("ambiguous roles resolved: %+v, %v", resolved, err)
	}
}
