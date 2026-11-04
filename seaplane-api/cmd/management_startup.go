package main

import (
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/authz"
	"github.com/spf13/cobra"
)

// loadManagement validates an optional management configuration before database connections or listeners.
// The existing AWS configuration preserves credentials and SDK endpoint overrides, including local DynamoDB.
func loadManagement(cmd *cobra.Command, cfg aws.Config) (*authz.Management, error) {
	path, err := cmd.Flags().GetString("authz-management-config-file")
	if err != nil {
		return nil, err
	}
	if !cmd.Flags().Changed("authz-management-config-file") {
		path = os.Getenv("AUTHZ_MANAGEMENT_CONFIG_FILE")
	}
	if path == "" {
		return nil, nil
	}
	managementConfig, err := authz.LoadManagementConfig(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load authorization management config: %w", err)
	}
	management, err := authz.NewManagement(dynamodb.NewFromConfig(cfg), managementConfig, cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize authorization management: %w", err)
	}
	return management, nil
}
