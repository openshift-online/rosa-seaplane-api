package authz_test

import (
	"errors"
	"testing"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
)

// TestCISeed pins the contract with pre-prod CI (rosa-seaplane ci/e2e-tests.sh): the exact grants it
// writes directly into DynamoDB let its admin role manage policies and use resources outside the deny probe.
func TestCISeed(t *testing.T) {
	const account = "111111111111"
	ctx := t.Context()
	client := authztest.Dynamo(t)
	service, err := authz.NewService(client, authztest.Tables, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	for id, content := range map[string]string{
		"e2e-all-actions":  `permit(principal, action in Seaplane::Action::"AllActions", resource) unless { context.requestLabels.hasTag("authz-deny-probe") };`,
		"e2e-policy-admin": `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`,
	} {
		authztest.Seed(t, client, authztest.Tables, account, "rosa-seaplane-account-admin", id, content)
	}
	session := "arn:aws:sts::" + account + ":assumed-role/rosa-seaplane-account-admin/seaplane-e2e"
	caller := authz.ManagementCaller{AccountID: account, CallerARN: session, Request: authz.RequestContext{RequestTime: time.Now()}}

	// Management reads accept the seeded rows and the grant authorizes writes.
	if _, err := service.CreatePolicy(ctx, caller, "created", `permit(principal, action in Seaplane::Action::"ReadOnly", resource);`); err != nil {
		t.Fatal(err)
	}
	if policies, err := service.ListPolicies(ctx, caller); err != nil || len(policies) != 3 {
		t.Fatalf("policies = %+v, %v", policies, err)
	}
	if attachments, err := service.ListAttachments(ctx, caller, ""); err != nil || len(attachments) != 2 {
		t.Fatalf("attachments = %+v, %v", attachments, err)
	}

	// Resource actions are allowed except for deny-probe requests.
	collection := authz.Resource{Kind: authz.Collection, CollectionKind: authz.Cluster, AccountID: account}
	for _, tc := range []struct {
		labels  map[string]string
		allowed bool
	}{{nil, true}, {map[string]string{"authz-deny-probe": "true"}, false}} {
		prepared, err := service.Prepare(ctx, authz.Identity{AccountID: account, CallerARN: session}, authz.RequestContext{RequestTime: time.Now(), RequestLabels: tc.labels})
		if err != nil {
			t.Fatal(err)
		}
		if decision, err := prepared.Check(ctx, authz.ListClusters, collection); err != nil || decision.Allowed != tc.allowed {
			t.Fatalf("labels %v: %+v, %v", tc.labels, decision, err)
		}
	}
}

// TestServiceConfig requires three distinct valid tables before any backend use.
func TestServiceConfig(t *testing.T) {
	valid := authz.ServiceConfig{PolicyTable: "authz-policies", GlobalTable: "authz-global", RegionalTable: "authz-regional"}
	for name, mutate := range map[string]func(*authz.ServiceConfig){
		"valid":           func(*authz.ServiceConfig) {},
		"missing table":   func(c *authz.ServiceConfig) { c.RegionalTable = "" },
		"duplicate table": func(c *authz.ServiceConfig) { c.RegionalTable = c.GlobalTable },
		"invalid table":   func(c *authz.ServiceConfig) { c.RegionalTable = "x/y" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			_, err := authz.NewService(authztest.Dynamo(t), cfg, "us-east-1")
			if wantOK := name == "valid"; wantOK != (err == nil) || (err != nil && !errors.Is(err, authz.ErrValidation)) {
				t.Fatalf("%+v: %v", cfg, err)
			}
		})
	}
}
