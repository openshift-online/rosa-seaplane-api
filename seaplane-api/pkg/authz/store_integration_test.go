//go:build integration

package authz_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

const (
	account = "111111111111"

	// Pinned so local and CI runs use the same emulator.
	dynamoLocalImage = "public.ecr.aws/aws-dynamodb-local/aws-dynamodb-local@sha256:744bf7138f0e893a6438de89bc3374501105b42bc7473cef6d561f30ff2dec6a"
)

// endpoint is shared by every test; localStores isolates tables per test.
var endpoint = os.Getenv("AUTHZ_DYNAMODB_ENDPOINT")

// TestMain runs the tests against AUTHZ_DYNAMODB_ENDPOINT or a disposable DynamoDB Local container.
func TestMain(m *testing.M) {
	if endpoint == "" {
		container, err := startDynamoDB()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer stopDynamoDB(container)
	}
	m.Run()
}

// startDynamoDB runs DynamoDB Local on a free port and waits until it answers.
func startDynamoDB() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	container := fmt.Sprintf("authz-storage-test-%d", port)
	out, err := exec.Command("podman", "run", "-d", "--rm", "--name", container, "-p", fmt.Sprintf("127.0.0.1:%d:8000", port), dynamoLocalImage, "-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("start DynamoDB Local: %w: %s", err, out)
	}

	endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	client := localClient()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = client.ListTables(ctx, &dynamodb.ListTablesInput{})
		cancel()
		if err == nil {
			return container, nil
		}
	}
	stopDynamoDB(container)
	return "", fmt.Errorf("DynamoDB Local not ready at %s: %w", endpoint, err)
}

func stopDynamoDB(container string) {
	if err := exec.Command("podman", "stop", container).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "stop DynamoDB Local container %s: %v\n", container, err)
	}
}

// localClient uses dummy credentials, which DynamoDB Local accepts.
func localClient() *dynamodb.Client {
	return dynamodb.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
	}), RetryMaxAttempts: 1}, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// localStores isolates each test's tables while sharing one local service.
func localStores(t *testing.T) (*authz.Store, *authz.Store, *dynamodb.Client, authz.StoreConfig) {
	t.Helper()
	client := localClient()
	prefix := "authz-test-" + uuid.NewString()
	cfg := authz.StoreConfig{AccountID: account, Region: "us-east-1", WriterRegion: "us-east-1", PolicyTable: prefix + "-policies", GlobalTable: prefix + "-global", RegionalTable: prefix + "-east"}
	for _, name := range []string{cfg.PolicyTable, cfg.GlobalTable, cfg.RegionalTable, prefix + "-west"} {
		_, err := client.CreateTable(t.Context(), &dynamodb.CreateTableInput{TableName: &name, BillingMode: types.BillingModePayPerRequest, AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}, {AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS}}, KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}, {AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &name}); err != nil {
				t.Errorf("delete table %s: %v", name, err)
			}
		})
	}
	east, err := authz.NewStore(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	westCfg := cfg
	westCfg.Region, westCfg.RegionalTable = "us-west-2", prefix+"-west"
	west, err := authz.NewStore(client, westCfg)
	if err != nil {
		t.Fatal(err)
	}
	return east, west, client, cfg
}

func TestStoredPolicyCRUD(t *testing.T) {
	east, west, client, cfg := localStores(t)
	ctx := t.Context()
	created, err := east.CreatePolicy(ctx, "read", `permit(principal, action, resource);`)
	if err != nil || created.Revision == "" || created.Kind != authz.CustomPolicy {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if _, err := east.CreatePolicy(ctx, "read", created.Content); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	got, err := east.GetPolicy(ctx, authz.PolicyRef{ID: "read", Kind: authz.CustomPolicy})
	if err != nil || got != created {
		t.Fatalf("get = %+v, %v", got, err)
	}
	updated, err := east.UpdatePolicy(ctx, "read", created.Revision, `forbid(principal, action, resource);`)
	if err != nil || updated.Revision == created.Revision {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := east.UpdatePolicy(ctx, "read", created.Revision, created.Content); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	if err := east.DeletePolicy(ctx, "read", created.Revision); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	if _, err := west.CreatePolicy(ctx, "west", created.Content); !errors.Is(err, authz.ErrWrongWriter) {
		t.Fatalf("wrong writer = %v", err)
	}
	cfg.AccountID = "222222222222"
	other, err := authz.NewStore(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.GetPolicy(ctx, authz.PolicyRef{ID: "read", Kind: authz.CustomPolicy}); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("foreign account = %v", err)
	}
	if err := east.DeletePolicy(ctx, "read", updated.Revision); err != nil {
		t.Fatal(err)
	}
	recreated, err := east.CreatePolicy(ctx, "read", created.Content)
	if err != nil || recreated.Revision == created.Revision || recreated.Revision == updated.Revision {
		t.Fatalf("recreate = %+v, %v", recreated, err)
	}
	if err := east.DeletePolicy(ctx, "read", updated.Revision); !errors.Is(err, authz.ErrConflict) {
		t.Fatalf("old incarnation delete = %v", err)
	}
}
