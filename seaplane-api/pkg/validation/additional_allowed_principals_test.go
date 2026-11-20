package validation

import (
	"fmt"
	"strings"
	"testing"

	"github.com/openshift-online/rosa-seaplane-api/featuregate"
)

func TestValidateAdditionalAllowedPrincipals(t *testing.T) {
	const path = additionalAllowedPrincipalsPath
	validPrincipals := []any{
		"arn:aws:iam::123456789012:root",
		"arn:aws:iam::123456789012:role/rosa/worker-role",
		"arn:aws:iam::123456789012:user/customer.user",
		"arn:aws-us-gov:iam::123456789012:federated-user/customer-user",
	}

	tooMany := make([]any, 26)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("arn:aws:iam::%012d:root", i+1)
	}
	longPrincipal := "arn:aws:iam::123456789012:role/" + strings.Repeat("a", 230)

	tests := []struct {
		name      string
		fields    map[string]any
		wantError bool
		wantText  string
	}{
		{name: "field absent", fields: map[string]any{}},
		{name: "empty list", fields: map[string]any{path: []any{}}},
		{name: "supported principal types", fields: map[string]any{path: validPrincipals}},
		{name: "not an array", fields: map[string]any{path: "arn:aws:iam::123456789012:root"}, wantError: true, wantText: "must be an array"},
		{name: "too many principals", fields: map[string]any{path: tooMany}, wantError: true, wantText: "more than 25"},
		{name: "entry is not a string", fields: map[string]any{path: []any{42}}, wantError: true, wantText: "entry 0"},
		{name: "malformed ARN", fields: map[string]any{path: []any{"not-an-arn"}}, wantError: true, wantText: "entry 0"},
		{name: "unsupported partition", fields: map[string]any{path: []any{"arn:aws_bad:iam::123456789012:root"}}, wantError: true, wantText: "entry 0"},
		{name: "unsupported service", fields: map[string]any{path: []any{"arn:aws:s3:::bucket"}}, wantError: true, wantText: "entry 0"},
		{name: "regional IAM ARN", fields: map[string]any{path: []any{"arn:aws:iam:us-east-1:123456789012:root"}}, wantError: true, wantText: "entry 0"},
		{name: "invalid account ID", fields: map[string]any{path: []any{"arn:aws:iam::1234:root"}}, wantError: true, wantText: "entry 0"},
		{name: "unsupported principal type", fields: map[string]any{path: []any{"arn:aws:iam::123456789012:group/engineering"}}, wantError: true, wantText: "entry 0"},
		{name: "invalid principal name", fields: map[string]any{path: []any{"arn:aws:iam::123456789012:role/bad:name"}}, wantError: true, wantText: "entry 0"},
		{name: "overlong ARN", fields: map[string]any{path: []any{longPrincipal}}, wantError: true, wantText: "255-character"},
		{name: "duplicate principal", fields: map[string]any{path: []any{
			"arn:aws:iam::123456789012:root",
			"arn:aws:iam::123456789012:root",
		}}, wantError: true, wantText: "duplicates principal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAdditionalAllowedPrincipals(tt.fields)
			if !tt.wantError {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if err.Field != path {
				t.Errorf("expected error field %q, got %q", path, err.Field)
			}
			if !strings.Contains(err.Reason, tt.wantText) {
				t.Errorf("expected reason to contain %q, got %q", tt.wantText, err.Reason)
			}
		})
	}
}

func TestFieldValidator_ValidatesAdditionalAllowedPrincipals(t *testing.T) {
	validator := NewFieldValidator("Cluster")
	spec := map[string]any{
		"hostedCluster": map[string]any{
			"platform": map[string]any{
				"aws": map[string]any{
					"additionalAllowedPrincipals": []any{"not-an-arn"},
				},
			},
		},
	}

	if errs := validator.ValidateCreate(spec, featuregate.Default); len(errs) != 1 || errs[0].Field != additionalAllowedPrincipalsPath {
		t.Fatalf("expected create validation error for %s, got %v", additionalAllowedPrincipalsPath, errs)
	}
	if errs := validator.ValidateUpdate(spec, nil, featuregate.Default); len(errs) != 1 || errs[0].Field != additionalAllowedPrincipalsPath {
		t.Fatalf("expected update validation error for %s, got %v", additionalAllowedPrincipalsPath, errs)
	}

	spec["hostedCluster"].(map[string]any)["platform"].(map[string]any)["aws"].(map[string]any)["additionalAllowedPrincipals"] = []any{"arn:aws:iam::123456789012:root"}
	if errs := validator.ValidateCreate(spec, featuregate.Default); errs != nil {
		t.Fatalf("expected valid additional allowed principal to pass, got %v", errs)
	}
}
