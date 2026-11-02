package authz

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/google/uuid"
)

// PublishManagedPolicy is a platform provisioning operation, not tenant CRUD.
// Deployment must supply a dedicated publisher credential; runtime roles must
// lack catalog write permission. Existing entries cannot be replaced or deleted.
func PublishManagedPolicy(ctx context.Context, client *dynamodb.Client, table, id, content string) (StoredPolicy, error) {
	if client == nil || table == "" || !isStoreID(id) {
		return StoredPolicy{}, fmt.Errorf("invalid catalog publication")
	}
	_, validator, err := loadSchema()
	if err != nil {
		return StoredPolicy{}, err
	}
	store := &Store{validator: validator}
	if err := store.checkContent(content, id); err != nil {
		return StoredPolicy{}, err
	}
	policy := StoredPolicy{ID: id, OwnerAccountID: managedOwner, Kind: ManagedPolicy, Content: content, Revision: uuid.NewString()}
	item, err := storeItem("ACCOUNT#"+managedOwner, "POLICY#"+id, policy.Revision, policy)
	if err != nil {
		return StoredPolicy{}, err
	}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &table, Item: item, ConditionExpression: aws.String("attribute_not_exists(pk)")})
	if err != nil {
		return StoredPolicy{}, storeError(err)
	}
	return policy, nil
}
