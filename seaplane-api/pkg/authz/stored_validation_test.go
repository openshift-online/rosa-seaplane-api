package authz_test

import (
	"errors"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
)

func TestStoredPolicyValidation(t *testing.T) {
	store, err := authz.NewTestStore(authztest.Dynamo(t), authz.StoreConfig{AccountID: "123456789012", Region: "us-east-1", PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional"})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"malformed":            "permit(",
		"schema invalid":       `permit(principal, action, resource) when { context.untrusted == "yes" };`,
		"unguarded tag":        `forbid(principal, action == Seaplane::Action::"DescribeCluster", resource) when { resource.getTag("x") == "y" };`,
		"multiple statements":  `permit(principal, action, resource); permit(principal, action, resource);`,
		"unsupported template": `permit(principal, action == ?action, resource);`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.CreatePolicy(t.Context(), "invalid", content); !errors.Is(err, authz.ErrValidation) {
				t.Fatalf("accepted invalid policy: %v", err)
			}
		})
	}
	policy, err := store.CreatePolicy(t.Context(), "read", `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []authz.AttachmentSpec{
		{Policy: authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, PrincipalARN: "bad-arn", Scope: authz.GlobalScope},
		{Policy: authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws:iam::210987654321:role/readers", Scope: authz.GlobalScope},
		{Policy: authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws-us-gov:iam::123456789012:role/readers", Scope: authz.GlobalScope},
		{Policy: authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws:iam::123456789012:role/readers", Scope: "invalid"},
	} {
		if _, err := store.CreateAttachment(t.Context(), spec); !errors.Is(err, authz.ErrValidation) {
			t.Fatalf("accepted invalid attachment: %v", err)
		}
	}
}

func TestStoredPreparedSnapshot(t *testing.T) {
	management := authztest.Management(t, "123456789012", "us-east-1", `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`)
	id := authz.Identity{AccountID: "123456789012", CallerARN: "arn:aws:iam::123456789012:user/test"}
	request := authz.RequestContext{RequestTime: time.Now()}
	frozen, err := management.Prepare(t.Context(), id, request)
	if err != nil {
		t.Fatal(err)
	}
	admin := authztest.Admin(id.AccountID)
	policy, err := management.GetPolicy(t.Context(), admin, authz.PolicyRef{ID: "grant-0", Kind: authz.CustomPolicy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := management.UpdatePolicy(t.Context(), admin, authz.PolicyRef{ID: policy.ID, Kind: authz.CustomPolicy}, policy.Revision, `forbid(principal, action, resource);`); err != nil {
		t.Fatal(err)
	}
	fresh, err := management.Prepare(t.Context(), id, request)
	if err != nil {
		t.Fatal(err)
	}
	resource := authz.Resource{Kind: authz.Collection, CollectionKind: authz.Cluster, AccountID: id.AccountID}
	before, err := frozen.Check(t.Context(), authz.ListClusters, resource)
	if err != nil || !before.Allowed || len(before.Provenance) != 1 || before.Provenance[0].PolicyRevision != policy.Revision || before.Provenance[0].AttachmentRevision == "" {
		t.Fatalf("frozen decision lost revisions: %+v %v", before, err)
	}
	after, err := fresh.Check(t.Context(), authz.ListClusters, resource)
	if err != nil || after.Allowed || len(after.Provenance) != 1 || after.Provenance[0].PolicyRevision == policy.Revision {
		t.Fatalf("new request ignored revocation: %+v %v", after, err)
	}
}
