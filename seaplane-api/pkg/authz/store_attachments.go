package authz

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/google/uuid"
)

// AttachmentScope identifies whether an attachment applies globally or to one region.
type AttachmentScope string

const (
	// GlobalScope applies an attachment across regions.
	GlobalScope AttachmentScope = "global"
	// RegionalScope applies an attachment only in its stored region.
	RegionalScope AttachmentScope = "regional"
)

// AttachmentSpec links a policy reference to a principal ARN and a global or regional scope.
type AttachmentSpec struct {
	Policy       PolicyRef
	PrincipalARN string
	Scope        AttachmentScope
}

// StoredAttachment contains an attachment specification, ID, owning account, region, and revision.
// Region is empty for global attachments and identifies the region for regional attachments.
type StoredAttachment struct {
	AttachmentSpec
	ID, OwnerAccountID, Region, Revision string
}

// attachmentTable returns the table name for a global or regional attachment scope.
func (s *Store) attachmentTable(scope AttachmentScope) (string, error) {
	switch scope {
	case GlobalScope:
		return s.config.GlobalTable, nil
	case RegionalScope:
		return s.config.RegionalTable, nil
	default:
		return "", fmt.Errorf("%w: invalid attachment scope", ErrValidation)
	}
}

// checkAttachment checks the attachment's identity, account ownership, policy reference, and scope.
// Regional attachments must belong to the store's region.
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

// principalKey builds an account-scoped lookup key for a principal.
func principalKey(principal principalARN) string {
	if principal.kind == "role" {
		return roleKey(principal)
	}
	return "ACCOUNT#" + principal.account + "#EXACT#" + principal.original
}

// roleKey returns an account-scoped lookup key shared by an IAM role and its assumed-role sessions.
func roleKey(principal principalARN) string {
	return "ACCOUNT#" + principal.account + "#ROLE#" + principal.roleAlias()
}

// GetAttachment returns the account's attachment for the given scope and ID.
// It returns ErrNotFound if the attachment does not exist.
func (s *Store) GetAttachment(ctx context.Context, scope AttachmentScope, id string) (StoredAttachment, error) {
	table, err := s.attachmentTable(scope)
	if err != nil {
		return StoredAttachment{}, err
	}
	if !isStoreID(id) {
		return StoredAttachment{}, fmt.Errorf("%w: invalid attachment ID", ErrValidation)
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

// CreateAttachment creates a policy attachment with a generated ID and revision.
func (s *Store) CreateAttachment(ctx context.Context, spec AttachmentSpec) (StoredAttachment, error) {
	binding := StoredAttachment{AttachmentSpec: spec, ID: uuid.NewString(), OwnerAccountID: s.config.AccountID, Revision: uuid.NewString()}
	if spec.Scope == RegionalScope {
		binding.Region = s.config.Region
	}
	return s.writeAttachment(ctx, binding, "")
}

// UpdateAttachment changes an attachment's policy reference and returns the updated attachment.
// It returns ErrConflict if expected does not match the stored revision.
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

// writeAttachment saves an attachment and its principal lookup record in one transaction.
// It checks the attachment's expected revision and the referenced policy's revision.
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
		return StoredAttachment{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	policy, err := s.GetPolicy(ctx, binding.Policy)
	if err != nil {
		return StoredAttachment{}, err
	}
	// Validate the policy content before attaching it.
	if err := s.checkContent(policy.Content, policy.ID); err != nil {
		return StoredAttachment{}, err
	}
	principal, err := parsePrincipal(binding.PrincipalARN)
	if err != nil {
		return StoredAttachment{}, err
	}
	condition := conditionCreateOnly
	var values map[string]types.AttributeValue
	if expected != "" {
		condition = conditionMatchRevision
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

// DeleteAttachment deletes an attachment and its principal lookup record.
// It returns ErrConflict if expected does not match the stored revision.
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
		writes = append(writes, types.TransactWriteItem{Delete: &types.Delete{TableName: &table, Key: storeKey(pk, "ATTACHMENT#"+id), ConditionExpression: aws.String(conditionMatchRevision), ExpressionAttributeValues: map[string]types.AttributeValue{":expected": &types.AttributeValueMemberS{Value: expected}}}})
	}
	_, err = s.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: writes})
	return storeError(err)
}
