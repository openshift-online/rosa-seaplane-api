package iam

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// BuildEC2TrustPolicy returns the trust policy for the worker node instance
// role, which is trusted by the EC2 service (not OIDC web identity).
func BuildEC2TrustPolicy() string {
	return `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Service": ["ec2.amazonaws.com"]
    },
    "Action": "sts:AssumeRole"
  }]
}`
}

// BuildWebIdentityTrustPolicy returns the OIDC web-identity trust policy
// for an operator role, conditioned on the role's service accounts (sub)
// and the "openshift" audience. No client should hand-write this JSON.
func BuildWebIdentityTrustPolicy(input TrustPolicyInput, role OperatorRoleSpec) (string, error) {
	if role.IsWorkerRole {
		return "", fmt.Errorf("role %q is the worker role; use BuildEC2TrustPolicy", role.Name)
	}
	if len(role.ServiceAccounts) == 0 {
		return "", fmt.Errorf("role %q has no service accounts", role.Name)
	}

	issuerDomain, err := IssuerDomain(input.IssuerURL)
	if err != nil {
		return "", err
	}

	subject, err := OIDCSubject(role.ServiceAccounts)
	if err != nil {
		return "", fmt.Errorf("building trust policy for role %q: %w", role.Name, err)
	}

	return fmt.Sprintf(`{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {
      "Federated": "arn:%s:iam::%s:oidc-provider/%s"
    },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "%s:sub": %s,
        "%s:aud": "openshift"
      }
    }
  }]
}`,
		input.Partition,
		input.AccountID,
		issuerDomain,
		issuerDomain,
		subject,
		issuerDomain,
	), nil
}

// OIDCSubject renders the trust-policy "sub" condition value: a JSON string
// for a single service account, a JSON array for multiple — matching AWS
// StringEquals semantics and the V1/V2 CLI behavior.
func OIDCSubject(serviceAccounts []string) (string, error) {
	if len(serviceAccounts) == 0 {
		return "", fmt.Errorf("no service accounts configured")
	}
	var subject any
	if len(serviceAccounts) == 1 {
		subject = serviceAccounts[0]
	} else {
		subject = serviceAccounts
	}
	encoded, err := json.Marshal(subject)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// IssuerDomain derives the OIDC issuer domain (host + path, without the
// scheme) from the issuer URL. This matches how the AWS OIDC provider is
// registered, so the trust policy always references the provider that was
// actually created. A naive scheme-strip would diverge whenever the issuer
// carries a query string, fragment, or trailing slash.
func IssuerDomain(issuerURL string) (string, error) {
	parsed, err := url.ParseRequestURI(issuerURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("expected OIDC issuer URL '%s' to use an https:// scheme", issuerURL)
	}
	return parsed.Host + parsed.Path, nil
}

// ManagedPolicyARNs renders the full policy ARNs for a role in the given
// AWS partition, e.g. "arn:aws:iam::aws:policy/service-role/ROSAIngressOperatorPolicy".
// Definitions store partition-relative policy paths for this reason.
func ManagedPolicyARNs(role OperatorRoleSpec, partition string) []string {
	arns := make([]string, len(role.ManagedPolicies))
	for i, path := range role.ManagedPolicies {
		arns[i] = fmt.Sprintf("arn:%s:iam::aws:policy/%s", partition, strings.TrimPrefix(path, "/"))
	}
	return arns
}
