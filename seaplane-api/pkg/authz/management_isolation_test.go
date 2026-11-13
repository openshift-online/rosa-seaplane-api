//go:build integration

package authz_test

import (
	"errors"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
)

// TestManagementAccounts scopes admin and delegated grants to the authenticated account.
func TestManagementAccounts(t *testing.T) {
	m, _, admin, delegate, cfg := managementFixture(t)
	policy := grantManagement(t, m, admin, delegate, "all", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.GlobalScope)
	other := admin
	other.AccountID = "222222222222"
	other.CallerARN = "arn:aws:iam::222222222222:user/delegate"
	if _, err := m.ListPolicies(t.Context(), other); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("delegation escaped account = %v", err)
	}

	// Another account's seeded admin administers only its own account's records.
	authztest.SeedAdmin(t, localClient(), cfg, other.AccountID)
	other.CallerARN = "arn:aws:sts::222222222222:assumed-role/admin/session"
	if _, err := m.GetPolicy(t.Context(), other, authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("foreign policy = %v", err)
	}
	policies, err := m.ListPolicies(t.Context(), other)
	if err != nil || len(policies) != 1 || policies[0].ID != "seed-admin" {
		t.Fatalf("foreign list = %+v, %v", policies, err)
	}
	attachments, err := m.ListAttachments(t.Context(), other, "")
	if err != nil || len(attachments) != 1 || attachments[0].Policy.ID != "seed-admin" {
		t.Fatalf("foreign attachments = %+v, %v", attachments, err)
	}
}

// TestManagementResourceGroups cannot turn resource-only grants into policy administration.
func TestManagementResourceGroups(t *testing.T) {
	for _, group := range []string{"AllActions", "ReadOnly"} {
		t.Run(group, func(t *testing.T) {
			m, _, admin, delegate, _ := managementFixture(t)
			grantManagement(t, m, admin, delegate, "resource", `permit(principal, action in Seaplane::Action::"`+group+`", resource);`, authz.GlobalScope)
			if _, err := m.ListPolicies(t.Context(), delegate); !errors.Is(err, authz.ErrManagementDenied) {
				t.Fatalf("resource grants management = %v", err)
			}
			if _, err := m.ListAttachments(t.Context(), delegate, authz.RegionalScope); !errors.Is(err, authz.ErrManagementDenied) {
				t.Fatalf("resource grants local management = %v", err)
			}
		})
	}
}
