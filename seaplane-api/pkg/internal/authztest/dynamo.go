package authztest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

type dynamoRequest struct {
	TableName                 string
	Item, Key                 map[string]any
	ConditionExpression       string
	ExpressionAttributeValues map[string]any
	TransactItems             []struct{ Put, Delete, ConditionCheck *dynamoRequest }
	RequestItems              map[string]struct{ Keys []map[string]any }
}

// dynamoKey identifies the table and primary key in a DynamoDB request.
func dynamoKey(in *dynamoRequest) string {
	key := in.Key
	if in.Item != nil {
		key = in.Item
	}
	pk, _ := json.Marshal(key["pk"])
	sk, _ := json.Marshal(key["sk"])
	return in.TableName + string(pk) + string(sk)
}

// hasDynamoConflict checks the conditional expressions exercised by management mutations.
func hasDynamoConflict(in *dynamoRequest, old map[string]any) bool {
	if in.ConditionExpression == "attribute_not_exists(pk)" {
		return old != nil
	}
	for _, token := range []string{":expected", ":policy"} {
		if expected := in.ExpressionAttributeValues[token]; expected != nil {
			return old == nil || !reflect.DeepEqual(old["revision"], expected)
		}
	}
	return false
}

// Dynamo runs a DynamoDB JSON transport fixture with conditional writes and partition queries.
func Dynamo(t *testing.T) *dynamodb.Client {
	t.Helper()
	var mu sync.Mutex
	items := map[string]map[string]any{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var in dynamoRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		operation := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810.")
		id := dynamoKey(&in)
		switch operation {
		case "PutItem", "DeleteItem":
			if hasDynamoConflict(&in, items[id]) {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException"}`)
				return
			}
			if operation == "PutItem" {
				items[id] = in.Item
			} else {
				delete(items, id)
			}
			_, _ = io.WriteString(w, `{}`)
		case "TransactWriteItems":
			for _, write := range in.TransactItems {
				for _, part := range []*dynamoRequest{write.Put, write.Delete, write.ConditionCheck} {
					if part != nil && hasDynamoConflict(part, items[dynamoKey(part)]) {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#TransactionCanceledException","CancellationReasons":[{"Code":"ConditionalCheckFailed"}]}`)
						return
					}
				}
			}
			for _, write := range in.TransactItems {
				if write.Put != nil {
					items[dynamoKey(write.Put)] = write.Put.Item
				}
				if write.Delete != nil {
					delete(items, dynamoKey(write.Delete))
				}
			}
			_, _ = io.WriteString(w, `{}`)
		case "GetItem":
			_ = json.NewEncoder(w).Encode(map[string]any{"Item": items[id]})
		case "BatchGetItem":
			responses := map[string][]map[string]any{}
			for table, request := range in.RequestItems {
				responses[table] = []map[string]any{}
				for _, key := range request.Keys {
					if item := items[dynamoKey(&dynamoRequest{TableName: table, Key: key})]; item != nil {
						responses[table] = append(responses[table], item)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Responses": responses})
		case "Query":
			found := []map[string]any{}
			for id, item := range items {
				if strings.HasPrefix(id, in.TableName+"{") && reflect.DeepEqual(item["pk"], in.ExpressionAttributeValues[":pk"]) {
					found = append(found, item)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": found})
		default:
			t.Errorf("unexpected DynamoDB operation %s", operation)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(endpoint.Close)
	return dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("local", "local", "")}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint.URL) })
}

// Tables are the table names every fixture service uses.
var Tables = authz.ServiceConfig{PolicyTable: "policies", GlobalTable: "global", RegionalTable: "regional"}

// AdminPolicy is the management grant pre-prod CI seeds for its admin role.
const AdminPolicy = `permit(principal, action in Seaplane::Action::"PolicyAdmin", resource);`

// Admin returns a caller in a session of the fixture admin role, which SeedAdmin grants management.
func Admin(account string) authz.ManagementCaller {
	return authz.ManagementCaller{AccountID: account, CallerARN: "arn:aws:sts::" + account + ":assumed-role/admin/session", Request: authz.RequestContext{RequestTime: time.Now()}}
}

// Seed writes one grant exactly as pre-prod CI does in rosa-seaplane ci/e2e-tests.sh, bypassing the API:
// a custom policy plus a global attachment for an IAM role, sharing one fixed ID so reruns are no-ops.
// Literal JSON, not store code, so the contract test catches store format drift.
func Seed(t *testing.T, client *dynamodb.Client, cfg authz.ServiceConfig, account, role, id, content string) {
	t.Helper()
	policyRevision, attachmentRevision := uuid.NewString(), uuid.NewString()
	policy, err := json.Marshal(map[string]string{"ID": id, "OwnerAccountID": account, "Content": content, "Revision": policyRevision, "Kind": "custom"})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := json.Marshal(map[string]any{
		"Policy": map[string]string{"ID": id, "Kind": "custom"}, "PrincipalARN": "arn:aws:iam::" + account + ":role/" + role,
		"Scope": "global", "ID": id, "OwnerAccountID": account, "Region": "", "Revision": attachmentRevision,
	})
	if err != nil {
		t.Fatal(err)
	}
	put := func(table, pk, sk, revision string, record []byte) types.TransactWriteItem {
		item := map[string]types.AttributeValue{}
		for name, value := range map[string]string{"pk": pk, "sk": sk, "revision": revision, "record": string(record)} {
			item[name] = &types.AttributeValueMemberS{Value: value}
		}
		return types.TransactWriteItem{Put: &types.Put{TableName: aws.String(table), Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")}}
	}
	accountKey := "ACCOUNT#" + account
	_, err = client.TransactWriteItems(t.Context(), &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		put(cfg.PolicyTable, accountKey, "POLICY#"+id, policyRevision, policy),
		put(cfg.GlobalTable, accountKey, "ATTACHMENT#"+id, attachmentRevision, attachment),
		put(cfg.GlobalTable, accountKey+"#ROLE#aws/"+account+"/"+role, "ATTACHMENT#"+id, attachmentRevision, attachment),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// SeedAdmin grants management to the fixture admin role's sessions in account.
func SeedAdmin(t *testing.T, client *dynamodb.Client, cfg authz.ServiceConfig, account string) {
	t.Helper()
	Seed(t, client, cfg, account, "admin", "seed-admin", AdminPolicy)
}

// Management returns a service whose account admin is seeded and whose test user holds the given policies.
func Management(t *testing.T, account, region string, policies ...string) *authz.Service {
	t.Helper()
	client := Dynamo(t)
	management, err := authz.NewService(client, Tables, region)
	if err != nil {
		t.Fatal(err)
	}
	SeedAdmin(t, client, Tables, account)
	caller := Admin(account)
	for i, content := range policies {
		id := fmt.Sprintf("grant-%d", i)
		if _, err := management.CreatePolicy(t.Context(), caller, id, content); err != nil {
			t.Fatal(err)
		}
		if _, err := management.CreateAttachment(t.Context(), caller, authz.AttachmentSpec{Policy: authz.PolicyRef{ID: id, Kind: authz.CustomPolicy}, PrincipalARN: "arn:aws:iam::" + account + ":user/test", Scope: authz.GlobalScope}); err != nil {
			t.Fatal(err)
		}
	}
	return management
}
