package authz

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// queryItems reads every page for a partition key with strong local consistency.
// It returns an error instead of partial results if a query fails.
func (s *Store) queryItems(ctx context.Context, table, pk string) ([]map[string]types.AttributeValue, error) {
	input := &dynamodb.QueryInput{TableName: &table, ConsistentRead: aws.Bool(true), Limit: aws.Int32(storePageSize), KeyConditionExpression: aws.String("pk = :pk"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: pk}}}
	items := []map[string]types.AttributeValue{}
	for {
		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, backendError(err)
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items, nil
		}
		input.ExclusiveStartKey = out.LastEvaluatedKey
	}
}

// ListPolicies returns the account's custom policies and the platform's managed policies.
func (s *Store) ListPolicies(ctx context.Context) ([]StoredPolicy, error) {
	policies := []StoredPolicy{}
	for _, owner := range []string{s.config.AccountID, managedOwner} {
		items, err := s.queryItems(ctx, s.config.PolicyTable, "ACCOUNT#"+owner)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			policy, err := s.decodePolicy(item)
			if err != nil {
				return nil, err
			}
			policies = append(policies, policy)
		}
	}
	return policies, nil
}

// readBindings returns validated attachments for a scope and account or principal lookup key.
// It returns an error if any record is malformed or inconsistent.
func (s *Store) readBindings(ctx context.Context, scope AttachmentScope, pk string) ([]StoredAttachment, error) {
	table, err := s.attachmentTable(scope)
	if err != nil {
		return nil, err
	}
	items, err := s.queryItems(ctx, table, pk)
	if err != nil {
		return nil, err
	}
	bindings := []StoredAttachment{}
	for _, item := range items {
		var binding StoredAttachment
		revision, err := decodeItem(item, &binding)
		if err != nil {
			return nil, err
		}
		if binding.Scope != scope || binding.Revision != revision {
			return nil, fmt.Errorf("inconsistent attachment record")
		}
		if err := s.checkAttachment(binding); err != nil {
			return nil, err
		}
		principal, err := parsePrincipal(binding.PrincipalARN)
		if err != nil {
			return nil, err
		}
		if !matchesStoreKey(item, pk, "ATTACHMENT#"+binding.ID) || (pk != "ACCOUNT#"+s.config.AccountID && pk != principalKey(principal)) {
			return nil, fmt.Errorf("attachment lookup key mismatch")
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// ListAttachments returns the account's global attachments and attachments for the store's region.
func (s *Store) ListAttachments(ctx context.Context) ([]StoredAttachment, error) {
	bindings := []StoredAttachment{}
	for _, scope := range []AttachmentScope{GlobalScope, RegionalScope} {
		part, err := s.readBindings(ctx, scope, "ACCOUNT#"+s.config.AccountID)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, part...)
	}
	return bindings, nil
}
