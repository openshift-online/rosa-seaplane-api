# Platform API authorization

Resource requests evaluate Cedar in-process (`cedar-go v1.8.0`) against a startup
bundle. Enrollment and gateway authentication grant nothing. The temporary
allowlist is the exception, granting full access to its accounts' own resources.

[Management](management.md) uses DynamoDB and separate bootstrap
administrators, without implicit resource grants. Resource resolution stays on
the bundle until ROSAENG-67498. See the [storage contract](storage-contract.md)
and [future design](design.md). Platform setup lives in
`rosa-seaplane/docs/platform-api-authorization.md`. `AccessEntry` is unimplemented.
The removed `/api/v0/authz/check` returns a structured 404.

## Startup

| Input                                       | Behavior                                              |
| ------------------------------------------- | ----------------------------------------------------- |
| `--authz-config-file` / `AUTHZ_CONFIG_FILE` | Required bundle path. The flag wins, even when empty. |
| AWS SDK region (`AWS_REGION`)               | Fixed service region. Required.                       |
| `--dynamodb-region`                         | Deprecated region fallback.                           |
| `--allowed-accounts`, `--dynamodb-prefix`   | Deprecated, ignored.                                  |

Startup validates the whole bundle before database connections or listeners.
Failure logs `authorization startup failed` and exits nonzero. Changes require a
rollout. Old replicas keep their immutable permissions until replaced.

## Bundle

One YAML document, version 1:

```yaml
formatVersion: 1
registeredAccounts: ["123456789012"]
policies:
  - id: read
    ownerAccountID: "123456789012"
    content: permit(principal, action in Seaplane::Action::"ReadOnly", resource);
attachments:
  - id: readers
    policyID: read
    principalARN: arn:aws:iam::123456789012:role/platform/readers
    scope: regional # or global, with no region
    region: us-east-1
```

- `registeredAccounts`: accounts admitted to the API. Admission only.
- The temporary allowlist uses `POST /api/v0/accounts` with `{"accountId":"<id>"}`
  and `DELETE /api/v0/accounts/{id}`. Both return 204 idempotently without rollout.
  Only the platform account (`AWS_ACCOUNT_ID`) may call them. Unset denies all.
  Allowlisted accounts get full access to their own resources, diagnostic ID
  `allowlist/<account>`. Bundle accounts never use the allowlist. Lookup errors
  deny. Persistent admission and grants must replace this bypass.
- Each policy holds one Cedar statement, strictly validated against the schema.
- An attachment's principal account must equal its policy's `ownerAccountID`.
- `regional` attachments apply only in their region; `global` applies everywhere.
- Startup fails on any invalid record, including unattached policies and
  off-region attachments, and on unknown fields, duplicate keys or IDs, YAML
  aliases, dangling references, and ambiguous role aliases.

## Principal matching

| Attachment ARN        | Matches                                                     |
| --------------------- | ----------------------------------------------------------- |
| IAM user, STS session | That exact caller only.                                     |
| IAM role              | Its assumed-role sessions; the role becomes a Cedar parent. |

STS ARNs omit role paths, so roles match on account and final role name. Two
configured roles differing only by path fail startup. Only the `aws` partition
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

- Lists return every item in full and count it in `total`. List permission
  alone is enough; Describe is not checked per item.
- Create and update check the base action before any lookup or validation, so a
  caller without it learns nothing about referenced resources or fields.
- Updates compare the stored spec with the merged object to be written. All
  required actions must allow before any write.
- Service-set fields never change on update. An exact echo is accepted;
  changing or clearing one returns 422. An explicit `null` parent keeps its
  service-set children.
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

Customer labels are `metadata.annotations`, in the public API and in storage;
`metadata.labels` hold only system keys (`seaplane.io/`), and client-supplied
labels are dropped on create. The routes keep the `labels` name.

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
- Creates discard caller-supplied labels and system-prefix annotations, validate
  customer syntax, install trusted system labels, and check only the resource
  Create action against the candidate. These are metadata annotations, not
  NodePool `spec.labels` or Cluster `spec.tags`.
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

Label administration controls authorization inputs. Grant it only to principals
trusted to transfer access through customer labels, or limit the values they may
write with `context.requestLabels`; reserving system keys does not prevent that
transfer.

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
`sourceIp` comes from the gateway. `userAgent` is caller-controlled; do not trust it.

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

Tests pin all nine Cedar examples in this document.

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

Failures log `request authorization failed` with `operation` (the request),
`action` (the failing check), caller, `stage`, `cause`, `provenance` (policy,
attachment, `BundleHash` = SHA-256 of the bundle file), and Cedar
`diagnostics`. Denials log `request authorization denied` at INFO with
`operation`, `action`, caller, and `provenance` (empty when no policy permits;
forbid IDs when one matches). Logs never include policy content.

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
- `outcome`: `allow`, `deny`, `error`. `stage`: `resolution`, `parsing`,
  `binding`, `entity_validation`, `evaluation`, `resource_loading`.
- Unauthenticated, unenrolled, rate-limited, and public requests record nothing.

## Verify locally

| Command               | Covers                                                                                             |
| --------------------- | -------------------------------------------------------------------------------------------------- |
| `make test-api`       | Unit tests, including `seaplane-api/pkg/authz`.                                                    |
| `make test-api-int`   | Handler and server integration tests.                                                              |
| `make test-e2e-authz` | Real binary over HTTP with disposable PostgreSQL (Podman). Results in `test-results/authz-http-*`. |

Local results do not prove deployed metrics delivery. Verify ServiceMonitor,
Prometheus, and Thanos at matching API, operator, and test revisions.

## Limits

- The API trusts gateway identity headers. The listener must be reachable only
  through the gateway.
- The schema validator uses `cedar-go` experimental packages. Upgrades must keep
  all authz tests passing without weakening validation.
