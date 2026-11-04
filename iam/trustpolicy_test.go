package iam

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildEC2TrustPolicy(t *testing.T) {
	want := `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Service": ["ec2.amazonaws.com"]
    },
    "Action": "sts:AssumeRole"
  }]
}`
	got := BuildEC2TrustPolicy()
	if got != want {
		t.Fatalf("EC2 trust policy mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
	// Must be valid JSON.
	var doc map[string]any
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("EC2 trust policy is not valid JSON: %v", err)
	}
}

// TestBuildWebIdentityTrustPolicyGolden pins the exact trust-policy shape
// produced by the rosa CLI's by_prefix_seaplane.go (D2 contract).
func TestBuildWebIdentityTrustPolicyGolden(t *testing.T) {
	set := DefaultDefinitionSet()
	ingress, _ := set.Role(SuffixIngress)

	want := `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.s3.us-east-1.amazonaws.com/abc123"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "oidc.s3.us-east-1.amazonaws.com/abc123:sub": "system:serviceaccount:openshift-ingress-operator:ingress-operator",
        "oidc.s3.us-east-1.amazonaws.com/abc123:aud": "openshift"
      }
    }
  }]
}`

	got, err := BuildWebIdentityTrustPolicy(TrustPolicyInput{
		Partition: "aws",
		AccountID: "123456789012",
		IssuerURL: "https://oidc.s3.us-east-1.amazonaws.com/abc123",
	}, ingress)
	if err != nil {
		t.Fatalf("BuildWebIdentityTrustPolicy: %v", err)
	}
	if got != want {
		t.Fatalf("trust policy mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
	if err := json.Unmarshal([]byte(got), &map[string]any{}); err != nil {
		t.Fatalf("trust policy is not valid JSON: %v", err)
	}
}

func TestBuildWebIdentityTrustPolicyMultipleServiceAccounts(t *testing.T) {
	set := DefaultDefinitionSet()
	registry, _ := set.Role(SuffixImageRegistry)

	got, err := BuildWebIdentityTrustPolicy(TrustPolicyInput{
		Partition: "aws",
		AccountID: "123456789012",
		IssuerURL: "https://oidc.example.com/issuer",
	}, registry)
	if err != nil {
		t.Fatalf("BuildWebIdentityTrustPolicy: %v", err)
	}
	// sub must be a JSON array for multiple service accounts.
	if !strings.Contains(got, `"oidc.example.com/issuer:sub": ["system:serviceaccount:openshift-image-registry:cluster-image-registry-operator","system:serviceaccount:openshift-image-registry:registry"]`) {
		t.Fatalf("expected array sub condition, got:\n%s", got)
	}
}

func TestBuildWebIdentityTrustPolicyWorkerRoleRejected(t *testing.T) {
	set := DefaultDefinitionSet()
	worker, _ := set.Role(SuffixWorkerRole)
	if _, err := BuildWebIdentityTrustPolicy(TrustPolicyInput{
		Partition: "aws",
		AccountID: "123456789012",
		IssuerURL: "https://oidc.example.com",
	}, worker); err == nil {
		t.Fatal("expected error building web-identity trust policy for worker role")
	}
}

func TestBuildWebIdentityTrustPolicyBadIssuer(t *testing.T) {
	set := DefaultDefinitionSet()
	ingress, _ := set.Role(SuffixIngress)
	input := TrustPolicyInput{Partition: "aws", AccountID: "123456789012"}

	if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, "http://insecure.example.com"), ingress); err == nil {
		t.Fatal("expected error for non-https issuer URL")
	}
	if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, "://not-a-url"), ingress); err == nil {
		t.Fatal("expected error for unparseable issuer URL")
	}
	// A quote in the issuer path must be rejected, not interpolated into the
	// trust-policy JSON (would produce malformed JSON / extra keys).
	if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, "https://example.com/a\",\"x\":\"y"), ingress); err == nil {
		t.Fatal("expected error for quote in issuer path")
	}
	// Backslash is the other JSON-unsafe character.
	if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, "https://example.com/a\\b"), ingress); err == nil {
		t.Fatal("expected error for backslash in issuer path")
	}
	// RFC 3986 sub-delims and ':'/'@' are valid in issuer paths and must be
	// accepted (e.g. multi-tenant IdPs use 'tenant=blue').
	for _, issuer := range []string{
		"https://oidc.example.com/tenant=blue",
		"https://oidc.example.com/a@b+c;d,e!f",
		"https://oidc.example.com/a$b&c(d)e",
		"https://oidc.example.com/a'b*c",
	} {
		if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, issuer), ingress); err != nil {
			t.Errorf("expected issuer URL %q to be accepted: %v", issuer, err)
		}
	}
	if _, err := BuildWebIdentityTrustPolicy(withIssuer(input, "https:///no-host"), ingress); err == nil {
		t.Fatal("expected error for issuer URL with empty host")
	}
}

func TestBuildWebIdentityTrustPolicyInvalidInput(t *testing.T) {
	set := DefaultDefinitionSet()
	ingress, _ := set.Role(SuffixIngress)

	tests := []struct {
		name  string
		input TrustPolicyInput
	}{
		{name: "bad partition", input: TrustPolicyInput{Partition: "aws; rm -rf", AccountID: "123456789012", IssuerURL: "https://oidc.example.com"}},
		{name: "empty partition", input: TrustPolicyInput{Partition: "", AccountID: "123456789012", IssuerURL: "https://oidc.example.com"}},
		{name: "bad account ID", input: TrustPolicyInput{Partition: "aws", AccountID: "not-an-account", IssuerURL: "https://oidc.example.com"}},
		{name: "short account ID", input: TrustPolicyInput{Partition: "aws", AccountID: "12345", IssuerURL: "https://oidc.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildWebIdentityTrustPolicy(tt.input, ingress); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}

	// Valid GovCloud partition still accepted.
	if _, err := BuildWebIdentityTrustPolicy(TrustPolicyInput{
		Partition: "aws-us-gov", AccountID: "123456789012", IssuerURL: "https://oidc.example.com",
	}, ingress); err != nil {
		t.Fatalf("expected aws-us-gov partition to be accepted: %v", err)
	}
}

func withIssuer(in TrustPolicyInput, issuer string) TrustPolicyInput {
	in.IssuerURL = issuer
	return in
}

func TestOIDCSubject(t *testing.T) {
	single, err := OIDCSubject([]string{"sa-one"})
	if err != nil {
		t.Fatalf("OIDCSubject: %v", err)
	}
	if single != `"sa-one"` {
		t.Fatalf("expected %q, got %q", `"sa-one"`, single)
	}

	multi, err := OIDCSubject([]string{"sa-one", "sa-two"})
	if err != nil {
		t.Fatalf("OIDCSubject: %v", err)
	}
	if multi != `["sa-one","sa-two"]` {
		t.Fatalf("expected %q, got %q", `["sa-one","sa-two"]`, multi)
	}

	if _, err := OIDCSubject(nil); err == nil {
		t.Fatal("expected error for empty service accounts")
	}
}

func TestIssuerDomain(t *testing.T) {
	tests := []struct {
		issuerURL string
		want      string
		wantErr   bool
	}{
		{issuerURL: "https://oidc.s3.us-east-1.amazonaws.com/abc", want: "oidc.s3.us-east-1.amazonaws.com/abc"},
		{issuerURL: "https://example.com/nested/path", want: "example.com/nested/path"},
		{issuerURL: "https://example.com/tenant=blue", want: "example.com/tenant=blue"},
		// A trailing slash is preserved verbatim: the domain must match the
		// OIDC provider registration exactly, and this mirrors the rosa CLI's
		// behavior (design doc: naive scheme/path normalization would diverge
		// from the provider that was actually created).
		{issuerURL: "https://example.com/path/", want: "example.com/path/"},
		{issuerURL: "http://example.com", wantErr: true},
		{issuerURL: "https:///no-host", wantErr: true},
		{issuerURL: "not-a-url", wantErr: true},
	}
	for _, tt := range tests {
		got, err := IssuerDomain(tt.issuerURL)
		if (err != nil) != tt.wantErr {
			t.Fatalf("IssuerDomain(%q) error = %v, wantErr %v", tt.issuerURL, err, tt.wantErr)
		}
		if err == nil && got != tt.want {
			t.Errorf("IssuerDomain(%q) = %q, want %q", tt.issuerURL, got, tt.want)
		}
	}
}

func TestManagedPolicyARNs(t *testing.T) {
	set := DefaultDefinitionSet()
	ingress, _ := set.Role(SuffixIngress)

	got := ManagedPolicyARNs(ingress, "aws")
	want := []string{"arn:aws:iam::aws:policy/service-role/ROSAIngressOperatorPolicy"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}

	gov := ManagedPolicyARNs(ingress, "aws-us-gov")
	if gov[0] != "arn:aws-us-gov:iam::aws:policy/service-role/ROSAIngressOperatorPolicy" {
		t.Fatalf("unexpected GovCloud ARN: %s", gov[0])
	}

	worker, _ := set.Role(SuffixWorkerRole)
	if got := ManagedPolicyARNs(worker, "aws"); len(got) != 2 {
		t.Fatalf("expected 2 worker policies, got %v", got)
	}
}

// TestAllRolesBuildTrustPolicies ensures every role in the embedded set can
// produce a valid trust policy (guards future definition additions).
func TestAllRolesBuildTrustPolicies(t *testing.T) {
	set := DefaultDefinitionSet()
	input := TrustPolicyInput{
		Partition: "aws",
		AccountID: "123456789012",
		IssuerURL: "https://oidc.s3.us-east-1.amazonaws.com/test",
	}
	for _, role := range set.Roles {
		var doc string
		var err error
		if role.IsWorkerRole {
			doc = BuildEC2TrustPolicy()
		} else {
			doc, err = BuildWebIdentityTrustPolicy(input, role)
			if err != nil {
				t.Fatalf("role %q: %v", role.Name, err)
			}
		}
		if err := json.Unmarshal([]byte(doc), &map[string]any{}); err != nil {
			t.Fatalf("role %q produced invalid JSON: %v", role.Name, err)
		}
	}
}
