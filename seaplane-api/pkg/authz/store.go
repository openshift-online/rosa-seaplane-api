package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/cedar-policy/cedar-go/x/exp/schema/resolved"
	"github.com/cedar-policy/cedar-go/x/exp/schema/validate"
	"github.com/google/uuid"
)

var (
	ErrConflict    = errors.New("authorization record revision conflict")
	ErrNotFound    = errors.New("authorization record not found")
	ErrWrongWriter = errors.New("global mutation requires assigned writer")
)

const (
	storePageSize  int32 = 100
	maxPolicyBytes       = 64 * 1024
	managedOwner         = "MANAGED"
)

type PolicyKind string

const (
	CustomPolicy  PolicyKind = "custom"
	ManagedPolicy PolicyKind = "managed"
)

type PolicyRef struct {
	ID   string
	Kind PolicyKind
}

type StoredPolicy struct {
	ID, OwnerAccountID, Content, Revision string
	Kind                                  PolicyKind
}

// StoreConfig identifies an account's authorization tables, local region, and assigned writer region.
// It contains trusted configuration, not request input.
type StoreConfig struct {
	AccountID, Region, WriterRegion         string
	PolicyTable, GlobalTable, RegionalTable string
}

// Store reads and writes DynamoDB authorization records for one account.
// Callers are responsible for authentication and management authorization.
type Store struct {
	client    *dynamodb.Client
	config    StoreConfig
	model     *resolved.Schema
	validator *validate.Validator
}

func NewStore(client *dynamodb.Client, config StoreConfig) (*Store, error) {
	if client == nil || !accountPattern.MatchString(config.AccountID) || !regionPattern.MatchString(config.Region) || !regionPattern.MatchString(config.WriterRegion) {
		return nil, fmt.Errorf("invalid authorization store configuration")
	}
	tables := map[string]bool{}
	for _, table := range []string{config.PolicyTable, config.GlobalTable, config.RegionalTable} {
		if table == "" || tables[table] {
			return nil, fmt.Errorf("authorization stores require three distinct tables")
		}
		tables[table] = true
	}
	model, validator, err := loadSchema()
	if err != nil {
		return nil, err
	}
	return &Store{client: client, config: config, model: model, validator: validator}, nil
}

func (s *Store) requireWriter() error {
	if s.config.Region != s.config.WriterRegion {
		return ErrWrongWriter
	}
	return nil
}

func isStoreID(id string) bool {
	return len(id) <= 128 && idPattern.MatchString(id)
}

func isRevision(revision string) bool {
	return revision != uuid.Nil.String() && uuid.Validate(revision) == nil
}

// storeKey builds a DynamoDB partition and sort key.
func storeKey(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: pk}, "sk": &types.AttributeValueMemberS{Value: sk}}
}

func matchesStoreKey(item map[string]types.AttributeValue, pk, sk string) bool {
	storedPK, hasPK := item["pk"].(*types.AttributeValueMemberS)
	storedSK, hasSK := item["sk"].(*types.AttributeValueMemberS)
	return hasPK && hasSK && storedPK.Value == pk && storedSK.Value == sk
}

func storeItem(pk, sk, revision string, record any) (map[string]types.AttributeValue, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	item := storeKey(pk, sk)
	item["revision"] = &types.AttributeValueMemberS{Value: revision}
	item["record"] = &types.AttributeValueMemberS{Value: string(payload)}
	return item, nil
}

func decodeItem(item map[string]types.AttributeValue, record any) (string, error) {
	payload, ok := item["record"].(*types.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("malformed authorization record")
	}
	revision, ok := item["revision"].(*types.AttributeValueMemberS)
	if !ok || !isRevision(revision.Value) {
		return "", fmt.Errorf("malformed authorization revision")
	}
	if err := json.Unmarshal([]byte(payload.Value), record); err != nil {
		return "", err
	}
	return revision.Value, nil
}

// readItem reads a record with strong local consistency and checks its key.
func (s *Store) readItem(ctx context.Context, table, pk, sk string) (map[string]types.AttributeValue, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &table, Key: storeKey(pk, sk), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, ErrNotFound
	}
	if !matchesStoreKey(out.Item, pk, sk) {
		return nil, fmt.Errorf("authorization record key mismatch")
	}
	return out.Item, nil
}

// storeError maps failed write conditions to ErrConflict and preserves other errors.
func storeError(err error) error {
	var conflict *types.ConditionalCheckFailedException
	if errors.As(err, &conflict) {
		return ErrConflict
	}
	var canceled *types.TransactionCanceledException
	if errors.As(err, &canceled) {
		for _, reason := range canceled.CancellationReasons {
			if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
				return ErrConflict
			}
		}
	}
	return err
}
