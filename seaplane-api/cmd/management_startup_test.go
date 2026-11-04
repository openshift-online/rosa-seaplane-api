package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

const startupManagement = `formatVersion: 1
policyTable: policies
globalTable: global-attachments
regionalTable: regional-attachments
accounts:
  "123456789012":
    writerRegion: us-east-1
    administratorARNs:
      - arn:aws:iam::123456789012:role/admin
`

// TestManagementStartup verifies optional configuration, environment precedence, and fail-closed supplied files.
func TestManagementStartup(t *testing.T) {
	if serveCmd.Flags().Lookup("authz-management-config-file") == nil {
		t.Fatal("management flag missing")
	}
	valid := startupFile(t, startupManagement)
	for _, tc := range []struct {
		name, env  string
		args       []string
		want, fail bool
	}{
		{"absent", "", nil, false, false},
		{"environment", valid, nil, true, false},
		{"bad file", "/nonexistent/management.yaml", nil, false, true},
		{"bad content", startupFile(t, "formatVersion: 99"), nil, false, true},
		{"flag precedence", "/missing", []string{"--authz-management-config-file=" + valid}, true, false},
		{"explicit empty", valid, []string{"--authz-management-config-file="}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AUTHZ_MANAGEMENT_CONFIG_FILE", tc.env)
			cmd := startupCommand(t, tc.args...)
			management, err := loadManagement(cmd, aws.Config{Region: "us-east-1"})
			if (err != nil) != tc.fail || (management != nil) != tc.want {
				t.Fatalf("management=%v error=%v", management, err)
			}
		})
	}
}
