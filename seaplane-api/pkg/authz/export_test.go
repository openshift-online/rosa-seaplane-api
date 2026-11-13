package authz

import "github.com/aws/aws-sdk-go-v2/service/dynamodb"

// NewTestStore builds one account's store through NewService, the only production constructor.
func NewTestStore(client *dynamodb.Client, cfg StoreConfig) (*Store, error) {
	service, err := NewService(client, ServiceConfig{PolicyTable: cfg.PolicyTable, GlobalTable: cfg.GlobalTable, RegionalTable: cfg.RegionalTable}, cfg.Region)
	if err != nil {
		return nil, err
	}
	return service.store(cfg.AccountID), nil
}
