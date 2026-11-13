# Authorization management HTTP contract

Management uses gateway-authenticated AWS identity and account-local DynamoDB.
Management routes require [enrollment](README.md#enrollment-and-first-grants), like
resource routes. Administrative authority grants no resource access.

## Startup configuration

| Env var                                                            | Meaning                                        |
| ------------------------------------------------------------------ | ---------------------------------------------- |
| `AUTHZ_POLICY_TABLE`, `AUTHZ_GLOBAL_TABLE`, `AUTHZ_REGIONAL_TABLE` | Required. Three distinct DynamoDB table names. |

Invalid configuration fails before database connections or listeners. The SDK
uses existing AWS configuration, including `AWS_ENDPOINT_URL_DYNAMODB` for local
testing.

Every caller needs Cedar grants; there is no bypass. The first grants are
[seeded directly](storage-contract.md#pre-production-seed), and a broken grant
is repaired the same way.
[Role/session matching](README.md#principal-matching) applies. `PolicyAdmin` includes all management
actions. `RegionalAttachmentAdmin` includes only regional attachment actions.
Global management actions require global attachments; regional actions also accept
current-region attachments. Global attachment scope is not region-filtered.
Resource `AllActions` and `ReadOnly` exclude management.

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

Response fields are defined in OpenAPI. Managed policy PUT/DELETE returns 403.

## Revisions and consistency

Updates/deletes require one strong double-quoted nonzero UUID, for example
`If-Match: "550e8400-e29b-41d4-a716-446655440000"`. Missing, weak, list, and wildcard
preconditions return 400. Stale revisions return 409.

Creates/updates return the committed revision. Deletes return
`{id,deletedRevision,committedRegion}`. Success confirms a local commit.

## Errors

Errors use Kubernetes `Status` and platform error codes.

| Status | Meaning                                                                            |
| ------ | ---------------------------------------------------------------------------------- |
| 400    | Malformed JSON, query, or revision precondition                                    |
| 403    | Invalid identity, unregistered account, denied action, or immutable managed policy |
| 404    | Missing record                                                                     |
| 409    | Revision conflict                                                                  |
| 422    | Invalid policy or attachment                                                       |
| 500    | Authorization evaluation or resolution failure                                     |
| 503    | Backend I/O failure                                                                |
