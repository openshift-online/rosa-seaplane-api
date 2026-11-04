# Authorization management HTTP contract

Management uses gateway-authenticated AWS identity and account-local DynamoDB.
It requires neither Red Hat linking nor legacy `/accounts` enrollment. The
allowlist grants no management authority. Bootstrap grants no resource access.
[Resource authorization](README.md) still uses the startup bundle.

## Startup configuration

`--authz-management-config-file` overrides `AUTHZ_MANAGEMENT_CONFIG_FILE`, even
when explicitly empty. Empty disables routes. Invalid supplied config fails before
database connections or listeners. The SDK uses existing AWS configuration,
including `AWS_ENDPOINT_URL_DYNAMODB` for local testing.

```yaml
formatVersion: 1
policyTable: authz-policies
globalTable: authz-global-attachments
regionalTable: authz-regional-attachments
accounts:
  "111111111111":
    writerRegion: us-east-1
    administratorARNs:
      - arn:aws:iam::111111111111:role/customer-policy-admin
```

Config provisions no tables. Platform setup lives in
`rosa-seaplane/docs/platform-api-authorization.md`. Administrators manage only
their account and bypass broken grants for repair. [Role/session matching](README.md#principal-matching)
applies. Other callers need Cedar delegation. `PolicyAdmin` includes all management
actions. `RegionalAttachmentAdmin` includes only regional attachment actions.
Global grants require global attachments. Regional grants accept global or
current-region attachments. Resource `AllActions` and `ReadOnly` exclude management.

## Routes and payloads

Paths use `/api/v0/authz`. See the [public OpenAPI specification](../../api/v1alpha1/public/openapi.yaml).

| Method | Path                | Input                                                 | Success                              |
| ------ | ------------------- | ----------------------------------------------------- | ------------------------------------ |
| POST   | `/policies`         | `{id,content}`                                        | 201 policy                           |
| GET    | `/policies`         | None                                                  | 200 `PolicyList`, custom and managed |
| GET    | `/policies/{id}`    | `?type=custom` or `managed`, default custom           | 200 policy                           |
| PUT    | `/policies/{id}`    | `{content}` and `If-Match`                            | 200 policy                           |
| DELETE | `/policies/{id}`    | `If-Match`                                            | 200 deletion receipt                 |
| POST   | `/attachments`      | `{policy:{id,type},principalArn,scope}`               | 201 attachment                       |
| GET    | `/attachments`      | Optional `?scope=global` or `regional`                | 200 `AttachmentList`                 |
| DELETE | `/attachments/{id}` | Required `?scope=global` or `regional` and `If-Match` | 200 deletion receipt                 |

Bodies reject unknown fields and trailing values. Ownership, revision, and region
are server-assigned. Attachment scope defaults to global. Regional identity comes
from the service region. An unfiltered list requires both list permissions and
returns global plus local attachments. Regional-only delegates must request
`?scope=regional`. No scope is silently omitted.

Policies return `id`, `type`, `ownerAccountId`, `content`, and `revision`.
Attachments return `id`, `ownerAccountId`, `policy`, `principalArn`, `scope`,
`revision`, and regional-only `region`. Lists return `kind`, `total`, and `items`.
Managed policy PUT/DELETE returns 403. Item policy routes accept the `type` query.

## Revisions and consistency

Updates/deletes require one strong double-quoted nonzero UUID, for example
`If-Match: "550e8400-e29b-41d4-a716-446655440000"`. Missing, weak, list, and wildcard
preconditions return 400. Stale revisions return 409.

Creates/updates return the committed revision. Deletes return
`{id,deletedRevision,committedRegion}`. Success confirms a local commit, not
replication. Other regions may authorize old permissions even after revocation.

Global mutations require the account's assigned writer. Wrong-writer 409 includes
`X-Authz-Writer-Region` and `details.causes` field `writerRegion`. There is no proxy.
Retry the original authenticated request there. The writer reauthorizes locally.
Writer failure fails global writes. Never fail over. Regional writes stay local.
See [storage guarantees and fencing requirements](storage-contract.md).

## Errors

Errors use Kubernetes `Status` and platform error codes.

| Status | Meaning                                                                            |
| ------ | ---------------------------------------------------------------------------------- |
| 400    | Malformed JSON, query, or revision precondition                                    |
| 403    | Invalid identity, unregistered account, denied action, or immutable managed policy |
| 404    | Missing record or removed `/api/v0/authz/check`                                    |
| 409    | Revision conflict or wrong writer                                                  |
| 422    | Invalid policy or attachment                                                       |
| 500    | Authorization evaluation or resolution failure                                     |
| 503    | Backend I/O failure                                                                |
