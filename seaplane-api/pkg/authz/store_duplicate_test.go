//go:build integration

package authz_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

func TestStoredDuplicateBindingFails(t *testing.T) {
	store, _, client, cfg := localStores(t)
	permit, err := store.CreatePolicy(t.Context(), "permit", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	forbid, err := store.CreatePolicy(t.Context(), "forbid", `forbid(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:sts::111111111111:assumed-role/developer/alice"
	denial, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{PrincipalARN: caller, Policy: authz.PolicyRef{ID: forbid.ID, Kind: forbid.Kind}, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{PrincipalARN: "arn:aws:iam::111111111111:role/developer", Policy: authz.PolicyRef{ID: permit.ID, Kind: permit.Kind}, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	// A well-keyed but corrupt duplicate must not replace the forbid in Cedar.
	grant.ID = denial.ID
	payload, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PutItem(t.Context(), &dynamodb.PutItemInput{TableName: &cfg.GlobalTable, Item: map[string]types.AttributeValue{
		"pk":       &types.AttributeValueMemberS{Value: "ACCOUNT#" + account + "#ROLE#aws/" + account + "/developer"},
		"sk":       &types.AttributeValueMemberS{Value: "ATTACHMENT#" + denial.ID},
		"revision": &types.AttributeValueMemberS{Value: grant.Revision},
		"record":   &types.AttributeValueMemberS{Value: string(payload)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), authz.Identity{AccountID: account, CallerARN: caller}, authz.RequestContext{RequestTime: time.Now()})
	if err == nil || prepared != nil {
		t.Fatalf("duplicate binding: has preparation %t, error %v", prepared != nil, err)
	}
}
