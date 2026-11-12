# DynamoDB authorization storage contract

The Store backs [resource authorization](README.md) and [management](management.md).
[Platform setup](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/platform-api-authorization.md)
owns table provisioning, retention, IAM, and configuration delivery.

## Ownership and placement

The platform hosts the tables. The authenticated customer account owns its custom
policies and attachments; ownership never comes from request input.

| Data                                   | Writer                                                                                      |
| -------------------------------------- | ------------------------------------------------------------------------------------------- |
| Custom policies and global attachments | Local API, subject to management authorization; [pre-production seed](#pre-production-seed) |
| Managed policy catalog                 | Platform publication only                                                                   |
| Regional attachments                   | Local region                                                                                |

Customer policy IDs are account-scoped; managed catalog IDs are platform-scoped.
References distinguish the two. The API generates UUID attachment IDs; any store ID is valid.
Ownership and attachment principal, scope, and region are immutable.

## Policy binding

Policies contain one Cedar statement validated against the shipped `Seaplane`
schema. Templates support `principal == ?principal` or `principal in ?principal`
in the principal header, including annotations and comments. Other slot forms
are rejected. Binding uses typed AST entities, not string substitution, and
preserves effects and conditions. It does not rewrite equality to membership.
See [principal matching](README.md#principal-matching).

## Reads and writes

Each attachment has an account/ID management record and a complete
account/principal lookup record. A revision-checked transaction maintains both.
Base-table queries support strong local reads without an eventually consistent GSI.

Resolution reads global and current-region attachments for the caller and its
supported parent role. It completes all query pages and policy batches, including
unprocessed keys. Incomplete reads, malformed records, and missing policies fail
without returning a usable partial set. Reads do not form an atomic snapshot.
Request-level reuse is described in [runtime authorization](README.md#startup).

Creates cannot overwrite. Updates and deletes require the authoritative record's
revision. Fresh revisions protect deletion and recreation from stale writes.
See [HTTP preconditions and deletion receipts](management.md#revisions-and-consistency).

Deleting an attached custom policy is allowed, but subsequent resolution fails.
Repair the reference or remove the attachment through authorized management;
resolution never silently drops a dangling attachment. Policies are validated on
write and binding, not management reads, so policies invalidated by schema changes
remain listable and deletable for repair.

## Pre-production seed

Management requires stored grants, so pre-production CI writes each account's
first grants directly (rosa-seaplane `ci/e2e-tests.sh`). `TestCISeed` pins this
format. Each grant is one policy and one global attachment for an IAM role,
sharing a fixed ID so reruns fail the condition and change nothing. Every
attribute is a string; each `Put` uses `attribute_not_exists(pk)`:

| Table    | `pk`                                    | `sk`              | `revision` | `record`                                                                                      |
| -------- | --------------------------------------- | ----------------- | ---------- | --------------------------------------------------------------------------------------------- |
| policies | `ACCOUNT#<acct>`                        | `POLICY#<id>`     | `R1`       | `{"ID":"<id>","OwnerAccountID":"<acct>","Content":"<cedar>","Revision":"R1","Kind":"custom"}` |
| global   | `ACCOUNT#<acct>`                        | `ATTACHMENT#<id>` | `R2`       | `A`                                                                                           |
| global   | `ACCOUNT#<acct>#ROLE#aws/<acct>/<role>` | `ATTACHMENT#<id>` | `R2`       | `A`                                                                                           |

`R1` and `R2` are random UUIDs. `A` is
`{"Policy":{"ID":"<id>","Kind":"custom"},"PrincipalARN":"arn:aws:iam::<acct>:role/<role>","Scope":"global","ID":"<id>","OwnerAccountID":"<acct>","Region":"","Revision":"R2"}`.
Invalid Cedar or a malformed row fails closed for the callers it applies to.

## Regional storage

Each API reads and writes its configured regional tables. Global attachments are
not region-filtered; regional attachments apply only in their region. Neither
scope replicates records to another deployment.

Multi-account Global Tables and their consistency semantics are outside this
regional delivery.

Local Store tests cover CRUD, account isolation, revisions, transactions, complete
reads, failure handling, and template binding. `make test-api-int` runs DynamoDB
Local tests; `AUTHZ_DYNAMODB_ENDPOINT` can reuse a local instance. These tests do
not prove deployed IAM, recovery, or retained-state survival.
