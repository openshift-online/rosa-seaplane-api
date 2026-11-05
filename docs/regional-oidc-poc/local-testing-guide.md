# Test regional OIDC against an ephemeral environment

Assumes the ephemeral environment runs the updated Platform API and the
customer account/caller already has API access. Requires AWS CLI v2, ROSA,
`awscurl`, `jq`, and `oc` for the existing ROSA setup command.

## 1. Set variables

If using separate caller and administrator profiles, both must belong to the same account.
Run the remaining commands in the same shell.

```bash
API_URL="https://YOUR_API_ID.execute-api.us-east-1.amazonaws.com/prod"
SERVICE_REGION="us-east-1"
PROFILE="your-aws-profile"
ADMIN_PROFILE="$PROFILE"
ROLE_PREFIX="RegionalOIDCPoC"
```

Use an unused role prefix for a new test.

## 2. Create the installer role

The existing command requires a normal OCM login. Do not pass
`--seaplane-url`; the AWS profile determines where the roles are created.
This creates the HCP installer, support, and worker roles.

```bash
rosa login

rosa create account-roles \
  --profile "$ADMIN_PROFILE" \
  --region "$SERVICE_REGION" \
  --hosted-cp --mode auto --prefix "$ROLE_PREFIX"

INSTALLER_ROLE_NAME="${ROLE_PREFIX}-HCP-ROSA-Installer-Role"

INSTALLER_ROLE_ARN=$(aws --profile "$ADMIN_PROFILE" iam get-role \
  --role-name "$INSTALLER_ROLE_NAME" \
  --query 'Role.Arn' --output text)
```

If reusing an installer role, skip creation and use its name when obtaining
the ARN. Standard HCP installer permissions already include `ec2:DescribeVpcs`.

## 3. Fetch regional identity

This returns the issuer and required trust statement; it does not change IAM.

```bash
awscurl --profile "$PROFILE" \
  --service execute-api --region "$SERVICE_REGION" \
  "$API_URL/api/v0/regional_identity" > regional-identity.json

ISSUER_URL=$(jq -er '.issuerURL' regional-identity.json)
AUDIENCE=$(jq -er '.audience' regional-identity.json)
PROVIDER_ARN=$(jq -er '.additionalTrustStatement.Principal.Federated' regional-identity.json)
```

## 4. Register the issuer in customer IAM

```bash
aws --profile "$ADMIN_PROFILE" iam create-open-id-connect-provider \
  --url "$ISSUER_URL" \
  --client-id-list "$AUDIENCE"
```

If it returns `EntityAlreadyExists`, reuse the provider and ensure the STS
audience is present with this idempotent command:

```bash
aws --profile "$ADMIN_PROFILE" iam add-client-id-to-open-id-connect-provider \
  --open-id-connect-provider-arn "$PROVIDER_ARN" \
  --client-id "$AUDIENCE"
```

## 5. Add regional trust to the installer role

Save the current trust, append the regional statement without duplicating it,
and apply the combined policy. Existing v1 trust and role permissions remain.

```bash
aws --profile "$ADMIN_PROFILE" iam get-role \
  --role-name "$INSTALLER_ROLE_NAME" \
  --query 'Role.AssumeRolePolicyDocument' \
  --output json > installer-trust-original.json

jq --slurpfile identity regional-identity.json '
  $identity[0].additionalTrustStatement as $new
  | .Statement |= (
      (if type == "array" then . else [.] end)
      | if any(.[]; . == $new) then . else . + [$new] end
    )
' installer-trust-original.json > installer-trust-regional.json

aws --profile "$ADMIN_PROFILE" iam update-assume-role-policy \
  --role-name "$INSTALLER_ROLE_NAME" \
  --policy-document file://installer-trust-regional.json
```

## 6. Test discovery

```bash
jq -n --arg role "$INSTALLER_ROLE_ARN" \
  '{installerRoleArn: $role}' > vpc-inquiry.json

awscurl --profile "$PROFILE" \
  --service execute-api --region "$SERVICE_REGION" \
  -X POST -H 'Content-Type: application/json' \
  -d "$(cat vpc-inquiry.json)" \
  "$API_URL/api/v0/aws_inquiries/vpcs" | jq .
```

A successful `VPCList` response demonstrates OIDC role assumption and EC2 access in the API's region.
