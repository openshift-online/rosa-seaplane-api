//go:build integration

package authz_test

import (
	"errors"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
)

// managementFixture creates an isolated service whose admin role holds the CI-seeded management grant.
func managementFixture(t *testing.T) (*authz.Service, *authz.Store, authz.ManagementCaller, authz.ManagementCaller, authz.ServiceConfig) {
	t.Helper()
	store, _, client, cfg := localStores(t)
	config := authz.ServiceConfig{PolicyTable: cfg.PolicyTable, GlobalTable: cfg.GlobalTable, RegionalTable: cfg.RegionalTable}
	service, err := authz.NewService(client, config, cfg.Region)
	if err != nil {
		t.Fatal(err)
	}
	authztest.SeedAdmin(t, client, config, account)
	admin := authz.ManagementCaller{AccountID: account, CallerARN: "arn:aws:sts::111111111111:assumed-role/admin/session", Request: authz.RequestContext{RequestTime: time.Now()}}
	delegate := admin
	delegate.CallerARN = "arn:aws:iam::111111111111:user/delegate"
	return service, store, admin, delegate, config
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
		t.Fatalf("admin resource = %+v, %v", decision, err)
	}
}
