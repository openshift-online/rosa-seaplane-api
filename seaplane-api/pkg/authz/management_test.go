//go:build integration

package authz_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

const adminARN = "arn:aws:iam::111111111111:role/path/admin"

// managementFixture creates an isolated service with one bootstrap role.
func managementFixture(t *testing.T) (*authz.Management, *authz.Store, authz.ManagementCaller, authz.ManagementCaller, authz.ManagementConfig) {
	t.Helper()
	store, _, client, cfg := localStores(t)
	config := authz.ManagementConfig{FormatVersion: 1, PolicyTable: cfg.PolicyTable, GlobalTable: cfg.GlobalTable, RegionalTable: cfg.RegionalTable, Accounts: map[string]authz.ManagementAccount{account: {WriterRegion: cfg.WriterRegion, AdministratorARNs: []string{adminARN}}}}
	service, err := authz.NewManagement(client, config, cfg.Region)
	if err != nil {
		t.Fatal(err)
	}
	admin := authz.ManagementCaller{AccountID: account, CallerARN: "arn:aws:sts::111111111111:assumed-role/admin/session", Request: authz.RequestContext{RequestTime: time.Now()}}
	delegate := admin
	delegate.CallerARN = "arn:aws:iam::111111111111:user/delegate"
	return service, store, admin, delegate, config
}

// TestManagementDelegation verifies bootstrap, scoped delegation, and immediate revocation.
func TestManagementDelegation(t *testing.T) {
	m, _, admin, delegate, _ := managementFixture(t)
	ctx := t.Context()
	if !m.IsAccountRegistered(ctx, account) || m.IsAccountRegistered(ctx, "222222222222") {
		t.Fatal("registration")
	}
	if _, err := m.ListPolicies(ctx, delegate); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("ungranted = %v", err)
	}
	policy, err := m.CreatePolicy(ctx, admin, "regional", `permit(principal == ?principal, action in Seaplane::Action::"RegionalAttachmentAdmin", resource);`)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := m.CreateAttachment(ctx, admin, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: delegate.CallerARN, Scope: authz.RegionalScope})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListAttachments(ctx, delegate, authz.RegionalScope); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListAttachments(ctx, delegate, ""); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("both list = %v", err)
	}
	if _, err := m.ListPolicies(ctx, delegate); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("regional policy access = %v", err)
	}
	own, err := m.CreateAttachment(ctx, delegate, authz.AttachmentSpec{Policy: grant.Policy, PrincipalARN: delegate.CallerARN, Scope: authz.RegionalScope})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteAttachment(ctx, delegate, own.Scope, own.ID, uuid.NewString()); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale attachment delete = %v", err)
	}
	if _, err := m.DeleteAttachment(ctx, delegate, own.Scope, own.ID, own.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DeleteAttachment(ctx, admin, grant.Scope, grant.ID, grant.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListAttachments(ctx, delegate, authz.RegionalScope); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("revoked = %v", err)
	}
}

// TestManagementGlobalIsolation rejects global actions from even broad regional grants.
func TestManagementGlobalIsolation(t *testing.T) {
	m, store, admin, delegate, _ := managementFixture(t)
	ctx := t.Context()
	policy := grantManagement(t, m, admin, delegate, "all", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.RegionalScope)
	if _, err := m.ListPolicies(ctx, delegate); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("regional escalation = %v", err)
	}
	globalSpec := authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: delegate.CallerARN, Scope: authz.GlobalScope}
	if _, err := m.CreateAttachment(ctx, delegate, globalSpec); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("regional global-write escalation = %v", err)
	}
	if _, err := m.ListAttachments(ctx, delegate, authz.RegionalScope); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateAttachment(ctx, admin, globalSpec); err != nil {
		t.Fatal(err)
	}
	grantManagement(t, m, admin, delegate, "regional-forbid", `forbid(principal, action, resource);`, authz.RegionalScope)
	if _, err := m.ListPolicies(ctx, delegate); err != nil {
		t.Fatalf("global check used regional forbid: %v", err)
	}
	if _, err := m.ListAttachments(ctx, delegate, authz.RegionalScope); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("regional forbid = %v", err)
	}
	delegate.AccountID = "222222222222"
	if _, err := m.ListPolicies(ctx, delegate); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("foreign = %v", err)
	}
	prepared, err := store.Prepare(ctx, authz.Identity{AccountID: admin.AccountID, CallerARN: admin.CallerARN}, admin.Request)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := prepared.Check(ctx, authz.ListClusters, authz.Resource{Kind: authz.Collection, CollectionKind: authz.Cluster, AccountID: account})
	if err != nil || decision.Allowed {
		t.Fatalf("bootstrap resource = %+v, %v", decision, err)
	}
}

// TestManagementValidation checks schema validation, revisions, and managed immutability.
func TestManagementValidation(t *testing.T) {
	m, _, admin, _, cfg := managementFixture(t)
	ctx := t.Context()
	for _, content := range []string{`not cedar`, `permit(principal, action == Seaplane::Action::"Bogus", resource);`, `permit(principal, action, resource) when { principal.missing == true };`, `permit(principal, action, resource == ?resource);`} {
		if _, err := m.CreatePolicy(ctx, admin, "bad", content); !errors.Is(err, authz.ErrValidation) {
			t.Fatalf("validation %q = %v", content, err)
		}
	}
	managed, err := authz.PublishManagedPolicy(ctx, localClient(), cfg.PolicyTable, "collision", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := m.CreatePolicy(ctx, admin, managed.ID, `forbid(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := m.ListPolicies(ctx, admin)
	if err != nil || len(policies) != 2 {
		t.Fatalf("list = %+v, %v", policies, err)
	}
	ref := authz.PolicyRef{ID: managed.ID, Kind: authz.ManagedPolicy}
	if _, err := m.UpdatePolicy(ctx, admin, ref, managed.Revision, custom.Content); !errors.Is(err, authz.ErrManagedPolicy) {
		t.Fatalf("managed update = %v", err)
	}
	if _, err := m.DeletePolicy(ctx, admin, ref, managed.Revision); !errors.Is(err, authz.ErrManagedPolicy) {
		t.Fatalf("managed delete = %v", err)
	}
	ref.Kind = authz.CustomPolicy
	if _, err := m.UpdatePolicy(ctx, admin, ref, uuid.NewString(), custom.Content); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	if _, err := m.DeletePolicy(ctx, admin, ref, uuid.NewString()); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	if _, err := m.CreateAttachment(ctx, admin, authz.AttachmentSpec{Policy: ref, PrincipalARN: "arn:aws:iam::222222222222:user/foreign", Scope: authz.GlobalScope}); !errors.Is(err, authz.ErrValidation) {
		t.Fatalf("foreign attachment = %v", err)
	}
	receipt, err := m.DeletePolicy(ctx, admin, ref, custom.Revision)
	if err != nil || receipt.DeletedRevision != custom.Revision || receipt.CommittedRegion != "us-east-1" {
		t.Fatalf("delete = %+v, %v", receipt, err)
	}
}
