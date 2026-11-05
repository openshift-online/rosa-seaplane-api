package main

import (
	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/spf13/cobra"
)

func registerCustomerAccessFlags(cmd *cobra.Command, identity *config.CustomerAccessConfig) {
	cmd.Flags().StringVar(&identity.IssuerURL, "regional-workload-issuer-url", "", "Regional EKS service-account issuer URL")
	cmd.Flags().StringVar(&identity.Subject, "regional-workload-subject", "", "Exact regional API service-account subject")
	cmd.Flags().StringVar(&identity.Audience, "regional-workload-audience", config.DefaultWorkloadAudience, "Projected workload token audience")
	cmd.Flags().StringVar(&identity.ContractVersion, "regional-workload-contract-version", config.DefaultWorkloadContractVersion, "Published regional workload identity revision")
	cmd.Flags().StringVar(&identity.TokenFile, "customer-access-token-file", config.DefaultCustomerAccessTokenFile, "Projected customer-access token file")
}
