package authz

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

// policyOwner returns the account ID or managed catalog owner for a valid policy reference.
func (s *Store) policyOwner(ref PolicyRef) (string, error) {
	if !isStoreID(ref.ID) {
		return "", fmt.Errorf("%w: invalid policy ID", ErrValidation)
	}
	switch ref.Kind {
	case CustomPolicy:
		return s.config.AccountID, nil
	case ManagedPolicy:
		return managedOwner, nil
	default:
		return "", fmt.Errorf("%w: invalid policy kind", ErrValidation)
	}
}

// checkContent checks that a policy or template fits the size limit and passes schema validation.
func (s *Store) checkContent(content, id string) error {
	_, err := s.bindPolicy(content, id, principalARN{kind: "user", original: "template-validation"})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return nil
}

// decodePolicy checks a stored policy's key, owner, and revision against its payload.
// It does not validate policy content.
func (s *Store) decodePolicy(item map[string]types.AttributeValue) (StoredPolicy, error) {
	var policy StoredPolicy
	revision, err := decodeItem(item, &policy)
	if err != nil {
		return StoredPolicy{}, err
	}
	owner, err := s.policyOwner(PolicyRef{ID: policy.ID, Kind: policy.Kind})
	if err != nil || policy.OwnerAccountID != owner || policy.Revision != revision || !matchesStoreKey(item, "ACCOUNT#"+owner, "POLICY#"+policy.ID) {
		return StoredPolicy{}, fmt.Errorf("inconsistent policy record")
	}
	return policy, nil
}

// GetPolicy returns a custom or managed policy identified by ref.
// It returns ErrNotFound if the policy does not exist.
func (s *Store) GetPolicy(ctx context.Context, ref PolicyRef) (StoredPolicy, error) {
	owner, err := s.policyOwner(ref)
	if err != nil {
		return StoredPolicy{}, err
	}
	item, err := s.readItem(ctx, s.config.PolicyTable, "ACCOUNT#"+owner, "POLICY#"+ref.ID)
	if err != nil {
		return StoredPolicy{}, err
	}
	return s.decodePolicy(item)
}

// CreatePolicy creates an account-owned custom policy and returns it with a new revision.
// It returns ErrConflict if a policy with that ID already exists.
func (s *Store) CreatePolicy(ctx context.Context, id, content string) (StoredPolicy, error) {
	return s.writePolicy(ctx, id, "", content)
}

// UpdatePolicy replaces a custom policy's content and returns it with a new revision.
// It returns ErrConflict if revision no longer matches the stored record.
func (s *Store) UpdatePolicy(ctx context.Context, id, revision, content string) (StoredPolicy, error) {
	if !isRevision(revision) {
		return StoredPolicy{}, fmt.Errorf("%w: expected revision is required", ErrValidation)
	}
	return s.writePolicy(ctx, id, revision, content)
}

// writePolicy creates or updates a custom policy in the assigned writer region.
// Updates require the expected revision.
func (s *Store) writePolicy(ctx context.Context, id, expected, content string) (StoredPolicy, error) {
	if err := s.requireWriter(); err != nil {
		return StoredPolicy{}, err
	}
	if !isStoreID(id) {
		return StoredPolicy{}, fmt.Errorf("%w: invalid policy ID", ErrValidation)
	}
	if err := s.checkContent(content, id); err != nil {
		return StoredPolicy{}, err
	}
	policy := StoredPolicy{ID: id, OwnerAccountID: s.config.AccountID, Kind: CustomPolicy, Content: content, Revision: uuid.NewString()}
	item, err := storeItem("ACCOUNT#"+policy.OwnerAccountID, "POLICY#"+id, policy.Revision, policy)
	if err != nil {
		return StoredPolicy{}, err
	}
	condition := conditionCreateOnly
	var values map[string]types.AttributeValue
	if expected != "" {
		condition = conditionMatchRevision
		values = map[string]types.AttributeValue{":expected": &types.AttributeValueMemberS{Value: expected}}
	}
	_, err = s.client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.config.PolicyTable, Item: item, ConditionExpression: &condition, ExpressionAttributeValues: values})
	if err != nil {
		return StoredPolicy{}, storeError(err)
	}
	return policy, nil
}

// DeletePolicy deletes a custom policy only if revision matches the stored record.
// Attachments referencing the policy are left unchanged.
func (s *Store) DeletePolicy(ctx context.Context, id, revision string) error {
	if err := s.requireWriter(); err != nil {
		return err
	}
	if !isStoreID(id) || !isRevision(revision) {
		return fmt.Errorf("%w: valid policy ID and expected revision required", ErrValidation)
	}
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.config.PolicyTable, Key: storeKey("ACCOUNT#"+s.config.AccountID, "POLICY#"+id), ConditionExpression: aws.String(conditionMatchRevision), ExpressionAttributeValues: map[string]types.AttributeValue{":expected": &types.AttributeValueMemberS{Value: revision}}})
	return storeError(err)
}
