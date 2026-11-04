# DynamoDB authorization storage contract

[ROSAENG-67496](https://redhat.atlassian.net/browse/ROSAENG-67496), under
[ROSAENG-67483](https://redhat.atlassian.net/browse/ROSAENG-67483).
The local Go store and [HTTP management](management.md) are implemented.
AWS provisioning, IAM, replication, and fencing evidence remain deferred.
[Resource authorization](README.md) stays on the startup bundle until ROSAENG-67498.

## Ownership and placement

The authenticated customer account owns custom policies and attachments. The
platform deployment account hosts infrastructure. Replication does not create
customer-owned tables or change tenant ownership. Never accept ownership from
request input.

| Data                 | Placement                          | Writer                               |
| -------------------- | ---------------------------------- | ------------------------------------ |
| Custom policies      | Global policy table                | Assigned writer per customer account |
| Managed policies     | Shared catalog in the policy table | Platform publication only            |
| Global attachments   | Global attachment table            | Assigned writer per customer account |
| Regional attachments | Non-replicated regional table      | Local region                         |

Policies belong to accounts, not creators. Managed catalog entries are immutable
through policy CRUD. Policy creation, inspection, and admission to management
routes grant no resource permissions. Access requires an applicable attachment and Cedar permit.

## Templates and matching

The adapter supports ordinary policies and `principal == ?principal` or
`principal in ?principal` in the principal header, including annotations and
comments. Other slot forms and positions are rejected. `cedar-go v1.8.0` has no
native template linker. Binding uses typed AST entities, not string substitution,
and preserves effects, conditions, and forbids without mutating shared policies.
The startup bundle format is unchanged.

Validation uses the shipped `Seaplane` schema and entity tags. Binding does not
rewrite equality to membership. [Principal matching](README.md#principal-matching)
remains account-scoped, with exact user/session matching, role-session membership,
and rejection of ambiguous role aliases. Forbid wins. Evaluation errors fail closed.

## Identity, revisions, and deletion

- Customer IDs are stable and account-scoped. Catalog IDs are platform-scoped.
  References distinguish custom from managed policies. Attachment IDs are
  store-minted UUIDs.
- Ownership and attachment principal, scope, and region are immutable. Creates
  cannot overwrite. Updates and deletes require the authoritative record's expected
  revision. Fresh opaque revisions protect delete/recreate from stale writes.
- Success confirms a local commit, not replication. HTTP preconditions and
  deletion receipts are defined in [management](management.md#revisions-and-consistency).
- Deleting an attached custom policy is allowed. Missing referenced policies fail
  resolution, including during deletion races or replication reordering. Never
  omit the attachment or cascade deletion across regions. Repair the reference or
  remove the attachment through authorized management.

## Writer enforcement

Global Tables are multi-active. Trusted per-customer assignment, runtime checks,
and AWS permissions must enforce the single-writer rule together. A region-wide
Boolean cannot represent accounts with different writers. Non-writers must not
commit global changes. Writer failure fails global mutations without fallback.
Regional writes remain local with revision checks.

Before reassignment, fence the old writer, account for outstanding requests and
credentials, and verify replacement state before enabling it. Replicated writer
hints cannot authorize takeover. The fencing mechanism remains open. Local guards
are not security evidence. The HTTP contract returns writer guidance, not a proxy.

## Lookup and consistency

Each attachment has an account/ID management record and a complete
account/principal lookup record. A local revision-checked transaction maintains
both. Base-table queries support strong local reads at the cost of duplication.
A GSI would offer only eventual consistency and could omit a newly added forbid.

Resolution reads global plus current-region attachments for the exact caller and
supported parent role. It completes every query page and policy batch, including
unprocessed keys. Incomplete reads, malformed records, and missing policies fail
without a usable partial set. There is no cache. Revisions remain available for
diagnostics. Content is validated on write and binding, not management reads, so
policies invalidated by schema changes remain listable and deletable for repair.

Replication is asynchronous and transaction items replicate independently.
Paginated reads are not atomic snapshots. Even complete strong local reads may
retain an old permit indefinitely during replication failure. There is no bounded
revocation guarantee. Observed replication latency is not a revocation SLA.

## Infrastructure decisions and delivery

Authorization state must survive RC/MC teardown independently of operator and
kube-applier storage. Explicit local policy, global-attachment, and
regional-attachment references are required. A prefix is insufficient. Replica
sharing must be explicit, not recreated per cluster. Intentional removal requires
an approved retention/recovery procedure.

The provisioning recommendation remains Terraform-managed CloudFormation stacks
using `AWS::DynamoDB::GlobalTable.GlobalTableSourceArn`, with each account's stack
owning its replica. Do not also manage those tables as Terraform DynamoDB
resources. The provider `6.71.0` investigation found no native multi-account replica
creation. Provider aliases or resource policies alone do not create replicas.
Validate provider support and stack creation, updates, recovery, and safe removal
in an approved isolated deployment before adoption.

Alternatives remain provider support or a maintained fork, same-account replicas
with different isolation, or custom provisioning/replication with added lifecycle
ownership. Independent tables are not replicas. Estimate replica, backup, and
lookup-duplication costs for the approved topology.

Required approvals cover:

- Operational owner, deployment accounts/regions, and replica membership/sharing.
- Durable Terraform backend, stack ownership, provisioning roles, and order.
- Replication permissions, encryption/key ownership, and network reachability.
- Backup/retention, deletion/disassociation protection, and recovery/teardown.
- Per-customer writer assignment, enforceable permissions, and reassignment.

Use EKS Pod Identity with table/index-scoped permissions, separate runtime and
replica administration, and protect catalog publication. A platform role does not
authenticate customers. Verify or add the role/association for
`platform-api/platform-api-sa`. Do not use static credentials.

Deliver explicit references, ARNs where needed, and regions through Terraform
outputs, bootstrap annotations, ApplicationSet, source configuration, and Helm.
Regenerate `deploy/` from `config/`. The management chart provisions no storage or
IAM. Platform instructions live in `rosa-seaplane/docs/platform-api-authorization.md`.

## Rollout and dangerous cleanup

Activate persistence separately from additive delivery. Replace the temporary
full-access allowlist with admission-only enrollment and explicit grants before
activation. Do not remove it prematurely or fall back to stale bundle grants
after persistent revocation.

Source inventory is not deployed AWS inventory:

- Temporary accounts are ConfigMaps in seaplane-db's `accounts` namespace. Retire
  consumers and records through enrollment and activation work.
- Source searches found no authz admin/group/member tables, target-type attachment
  indexes, or active AVP references. Inventory deployed resources, IAM, and remaining
  consumers before removal. Remove AVP grants only after callers stop using them.
- Preserve startup-bundle/bootstrap delivery until activation. Then retire obsolete
  inputs, including deprecated prefix/region handoff only where unused.
- Leave active operator/kube-applier tables untouched. No customer-data migration
  is part of this work.

## Acceptance evidence

Local `seaplane-api/pkg/authz/store_*.go` tests cover CRUD, managed/custom metadata,
account separation, independent regional scopes, revisions and delete/recreate,
transaction rollback, complete pagination/batches, missing references, failures,
and Cedar template semantics. `make test-api-int` runs race-enabled DynamoDB Local
tests in CI. `AUTHZ_DYNAMODB_ENDPOINT` can reuse a local instance.

Ordinary test tables, synthetic identities, and configuration writer guards prove
local Go/Cedar behavior only. Completion still requires approved topology and
teardown, reviewed infrastructure plans, Pod Identity/configuration delivery,
render/chart/integrated checks, and deployed legacy inventory. AWS evidence must
prove native replication across the intended accounts/regions, independent local
attachments, negative IAM tests, writer fencing/outage behavior, and safe storage
lifecycle. Local evidence alone cannot complete ROSAENG-67496.

## Sources

- [Runtime contract](README.md), [management contract](management.md), and
  [future design obligations](design.md).
- [Platform delivery](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/platform-api-authorization.md)
  and [GitOps configuration](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/design/gitops-cluster-configuration.md).
- [AWS multi-account Global Tables](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/globaltables-MultiAccount.html)
  and [CloudFormation support](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-dynamodb-globaltable.html).
- [Provider 6.71.0 implementation](https://github.com/hashicorp/terraform-provider-aws/blob/v6.71.0/internal/service/dynamodb/table_replica.go),
  [support request](https://github.com/hashicorp/terraform-provider-aws/issues/46877),
  and [proposed implementation](https://github.com/hashicorp/terraform-provider-aws/pull/46393).
- [Cedar Go 1.8.0](https://github.com/cedar-policy/cedar-go/tree/v1.8.0).
