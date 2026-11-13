package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
)

// loadAuthorization builds the authorization service before database connections or listeners.
// The existing AWS configuration preserves credentials and SDK endpoint overrides, including local DynamoDB.
func loadAuthorization(cfg aws.Config) (*authz.Service, error) {
	serviceConfig := authz.ServiceConfig{
		PolicyTable:   os.Getenv("AUTHZ_POLICY_TABLE"),
		GlobalTable:   os.Getenv("AUTHZ_GLOBAL_TABLE"),
		RegionalTable: os.Getenv("AUTHZ_REGIONAL_TABLE"),
	}
	authorization, err := authz.NewService(dynamodb.NewFromConfig(cfg), serviceConfig, cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize authorization service (AUTHZ_*_TABLE): %w", err)
	}
	return authorization, nil
}
