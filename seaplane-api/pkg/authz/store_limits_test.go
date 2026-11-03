//go:build integration

package authz_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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

func TestStoredConcurrentMutations(t *testing.T) {
	store, _, _, _ := localStores(t)
	ctx := t.Context()
	first, err := store.CreatePolicy(ctx, "first", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreatePolicy(ctx, "second", first.Content)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:iam::111111111111:user/alice"
	binding, err := store.CreateAttachment(ctx, authz.AttachmentSpec{PrincipalARN: caller, Policy: authz.PolicyRef{ID: first.ID, Kind: first.Kind}, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			_, err := store.UpdateAttachment(ctx, binding.Scope, binding.ID, binding.Revision, authz.PolicyRef{ID: second.ID, Kind: second.Kind})
			outcomes <- err
		})
	}
	wait.Wait()
	close(outcomes)
	successes := 0
	for err := range outcomes {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent writes = %d", successes)
	}
	got, err := store.GetAttachment(ctx, binding.Scope, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Resolve(ctx, authz.Identity{AccountID: account, CallerARN: caller})
	if err != nil || len(resolved) != 1 || resolved[0].Attachment != got || got.Policy.ID != second.ID {
		t.Fatalf("transaction copies disagree: %+v, %v", resolved, err)
	}
}

func TestStoredRejectsWrongLookupID(t *testing.T) {
	store, _, client, cfg := localStores(t)
	policy, err := store.CreatePolicy(t.Context(), "read", `permit(principal, action, resource);`)
	if err != nil {
		t.Fatal(err)
	}
	caller := "arn:aws:iam::111111111111:user/alice"
	binding, err := store.CreateAttachment(t.Context(), authz.AttachmentSpec{PrincipalARN: caller, Policy: authz.PolicyRef{ID: policy.ID, Kind: policy.Kind}, Scope: authz.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the external database fixture, then observe only the store interface.
	originalID := binding.ID
	binding.ID = "wrong-id"
	payload, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.UpdateItem(t.Context(), &dynamodb.UpdateItemInput{TableName: &cfg.GlobalTable, Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "ACCOUNT#" + account + "#EXACT#" + caller}, "sk": &types.AttributeValueMemberS{Value: "ATTACHMENT#" + originalID}}, UpdateExpression: aws.String("SET #record = :record"), ExpressionAttributeNames: map[string]string{"#record": "record"}, ExpressionAttributeValues: map[string]types.AttributeValue{":record": &types.AttributeValueMemberS{Value: string(payload)}}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.Resolve(t.Context(), authz.Identity{AccountID: account, CallerARN: caller})
	if err == nil || resolved != nil {
		t.Fatalf("malformed lookup accepted: %+v, %v", resolved, err)
	}
}
