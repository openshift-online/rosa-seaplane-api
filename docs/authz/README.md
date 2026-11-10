# Platform API authorization

Resource requests evaluate Cedar in-process (`cedar-go v1.8.0`). The authorization
service (`authz.Service`) resolves permissions only from DynamoDB. Gateway authentication,
database enrollment, and management authority grant no resource permissions.

See [management](management.md) for configuration and policy administration,
[storage](storage-contract.md) for persistence, and
[platform setup](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/platform-api-authorization.md)
for deployment.

## Startup

See [startup configuration](management.md#startup-configuration). The AWS SDK
region (`AWS_REGION`) is the fixed service region. Read failures fail closed.

Each new request resolves stored grants afresh. Checks and write retries within
a request reuse its Prepared snapshot. Policy and attachment revisions identify
decisions. An in-flight request can finish using its pre-revocation snapshot.

## Enrollment and first grants

Authenticated `/api/v0/accounts` routes are separate from resource admission.
Only the platform account configured by `AWS_ACCOUNT_ID` can enroll or unenroll
accounts. An unset platform account denies enrollment administration.
`POST /api/v0/accounts` accepts `{"accountId":"<id>"}`.
`DELETE /api/v0/accounts/{id}` removes enrollment. Both return 204 idempotently.
The platform account can enroll itself or customers before either is enrolled.
Enrollment lives in seaplane-db; lookup errors deny admission.

Resource and management access both require enrollment. Resource access also
requires stored grants, including for the platform account. Every management
call is authorized by stored grants, so pre-production CI writes each account's
first grants directly into DynamoDB
([seed format](storage-contract.md#pre-production-seed)). See
[management](management.md) for delegation and grant scope.

## Principal matching

| Attachment ARN        | Matches                                                     |
| --------------------- | ----------------------------------------------------------- |
| IAM user, STS session | That exact caller only.                                     |
| IAM role              | Its assumed-role sessions; the role becomes a Cedar parent. |

STS ARNs omit role paths, so roles match on account and final role name. Ambiguous
stored role aliases fail resolution. Only the `aws` partition
is supported.

Policies are evaluated unchanged: `principal == Seaplane::Role::"..."` does not
match a session; `principal in` does. Forbid beats permit. Any evaluation error
fails the request, even when Cedar would allow.

## Operations

| Resource       | Operation             | Required actions                                                         |
| -------------- | --------------------- | ------------------------------------------------------------------------ |
| Cluster        | List                  | ListClusters                                                             |
| Cluster        | Get                   | DescribeCluster                                                          |
| Cluster        | Create                | CreateCluster; Describe on each referenced OIDCConfig and DNSReservation |
| Cluster        | PUT / PATCH           | UpdateCluster + changed-field actions                                    |
| Cluster        | Delete                | DeleteCluster                                                            |
| NodePool       | List                  | ListNodePools                                                            |
| NodePool       | Get                   | DescribeNodePool                                                         |
| NodePool       | Create                | CreateNodePool                                                           |
| NodePool       | PUT / PATCH           | UpdateNodePool + changed-field actions                                   |
| NodePool       | Delete                | DeleteNodePool                                                           |
| OIDCConfig     | List                  | ListOIDCConfigs                                                          |
| OIDCConfig     | Get / Create / Delete | DescribeOIDCConfig / CreateOIDCConfig / DeleteOIDCConfig                 |
| DNSReservation | List                  | ListDNSReservations                                                      |
| DNSReservation | Get / Create / Delete | DescribeDNSReservation / CreateDNSReservation / DeleteDNSReservation     |

Changed-field actions, required in addition to the base update:

| Changed field                                                | Action                |
| ------------------------------------------------------------ | --------------------- |
| Cluster `hostedCluster.release`, `controlPlaneUpgradePolicy` | UpdateClusterVersion  |
| Cluster `hostedCluster.configuration`                        | UpdateClusterConfig   |
| NodePool `nodePool.replicas`, `nodePool.autoScaling`         | ScaleNodePool         |
| NodePool `nodePool.release`                                  | UpdateNodePoolVersion |

- List permission returns complete items without per-item Describe checks.
- Create and update check the base action before any lookup or validation, so a
  caller without it learns nothing about referenced resources or fields.
- Updates compare the stored spec with the merged object to be written. All
  required actions must allow before any write.
- Service-set fields accept exact echoes, reject changes with 422, and survive
  an explicit `null` parent.
- Updates and deletes write the checked `resourceVersion`. On a conflict, updates
  retry (up to 5 attempts), re-reading, re-validating, and re-authorizing each
  attempt; exhausted retries return 409 `*-MGMT-UPDATE-006`. Deletes return 409
  without retrying, except DNSReservation, which keeps upstream's conflict retry
  and re-authorizes each attempt.
- Denials return 403 `AUTHZ-DENIED-001`; authorization failures return 500
  `AUTHZ-FAILED-001`. Responses never include policy details.

## Customer metadata labels

Only `clusters`, `nodepools`, `oidc_configs`, and `dns_reservations` support
`/api/v0/{collection}/{id}/labels`. IDs remain account-scoped names, with
NodePool IDs `<cluster>.<child>`.

Customer labels use `metadata.annotations`; `metadata.labels` are system-only
(`seaplane.io/`). Creates discard client-supplied `metadata.labels` and
system-prefix annotations. Customer labels are distinct from NodePool `spec.labels`
and Cluster `spec.tags`.

- GET requires `ListLabelsForResource` and returns 200 with the complete stored
  JSON map of customer labels (`{}` when empty).
- PATCH accepts a non-empty JSON map, not an envelope. Strings set keys (including
  empty strings); `null` removes keys. Other keys remain unchanged. Success is
  204 without a body, even for mutation-only principals.
- Any string requires `LabelResource`; any null requires `UnlabelResource`.
  Mixed patches require both against the same pre-change snapshot. No Describe
  or base Update action is required. No-ops still require the submitted actions,
  but a wholly unchanged patch does not write.
- Malformed, non-object, null, empty, or wrong-value-type bodies return 400.
  Kubernetes key/value syntax errors and any set/removal under `seaplane.io/`
  return 422. Validation rejects the entire request, never just some keys.
- A resource holds at most 50 customer labels, as AWS tags; a create or PATCH
  past that returns 422. Request bodies are limited to 1 MiB.
- Creates validate customer labels and check only the resource Create action
  against the candidate.
- Storage resolves the caller's account and stored NodePool parent before
  authorization. Valid missing/foreign requests return 404 even with broad grants.
  Labels cannot grant permission to their own patch. An allowed patch may revoke
  the caller's future access; post-change permission is not required.
- PATCH retries conflicts up to five times, rereading and authorizing every
  snapshot and writing its checked resource version. Only submitted keys are
  reapplied. Different-key concurrent writes survive; same-key writes use
  last-successful-write semantics. No expected-version or ETag is accepted.
  UID replacement or deletion after the first read, and exhausted retries,
  return 409. Denials/evaluation failures do not write or retry.

Label administration can transfer access. Restrict grants to trusted principals
or constrain values with `context.requestLabels`.

## Entities

Namespace `Seaplane`. The schema is embedded from
`seaplane-api/pkg/authz/schema/seaplane.cedarschema`.

| Entity         | UID                                                                     | Parents                        |
| -------------- | ----------------------------------------------------------------------- | ------------------------------ |
| Principal      | caller ARN                                                              | matching Roles                 |
| Collection     | `<account>/<region>/{clusters,nodepools,oidc_configs,dns_reservations}` | none                           |
| Cluster        | `<account>/<region>/<cluster-uid>`                                      | Collection                     |
| NodePool       | `<account>/<region>/<cluster-uid>/<pool-uid>`                           | its stored Cluster, Collection |
| OIDCConfig     | `<account>/<region>/<config-uid>`                                       | Collection                     |
| DNSReservation | `<account>/<region>/<reservation-uid>`                                  | Collection                     |

- Resources carry `account` and `region` attributes. `region` is always the
  service region.
- Customer labels (annotations) are entity tags; system labels are not.
  Use `hasTag` before `getTag`. Collections have no tags.
- UIDs are seaplane-db object UIDs. Create checks use the validated candidate name in
  place of the UID, before any write.
- Ownership comes from storage (`account-<accountID>` namespace, account label),
  never from the request. A NodePool's parent is its stored controller reference
  and `seaplane.io/cluster-uid` label, re-checked against the parent's current
  UID; `clusterId` only filters lists.

## Context

Frozen once per request: `region`, `accountId`, `principalArn`, `sourceIp`,
`userAgent`, and `requestTime {unixSeconds, dayOfWeek (Mon=1), hour}` in UTC.
`sourceIp` comes from the gateway. `userAgent` is caller-controlled, not identity
proof. IAM-internal MFA conditions and session tags are unavailable.

`requestLabels` holds, as tags, the customer labels the request writes: the
annotations on a create, or the keys a label PATCH sets (not removals). Like
`aws:RequestTag`, it limits what a label grant may write:

```cedar
forbid(principal, action, resource)
when { context.requestLabels.hasTag("Team") && context.requestLabels.getTag("Team") != "a" };
```

## Groups and examples

| Group               | Actions                                                                   |
| ------------------- | ------------------------------------------------------------------------- |
| ReadOnly            | ListLabelsForResource plus customer List and Describe actions             |
| ClusterAdmin        | Create, Update, UpdateClusterConfig, UpdateClusterVersion, Delete Cluster |
| NodePoolAdmin       | Create, Update, ScaleNodePool, UpdateNodePoolVersion, Delete NodePool     |
| OIDCConfigAdmin     | Create, Delete OIDCConfig                                                 |
| DNSReservationAdmin | Create, Delete DNSReservation                                             |
| LabelAdmin          | LabelResource, UnlabelResource, without implicit label read permission    |
| AllActions          | The six customer groups, including LabelAdmin.                            |

```cedar
permit(principal, action in Seaplane::Action::"ReadOnly", resource);
```

```cedar
permit(principal, action in Seaplane::Action::"AllActions", resource);
```

Resource groups exclude management actions. See [management delegation](management.md#startup-configuration).

```cedar
forbid(principal, action == Seaplane::Action::"DeleteCluster", resource)
when { resource.hasTag("Environment") && resource.getTag("Environment") == "production" };
```

Team-scoped admin (also needs a List grant to discover clusters):

```cedar
permit(principal, action in Seaplane::Action::"ClusterAdmin", resource)
when { resource.hasTag("Team") && resource.getTag("Team") == "platform-engineering" };
```

Scaling needs the base update too:

```cedar
permit(principal, action in [Seaplane::Action::"UpdateNodePool", Seaplane::Action::"ScaleNodePool", Seaplane::Action::"DescribeNodePool", Seaplane::Action::"ListNodePools"], resource);
```

```cedar
permit(principal, action in Seaplane::Action::"NodePoolAdmin", resource)
when { resource in Seaplane::Cluster::"123456789012/us-east-1/550e8400-e29b-41d4-a716-446655440000" };
```

```cedar
permit(principal, action, resource)
when { context.requestTime.dayOfWeek >= 1 && context.requestTime.dayOfWeek <= 5 && context.requestTime.hour >= 9 && context.requestTime.hour < 17 };
```

```cedar
forbid(principal, action, resource)
unless { ["us-east-1", "us-west-2"].contains(context.region) };
```

## Logs and metrics

Failures log `request authorization failed` with operation, failing action, caller,
stage, cause, Cedar diagnostics, and policy/attachment IDs and revisions as
`provenance`. Denials log `request authorization denied` at INFO with operation,
action, caller, and provenance (empty with no permit; forbid IDs when matched).
Logs never include policy content.

| Metric                   | Labels                 |
| ------------------------ | ---------------------- |
| `authz_requests_total`   | `operation`, `outcome` |
| `authz_duration_seconds` | `operation`, `outcome` |
| `authz_failures_total`   | `operation`, `stage`   |

- One final outcome and duration observation per request reaching authorization,
  not per retry or checked action. A later retry denial/error replaces earlier
  allowed checks. Conflict exhaustion after allowed checks remains an authorization
  allow, not an HTTP success.
- Label GET uses `ListLabelsForResource`; PATCH uses `LabelResource` if any set
  was submitted, otherwise `UnlabelResource`. Mixed patches emit one outcome.
- `outcome`: `allow`, `deny`, `error`. `stage`: `resolution`, `binding`,
  `entity_validation`, `evaluation`, `resource_loading`.
- Unauthenticated, unenrolled, rate-limited, and public requests record nothing.

## Verify locally

| Command               | Covers                                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `make test-api`       | Unit tests, including `seaplane-api/pkg/authz`.                                                                                |
| `make test-api-int`   | Handler and server integration tests.                                                                                          |
| `make test-e2e-authz` | Binary HTTP tests with local PostgreSQL and DynamoDB. See [test setup](../../test/e2e-api/README.md#local-authorization-test). |

## Limits

- The API trusts gateway identity headers. The listener must be reachable only
  through the gateway.
- The schema validator uses `cedar-go` experimental packages. Upgrades must keep
  all authz tests passing without weakening validation.
