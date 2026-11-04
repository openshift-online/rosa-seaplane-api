package main

import (
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-api/pkg/config"
	"github.com/spf13/cobra"
)

func TestCustomerAccessFlags(t *testing.T) {
	var identity config.CustomerAccessConfig
	cmd := &cobra.Command{}
	registerCustomerAccessFlags(cmd, &identity)
	if identity != config.NewConfig().CustomerAccess {
		t.Fatal("flag defaults differ from configuration")
	}
	if err := cmd.ParseFlags([]string{"--regional-workload-issuer-url=https://issuer.example/id", "--regional-workload-subject=system:serviceaccount:ns:sa", "--customer-access-token-file=/token", "--regional-workload-contract-version=v2"}); err != nil {
		t.Fatal(err)
	}
	if identity.IssuerURL != "https://issuer.example/id" || identity.Subject != "system:serviceaccount:ns:sa" || identity.TokenFile != "/token" || identity.ContractVersion != "v2" {
		t.Fatalf("wrong flag values: %+v", identity)
	}
}
