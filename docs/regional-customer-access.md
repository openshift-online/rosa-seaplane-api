# Regional customer AWS access

The Platform API can discover VPCs in a customer account using its regional
service-account identity. The caller supplies an installer-role ARN with the
inquiry. The API exchanges its projected token through
`AssumeRoleWithWebIdentity`, verifies the resulting account through
`GetCallerIdentity`, and calls EC2 in the service region.

## Deployment

Regional workload identity is required for API startup. Configure it with:

```text
--regional-workload-issuer-url=https://oidc.eks.us-east-1.amazonaws.com/id/ACTUAL_ISSUER_ID
--regional-workload-subject=system:serviceaccount:platform-api:platform-api-sa
--regional-workload-audience=sts.amazonaws.com
--regional-workload-contract-version=regional-workload-v1
--customer-access-token-file=/var/run/seaplane/customer-access/token
```

Use the regional EKS cluster's actual service-account issuer. Derive the subject
from the deployed API namespace and service account. Keep the existing
`--oidc-issuer-base-url` setting for hosted-cluster OIDC document hosting.

Add this volume to the API pod:

```yaml
- name: customer-access-identity
  projected:
    defaultMode: 0444
    sources:
      - serviceAccountToken:
          audience: sts.amazonaws.com
          expirationSeconds: 3600
          path: token
```

Mount it in the API container:

```yaml
- name: customer-access-identity
  mountPath: /var/run/seaplane/customer-access
  readOnly: true
```

Mount the directory without `subPath` so token rotation remains visible. API
startup requires a readable nonempty token file. Preserve the existing AWS
region, authorization bundle, PostgreSQL connection, gateway, and platform Pod
Identity. Do not configure a fixed customer role through `AWS_ROLE_ARN` or a
service-account annotation.

The `rosa-seaplane` repository owns the actual API chart and provisioning.
It must export the EKS issuer and transport it through bootstrap/ArgoCD into
these API settings. That infrastructure wiring is separate from this API
implementation: the new API image requires the issuer settings and token mount
to start. Always render those settings and the volume/mount; there is no enable
flag or conditional route registration.
The operator needs no regional identity configuration for this endpoint.

## Customer setup

Use existing resource-account onboarding and caller authorization. The current
temporary account allowlist grants access to an account's own resources; bundle
enrollment alone grants nothing. Explicit resource grants need
`DescribeRegionalIdentity` and `ListAWSVPCs`, or resource `ReadOnly`/`AllActions`.
Authorization-management routes have separate permissions and do not currently
replace startup-bundle resource grants. See [authorization](authz/README.md).

Fetch identity metadata using customer credentials through the SigV4 gateway:

```bash
CUSTOMER_PROFILE=customer
SERVICE_REGION=us-east-1
API_BASE=https://YOUR_API_ID.execute-api.us-east-1.amazonaws.com/prod

awscurl --profile "$CUSTOMER_PROFILE" --service execute-api --region "$SERVICE_REGION" \
  "$API_BASE/api/v0/regional_identity" > regional-identity.json
```

Check that the response is successful. It contains `issuerURL`, `audience`,
`contractVersion`, `workloads.platform-api.subject`, and
`additionalTrustStatement`. No EKS access is needed by the customer.

Using customer IAM administrative credentials:

1. Register the returned issuer as an IAM OIDC provider in the customer account
   with client ID `sts.amazonaws.com`, or verify/reuse the existing provider.
2. Merge `additionalTrustStatement` into the installer role's trust policy,
   preserving other statements needed by v1. It permits web identity for the
   exact regional issuer, service-account subject, and STS audience.
3. Confirm the installer role permits `ec2:DescribeVpcs` on `*`. Permissions
   boundaries and organizational policies still apply. Keep hosted-cluster
   operator roles unchanged.

## Discover VPCs

Create `vpc-inquiry.json` with the actual installer-role ARN:

```json
{
  "installerRoleArn": "arn:aws:iam::123456789012:role/example-Installer-Role"
}
```

```bash
awscurl --profile "$CUSTOMER_PROFILE" --service execute-api --region "$SERVICE_REGION" \
  -X POST -H 'Content-Type: application/json' \
  -d "$(cat vpc-inquiry.json)" \
  "$API_BASE/api/v0/aws_inquiries/vpcs"
```

POST follows v1's `aws_inquiries/vpcs` pattern. The body accepts only
`installerRoleArn`, an IAM role in the authenticated caller's account; IAM role
paths are supported. Account/region overrides, user/session ARNs, and unknown
fields are rejected. Region comes from the service deployment.

The response is a complete list of VPCs visible through the role:

```json
{
  "kind": "VPCList",
  "page": 1,
  "size": 1,
  "total": 1,
  "items": [
    {
      "id": "vpc-0123456789abcdef0",
      "name": "demo-vpc",
      "cidrBlock": "10.0.0.0/16",
      "redHatManaged": false
    }
  ]
}
```

`name` comes from the Name tag and is omitted when absent; `cidrBlock` is the
primary IPv4 CIDR when present. Missing/invalid `red-hat-managed` tags yield
false. Empty inventory returns `items: []`, with zero `size` and `total`.
AWS pages are consumed internally under a 15-second operation deadline; there
are no public pagination controls or subnet/security-group enrichment options.
Failure never returns a partial list as complete. Shared resources may be
visible; discovery does not validate their suitability for a cluster.

Each inquiry freshly obtains a 900-second customer session. Credentials are
cached only within the request and are never persisted. No cluster or VPC is
created or modified. The API log `customer VPC discovery completed` includes
the assumed session ARN and result count for correlation with customer AWS
audit logs.

## Errors

Errors use the existing Kubernetes `Status` envelope, with a platform error code
in `message` and numeric HTTP `code`.

| HTTP | Meaning |
| --- | --- |
| 400 | `AWS_INQUIRY_INVALID_INPUT`: invalid body or role ownership/format. |
| 403 | Existing identity or resource authorization denial, before customer AWS calls. |
| 413 | Existing `REQUEST-001` body-size limit. |
| 422 | `CUSTOMER_ACCESS_TRUST_FAILED`, `CUSTOMER_ACCESS_ACCOUNT_MISMATCH`, or `CUSTOMER_ACCESS_PERMISSION_DENIED`. |
| 500 | `CUSTOMER_ACCESS_INTERNAL`: unexpected internal failure. |
| 502 | `CUSTOMER_ACCESS_INVALID_RESPONSE`: malformed AWS data. |
| 503 | `REGIONAL_IDENTITY_UNAVAILABLE` or `CUSTOMER_ACCESS_DEPENDENCY_UNAVAILABLE`. |
| 504 | `CUSTOMER_ACCESS_TIMEOUT`. |

Removing regional IAM trust prevents new exchanges; already-issued sessions
remain valid until expiration. Replacing the regional EKS cluster changes its
issuer and requires customer IAM updates. There is no fallback to ordinary
`AssumeRole` or ambient platform credentials.
