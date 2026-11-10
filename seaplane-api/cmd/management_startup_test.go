package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// setStartupTables supplies the table env vars a valid startup requires.
func setStartupTables(t *testing.T) {
	t.Setenv("AUTHZ_POLICY_TABLE", "policies")
	t.Setenv("AUTHZ_GLOBAL_TABLE", "global-attachments")
	t.Setenv("AUTHZ_REGIONAL_TABLE", "regional-attachments")
}

// TestManagementStartup requires three distinct table env vars.
func TestManagementStartup(t *testing.T) {
	for _, tc := range []struct {
		name, regionalTable string
		fail                bool
	}{
		{"tables", "regional-attachments", false},
		{"missing table", "", true},
		{"duplicate table", "policies", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setStartupTables(t)
			t.Setenv("AUTHZ_REGIONAL_TABLE", tc.regionalTable)
			management, err := loadAuthorization(aws.Config{Region: "us-east-1"})
			if (err != nil) != tc.fail || (management == nil) != tc.fail {
				t.Fatalf("management=%v error=%v", management, err)
			}
		})
	}
}
