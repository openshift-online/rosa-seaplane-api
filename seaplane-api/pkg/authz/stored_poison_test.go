package authz_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/internal/authztest"
)

func TestStoredPrepareRejectsPoisonedForbid(t *testing.T) {
	for _, scope := range []authz.AttachmentScope{authz.GlobalScope, authz.RegionalScope} {
		for _, tc := range []struct {
			name   string
			poison func(*authz.StoredAttachment, *types.AttributeValueMemberS)
		}{
			{"foreign owner", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.OwnerAccountID = "210987654321"
			}},
			{"malformed principal", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.PrincipalARN = "bad-arn"
			}},
			{"foreign principal", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.PrincipalARN = "arn:aws:iam::210987654321:user/test"
			}},
			{"lookup principal mismatch", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.PrincipalARN = "arn:aws:iam::123456789012:user/other"
			}},
			{"scope mismatch", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				if binding.Scope == authz.GlobalScope {
					binding.Scope, binding.Region = authz.RegionalScope, "us-east-1"
					return
				}
				binding.Scope, binding.Region = authz.GlobalScope, ""
			}},
			{"region mismatch", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.Region = "us-west-2"
			}},
			{"payload revision mismatch", func(binding *authz.StoredAttachment, _ *types.AttributeValueMemberS) {
				binding.Revision = "550e8400-e29b-41d4-a716-446655440000"
			}},
			{"malformed stored revision", func(_ *authz.StoredAttachment, revision *types.AttributeValueMemberS) {
				revision.Value = "bad-revision"
			}},
		} {
			t.Run(string(scope)+"/"+tc.name, func(t *testing.T) {
				ctx := t.Context()
				cfg := authz.StoreConfig{AccountID: "123456789012", Region: "us-east-1", PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional"}
				client := authztest.Dynamo(t)
				store, err := authz.NewTestStore(client, cfg)
				if err != nil {
					t.Fatal(err)
				}
				id := authz.Identity{AccountID: cfg.AccountID, CallerARN: "arn:aws:iam::123456789012:user/test"}
				request := authz.RequestContext{RequestTime: time.Now()}
				forbid, err := store.CreatePolicy(ctx, "forbid", `forbid(principal, action, resource);`)
				if err != nil {
					t.Fatal(err)
				}
				binding, err := store.CreateAttachment(ctx, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: forbid.ID, Kind: forbid.Kind}, PrincipalARN: id.CallerARN, Scope: scope})
				if err != nil {
					t.Fatal(err)
				}

				// Keep the original lookup key so Prepare must reject, not skip, the poisoned forbid.
				table := cfg.GlobalTable
				if scope == authz.RegionalScope {
					table = cfg.RegionalTable
				}
				stored, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: map[string]types.AttributeValue{
					"pk": &types.AttributeValueMemberS{Value: "ACCOUNT#" + cfg.AccountID + "#EXACT#" + id.CallerARN},
					"sk": &types.AttributeValueMemberS{Value: "ATTACHMENT#" + binding.ID},
				}})
				if err != nil {
					t.Fatal(err)
				}
				revision, ok := stored.Item["revision"].(*types.AttributeValueMemberS)
				if !ok {
					t.Fatal("missing forbid lookup record")
				}
				tc.poison(&binding, revision)
				payload, err := json.Marshal(binding)
				if err != nil {
					t.Fatal(err)
				}
				stored.Item["record"] = &types.AttributeValueMemberS{Value: string(payload)}
				if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: stored.Item}); err != nil {
					t.Fatal(err)
				}
				prepared, err := store.Prepare(ctx, id, request)
				var failure *authz.Failure
				if prepared != nil || !errors.As(err, &failure) || failure.Stage != authz.StageResolution {
					t.Fatalf("poisoned forbid must fail closed: prepared=%v err=%v", prepared, err)
				}
			})
		}
	}
}
