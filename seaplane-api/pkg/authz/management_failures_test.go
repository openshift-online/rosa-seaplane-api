//go:build integration

package authz_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// grantManagement attaches a policy for the requested scope through the bootstrap caller.
func grantManagement(t *testing.T, m *authz.Management, admin, delegate authz.ManagementCaller, id, content string, scope authz.AttachmentScope) authz.StoredPolicy {
	t.Helper()
	policy, err := m.CreatePolicy(t.Context(), admin, id, content)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.CreateAttachment(t.Context(), admin, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, PrincipalARN: delegate.CallerARN, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// TestManagementForbidAndErrors rejects forbids, evaluation errors, and broken grant references.
func TestManagementForbidAndErrors(t *testing.T) {
	for _, scenario := range []struct {
		name, content string
		failure       bool
	}{
		{"forbid", `forbid(principal, action == Seaplane::Action::"ListPolicies", resource);`, false},
		{"evaluation error", `forbid(principal, action == Seaplane::Action::"ListPolicies", resource) when { context.requestTime.unixSeconds * 9223372036854775807 > 0 };`, true},
		{"missing policy", `permit(principal, action, resource);`, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, _, admin, delegate, _ := managementFixture(t)
			grantManagement(t, m, admin, delegate, "allow", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.GlobalScope)
			policy := grantManagement(t, m, admin, delegate, "deny", scenario.content, authz.GlobalScope)
			if scenario.name == "missing policy" {
				if _, err := m.DeletePolicy(t.Context(), admin, authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, policy.Revision); err != nil {
					t.Fatal(err)
				}
			}
			_, err := m.ListPolicies(t.Context(), delegate)
			var failure *authz.Failure
			if scenario.failure {
				if !errors.As(err, &failure) {
					t.Fatalf("expected failure, got %v", err)
				}
			} else if !errors.Is(err, authz.ErrManagementDenied) {
				t.Fatalf("forbid = %v", err)
			}
			if _, err := m.ListPolicies(t.Context(), admin); err != nil {
				t.Fatalf("bootstrap repair access = %v", err)
			}
		})
	}
}

// TestManagementRepair keeps bootstrap available when stored Cedar becomes invalid.
func TestManagementRepair(t *testing.T) {
	m, _, admin, delegate, cfg := managementFixture(t)
	policy := grantManagement(t, m, admin, delegate, "broken", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.GlobalScope)
	policy.Content = `permit(principal, action == Seaplane::Action::"Removed", resource);`
	payload, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = localClient().PutItem(t.Context(), &dynamodb.PutItemInput{TableName: &cfg.PolicyTable, Item: map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "ACCOUNT#" + account}, "sk": &types.AttributeValueMemberS{Value: "POLICY#" + policy.ID},
		"revision": &types.AttributeValueMemberS{Value: policy.Revision}, "record": &types.AttributeValueMemberS{Value: string(payload)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var failure *authz.Failure
	if _, err := m.ListPolicies(t.Context(), delegate); !errors.As(err, &failure) {
		t.Fatalf("invalid stored policy = %v", err)
	}
	ref := authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}
	if _, err := m.GetPolicy(t.Context(), admin, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdatePolicy(t.Context(), admin, ref, policy.Revision, `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListPolicies(t.Context(), delegate); err != nil {
		t.Fatalf("repair = %v", err)
	}
}

// TestManagementWriterAndOutage checks local writes, writer guidance, and typed backend failures.
func TestManagementWriterAndOutage(t *testing.T) {
	m, _, admin, delegate, cfg := managementFixture(t)
	grantManagement(t, m, admin, delegate, "global", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.GlobalScope)
	westCfg := cfg
	westCfg.RegionalTable = cfg.RegionalTable[:len(cfg.RegionalTable)-4] + "west"
	west, err := authz.NewManagement(localClient(), westCfg, "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []authz.ManagementCaller{admin, delegate} {
		_, err := west.CreatePolicy(t.Context(), caller, "wrong", `permit(principal, action, resource);`)
		var guidance *authz.WrongWriterError
		if !errors.As(err, &guidance) || !errors.Is(err, authz.ErrWrongWriter) || guidance.Region != "us-east-1" {
			t.Fatalf("writer = %v", err)
		}
	}
	regional := grantManagement(t, m, admin, delegate, "local", `permit(principal, action in Seaplane::Action::"RegionalAttachmentAdmin", resource);`, authz.RegionalScope)
	if _, err := west.CreateAttachment(t.Context(), admin, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: regional.ID, Kind: regional.Kind}, PrincipalARN: admin.CallerARN, Scope: authz.RegionalScope}); err != nil {
		t.Fatal(err)
	}
	unavailable := dynamodb.New(localClient().Options(), func(o *dynamodb.Options) {
		o.HTTPClient = &http.Client{Transport: storeTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("injected outage") })}
	})
	offline, err := authz.NewManagement(unavailable, cfg, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []authz.ManagementCaller{admin, delegate} {
		if _, err := offline.ListPolicies(t.Context(), caller); !errors.Is(err, authz.ErrUnavailable) || errors.Is(err, authz.ErrValidation) {
			t.Fatalf("outage list = %v", err)
		}
	}
	if _, err := offline.CreatePolicy(t.Context(), admin, "outage", `permit(principal, action, resource);`); !errors.Is(err, authz.ErrUnavailable) {
		t.Fatalf("outage write = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if offline.IsAccountRegistered(canceled, account) {
		t.Fatal("canceled registration")
	}
}

// TestManagementRegionIsolation excludes off-region grants without partial global authorization.
func TestManagementRegionIsolation(t *testing.T) {
	m, _, admin, delegate, cfg := managementFixture(t)
	grantManagement(t, m, admin, delegate, "local", `permit(principal, action in Seaplane::Action::"RegionalAttachmentAdmin", resource);`, authz.RegionalScope)
	cfg.RegionalTable = cfg.RegionalTable[:len(cfg.RegionalTable)-4] + "west"
	west, err := authz.NewManagement(localClient(), cfg, "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := west.ListAttachments(t.Context(), delegate, authz.RegionalScope); !errors.Is(err, authz.ErrManagementDenied) {
		t.Fatalf("off-region = %v", err)
	}
	// Removing the local table must not affect a global-only management check.
	grantManagement(t, m, admin, delegate, "global", `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`, authz.GlobalScope)
	client := localClient()
	_, err = client.DeleteTable(t.Context(), &dynamodb.DeleteTableInput{TableName: aws.String(cfg.RegionalTable)})
	if err != nil {
		t.Fatal(err)
	}
	// Recreate for the existing harness cleanup after checking the outage.
	t.Cleanup(func() {
		_, err := client.CreateTable(context.Background(), &dynamodb.CreateTableInput{TableName: &cfg.RegionalTable, BillingMode: types.BillingModePayPerRequest, AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}, {AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS}}, KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}, {AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange}}})
		if err != nil {
			t.Error(err)
		}
	})
	if _, err := west.ListPolicies(t.Context(), delegate); err != nil {
		t.Fatalf("global touched regional store = %v", err)
	}
	if _, err := west.ListAttachments(t.Context(), delegate, authz.RegionalScope); !errors.Is(err, authz.ErrUnavailable) {
		t.Fatalf("regional outage = %v", err)
	}
}
