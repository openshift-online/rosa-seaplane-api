package authz

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

type AttachmentScope string

const (
	GlobalScope   AttachmentScope = "global"
	RegionalScope AttachmentScope = "regional"
)

type AttachmentSpec struct {
	Policy       PolicyRef
	PrincipalARN string
	Scope        AttachmentScope
}

type StoredAttachment struct {
	AttachmentSpec
	ID, OwnerAccountID, Region, Revision string
}

func (s *Store) attachmentTable(scope AttachmentScope) (string, error) {
	switch scope {
	case GlobalScope:
		return s.config.GlobalTable, nil
	case RegionalScope:
		return s.config.RegionalTable, nil
	default:
		return "", fmt.Errorf("invalid attachment scope")
	}
}

func (s *Store) checkAttachment(binding StoredAttachment) error {
	principal, err := parsePrincipal(binding.PrincipalARN)
	if err != nil {
		return err
	}
	if !isStoreID(binding.ID) || !isRevision(binding.Revision) || binding.OwnerAccountID != s.config.AccountID || principal.account != s.config.AccountID {
		return fmt.Errorf("invalid attachment identity")
	}
	if _, err := s.policyOwner(binding.Policy); err != nil {
		return err
	}
	if err := checkedScope(string(binding.Scope), binding.Region); err != nil {
		return err
	}
	if binding.Scope == RegionalScope && binding.Region != s.config.Region {
		return fmt.Errorf("foreign regional attachment")
	}
	return nil
}

// Role lookup keys deliberately omit paths, matching the existing STS semantics.
func principalKey(principal principalARN) string {
	if principal.kind == "role" {
		return roleKey(principal)
	}
	return "ACCOUNT#" + principal.account + "#EXACT#" + principal.original
}

// roleKey is shared by role writes and session lookups. If the two drift,
// role forbids silently stop resolving.
func roleKey(principal principalARN) string {
	return "ACCOUNT#" + principal.account + "#ROLE#" + principal.roleAlias()
}

func (s *Store) GetAttachment(ctx context.Context, scope AttachmentScope, id string) (StoredAttachment, error) {
	table, err := s.attachmentTable(scope)
	if err != nil {
		return StoredAttachment{}, err
	}
	if !isStoreID(id) {
		return StoredAttachment{}, fmt.Errorf("invalid attachment ID")
	}
	item, err := s.readItem(ctx, table, "ACCOUNT#"+s.config.AccountID, "ATTACHMENT#"+id)
	if err != nil {
		return StoredAttachment{}, err
	}
	var binding StoredAttachment
	revision, err := decodeItem(item, &binding)
	if err != nil {
		return StoredAttachment{}, err
	}
	if binding.ID != id || binding.Scope != scope || binding.Revision != revision {
		return StoredAttachment{}, fmt.Errorf("inconsistent attachment identity")
	}
	if err := s.checkAttachment(binding); err != nil {
		return StoredAttachment{}, err
	}
	return binding, nil
}

// IDs are minted here so independent regions do not share client-selected names.
func (s *Store) CreateAttachment(ctx context.Context, spec AttachmentSpec) (StoredAttachment, error) {
	binding := StoredAttachment{AttachmentSpec: spec, ID: uuid.NewString(), OwnerAccountID: s.config.AccountID, Revision: uuid.NewString()}
	if spec.Scope == RegionalScope {
		binding.Region = s.config.Region
	}
	return s.writeAttachment(ctx, binding, "")
}

func (s *Store) UpdateAttachment(ctx context.Context, scope AttachmentScope, id, expected string, policy PolicyRef) (StoredAttachment, error) {
	binding, err := s.GetAttachment(ctx, scope, id)
	if err != nil {
		return StoredAttachment{}, err
	}
	if !isRevision(expected) || binding.Revision != expected {
		return StoredAttachment{}, ErrConflict
	}
	binding.Policy, binding.Revision = policy, uuid.NewString()
	return s.writeAttachment(ctx, binding, expected)
}

// Keep the management row and the complete principal lookup row in one transaction.
func (s *Store) writeAttachment(ctx context.Context, binding StoredAttachment, expected string) (StoredAttachment, error) {
	table, err := s.attachmentTable(binding.Scope)
	if err != nil {
		return StoredAttachment{}, err
	}
	if binding.Scope == GlobalScope {
		if err := s.requireWriter(); err != nil {
			return StoredAttachment{}, err
		}
	}
	if err := s.checkAttachment(binding); err != nil {
		return StoredAttachment{}, err
	}
	policy, err := s.GetPolicy(ctx, binding.Policy)
	if err != nil {
		return StoredAttachment{}, err
	}
	// Reads skip content validation, so attaching must check the policy can still bind.
	if err := s.checkContent(policy.Content, policy.ID); err != nil {
		return StoredAttachment{}, err
	}
	principal, err := parsePrincipal(binding.PrincipalARN)
	if err != nil {
		return StoredAttachment{}, err
	}
	condition := "attribute_not_exists(pk)"
	values := map[string]types.AttributeValue(nil)
	if expected != "" {
		condition = "revision = :expected"
		values = map[string]types.AttributeValue{":expected": &types.AttributeValueMemberS{Value: expected}}
	}
	writes := []types.TransactWriteItem{}
	for _, pk := range []string{"ACCOUNT#" + s.config.AccountID, principalKey(principal)} {
		item, err := storeItem(pk, "ATTACHMENT#"+binding.ID, binding.Revision, binding)
		if err != nil {
			return StoredAttachment{}, err
		}
		writes = append(writes, types.TransactWriteItem{Put: &types.Put{TableName: &table, Item: item, ConditionExpression: &condition, ExpressionAttributeValues: values}})
	}
	writes = append(writes, types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: &s.config.PolicyTable, Key: storeKey("ACCOUNT#"+policy.OwnerAccountID, "POLICY#"+policy.ID), ConditionExpression: aws.String("revision = :policy"), ExpressionAttributeValues: map[string]types.AttributeValue{":policy": &types.AttributeValueMemberS{Value: policy.Revision}}}})
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: writes})
	if err != nil {
		return StoredAttachment{}, storeError(err)
	}
	return binding, nil
}

func (s *Store) DeleteAttachment(ctx context.Context, scope AttachmentScope, id, expected string) error {
	if scope == GlobalScope {
		if err := s.requireWriter(); err != nil {
			return err
		}
	}
	binding, err := s.GetAttachment(ctx, scope, id)
	if err != nil {
		return err
	}
	if !isRevision(expected) || binding.Revision != expected {
		return ErrConflict
	}
	table, err := s.attachmentTable(scope)
	if err != nil {
		return err
	}
	principal, err := parsePrincipal(binding.PrincipalARN)
	if err != nil {
		return err
	}
	writes := []types.TransactWriteItem{}
	for _, pk := range []string{"ACCOUNT#" + s.config.AccountID, principalKey(principal)} {
		writes = append(writes, types.TransactWriteItem{Delete: &types.Delete{TableName: &table, Key: storeKey(pk, "ATTACHMENT#"+id), ConditionExpression: aws.String("revision = :expected"), ExpressionAttributeValues: map[string]types.AttributeValue{":expected": &types.AttributeValueMemberS{Value: expected}}}})
	}
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: writes})
	return storeError(err)
}
