# DynamoDB authorization storage design

**Last Updated Date**: 2026-11-02

**Tracking**: [ROSAENG-67496](https://redhat.atlassian.net/browse/ROSAENG-67496),
under [ROSAENG-67483](https://redhat.atlassian.net/browse/ROSAENG-67483).
**Status**: Design for review. The local store is implemented; AWS provisioning
has not started. Agreed behavior and proposed implementation choices are
distinguished below.

## Summary and scope

Use native DynamoDB Global Tables for policies and global attachments, ordinary
regional tables for regional attachments, and one authoritative global writer per
customer account. Read and evaluate locally. Authorization storage survives
Regional Cluster (RC) and Management Cluster (MC) teardown.

This ticket delivers real Go store CRUD, lookup paths, provisioning, IAM,
configuration delivery and local/AWS evidence. Live authorization stays on its
startup bundle until deliberate activation. Companion tasks cover management
endpoints (67497), resolver activation (67498), metrics (67499), final validation/docs
(67500), and persistent enrollment (75149). No SSO, identity linking, CLI workflows,
caching or customer-data migration.

## Agreed behavior

### Ownership and placement

A **customer account** owns custom policies and attachments; a **deployment
account** hosts platform infrastructure. Replication across deployment accounts
does not create tables in every customer's account or change tenant ownership.
Derive customer ownership from authenticated identity, never request input.

| Data                 | Placement                                          | Who writes                                                              |
| -------------------- | -------------------------------------------------- | ----------------------------------------------------------------------- |
| Custom policies      | Global policy table                                | Customer account's assigned writer, subject to management authorization |
| Managed policies     | Shared platform catalog in the global policy store | Platform publication only; policy CRUD updates/deletes are rejected     |
| Global attachments   | Global attachment table                            | Customer account's assigned writer                                      |
| Regional attachments | Non-replicated regional table                      | That region                                                             |

Custom policies belong to the account, not their creator. Customers may attach
managed policies but cannot change them. Platform publication introduces catalog
entries; it does not permit mutation of existing entries through policy CRUD.

An attachment binds a policy to an IAM principal and scope. Creating/viewing a
policy or enrolling an account grants no resource access in the target design.
Management authority is separate; resource access needs an applicable attachment
and an allowing Cedar decision.

### Templates and matching

The approved adapter supports `?principal` as the only template variable.
`cedar-go v1.8.0` has no native template parser/linker: use structured binding to
typed entities, not arbitrary string substitution. Validate against the shipped
`Seaplane` schema; preserve effects, scopes, conditions and forbids without
mutating shared policies. Define and test accepted syntax and rejection behavior
before exposing an API contract. Leave the startup bundle format unchanged.

Preserve current matching: users/sessions match exactly; role attachments are
considered for their assumed-role sessions. STS loses role paths, so retain
account/role-name matching and reject ambiguous aliases. Attachment matching does
not override Cedar: equality to a role is not membership; never rewrite `==` to
`in`. Forbid beats permit; evaluation errors fail the request.

Retain the shipped `Seaplane` namespace and entity tags, not the older source
design's `ROSA::` examples or label-map notation.

### Identity, concurrency and deletion

- Customer policy/attachment IDs are stable and account-scoped; catalog IDs are
  platform-scoped. Ownership is immutable, as are attachment principal, scope and
  regional identity. References distinguish custom policies from catalog entries.
- Creates cannot overwrite existing records. Updates/deletes require the expected
  revision at the authoritative store. Fresh opaque revisions prevent stale writes,
  including after delete/recreate. Success confirms a local commit, not replication.
- Deleting an attached **custom** policy is allowed. A missing referenced policy
  fails resolution, including during deletion races or replication reordering.
  Never silently omit its attachment or cascade deletion across regions. Repair
  the reference or remove the attachment through authorized management.

### Writer enforcement

Global Tables are multi-active; they do not enforce our single-writer rule.
Trusted per-customer assignment, runtime checks and AWS permissions must agree.
Non-writers cannot commit global changes. Writer failure fails global mutations,
without automatic fallback; regional writes remain local with revision checks.
A region-wide writer Boolean is insufficient when accounts have different writers.

Before reassignment, fence the old writer, account for outstanding requests and
credentials, verify replacement state, then enable the replacement. Replicated
writer hints cannot authorize takeover. The exact fencing mechanism is still open;
a local guard is not security evidence. Management request routing remains a later
integration decision.

## Proposed lookup layout and consistency

The local implementation uses an account/ID management record plus a complete
account/principal lookup record per attachment, maintained together in a local
revision-checked transaction. This replaces target-type lookup and supports
principal queries on the base table, preserving role/session matching. Production
deployment validation remains deferred.

**Tradeoff:** base-table queries allow strong local reads but require duplicated
records. A GSI (Global Secondary Index) maintains the index automatically but only
offers eventual consistency and can omit a recently added forbid.

The target resolver reads local global attachments plus only its own region's
attachments, for the exact caller and supported parent role. Resolve every policy
and every query page/batch, including unprocessed batch keys. Incomplete reads,
malformed records or missing policies must fail without a usable partial set.
Read each request without caching; retain revisions for diagnostics. Policy
content is validated on write and at binding, not on management reads, so a
policy invalidated by a schema change stays listable and deletable.

**Local consistency is not global freshness.** Replication is asynchronous and
replicates transaction items independently. Paginated reads are not an atomic
snapshot. Even complete strong local reads can retain an old permit indefinitely
during replication failure: **there is no bounded revocation guarantee**.

## Infrastructure and open decisions

The platform owns dedicated durable authorization state, separate from RC/MC,
operator and kube-applier lifecycles. Clusters consume explicit local policy,
global-attachment and regional-attachment references; a prefix is insufficient.
Replica sharing must be explicit, not recreated per cluster. Intentional storage
removal needs a separate approved retention/recovery procedure.

### Provisioning recommendation, not yet approved

Terraform AWS provider `6.71.0` cannot create native multi-account replicas;
support is proposed in an unmerged pull request. AWS's SDK can, but provider
aliases or resource policies alone do not establish them.

Recommend **Terraform-managed CloudFormation stacks** for these tables.
CloudFormation documents multi-account support through
`AWS::DynamoDB::GlobalTable.GlobalTableSourceArn`; each account's stack owns its
local replica. Do not also manage those tables directly as Terraform DynamoDB
resources. Validate creation, updates, recovery and safe removal in an approved
isolated deployment before adoption.

Alternatives: wait for/contribute provider support (upstream delay or fork
maintenance); use same-account replicas (changes isolation); write custom
provisioning/replication (adds lifecycle/recovery ownership). Independent tables
are not replicas. Keep the existing Cedar runtime with the approved adapter.

### Required approvals before provisioning

- Operational owner, deployment accounts/regions, replica sharing/membership.
- Durable Terraform backend, stack ownership, provisioning roles/order.
- Replication permissions, encryption/key ownership and network reachability.
- Backup/retention, deletion/disassociation protection and recovery/teardown.
- Per-customer writer assignment, enforceable permissions and reassignment.

Replica storage, replication and backups add cost; estimate it for that topology.
Lookup duplication, template adaptation and CloudFormation also add maintenance.

### Credentials and delivery

Keep EKS Pod Identity, with table/index-scoped access. Separate runtime access from
replica administration and protect catalog writes. A shared platform role does
not authenticate the customer.

Jira assumes an existing Platform API role/association; none was found in this
checkout's regional Terraform. Add it if absent for
`platform-api/platform-api-sa`, otherwise update it. No static credentials.

Deliver explicit table references (ARNs where needed) and regions through
**Terraform outputs → bootstrap annotations → ApplicationSet → source configuration
→ Helm**. Follow existing GitOps wiring; regenerate `deploy/` from `config/`.

## Rollout and legacy cleanup

Merge additive task slices into `main`; activate persistent authorization
separately. Today's temporary allowlist grants full access to an account's own
resources. Replace it with admission-only enrollment and explicit grants before
activation; neither remove it prematurely nor fall back to stale bundle grants
after persistent revocation.

Initial source inventory, **not an AWS inventory**:

- Temporary account records are ConfigMaps in seaplane-db's `accounts` namespace.
  Retire callers/records through enrollment and activation tasks.
- No authorization admin/group/member tables, target-type attachment indexes or
  active AVP references were found in current source searches. Inventory deployed
  resources/IAM and remaining consumers before removal; remove AVP grants only
  after callers stop using them.
- Preserve startup-bundle/bootstrap delivery until activation, then retire obsolete
  authorization inputs, including deprecated prefix/region handoff where unused.
- Leave active operator/kube-applier tables untouched. No customer-data migration.

## Completion evidence

The local storage slice is implemented and verified. AWS acceptance remains
deferred, so this is not Jira completion. Required evidence:

- **Local:** real Go CRUD; managed/custom metadata; account separation; global plus
  independent regional attachment behavior; revisions, delete/recreate and
  transaction rollback; complete pagination/batches; missing references and store
  failures; template binding that preserves Cedar semantics. Create the required
  tables and lookup structures in DynamoDB Local.
- **Delivery:** approved ownership/topology/state/teardown, reviewed infrastructure
  plans, Pod Identity and configuration wiring, render/chart/API/platform checks,
  and deployed legacy inventory with dependency-ordered removal.
- **AWS:** native replication visibility across the designed account/region
  boundaries, independent regional attachments, negative IAM tests, writer fencing
  and outage behavior, and safe storage lifecycle. Observed replication latency
  is not a revocation SLA.

The local implementation lives in API `seaplane-api/pkg/authz/store_*.go`.
API `make test-api-int`, also run in CI, starts a disposable DynamoDB Local
container for race-enabled store tests; set `AUTHZ_DYNAMODB_ENDPOINT` to reuse a
running instance. No production platform code is needed for this local slice.

The adapter accepts `principal == ?principal` and `principal in ?principal` in the
principal header, including annotations/comments. Other slot positions/forms are
rejected. Ordinary policies remain supported; the startup bundle is unchanged.
Attachment IDs are store-minted UUIDs. Tests use ordinary tables, synthetic
identities and a configuration-based writer guard: they prove local Go/Cedar
behavior, not AWS replication, tenant authentication or IAM enforcement.

**Deferred:** AWS provisioning/configuration delivery, Pod Identity/IAM,
replication, writer fencing and deployed inventory evidence.
The Jira ticket cannot be marked complete on local evidence alone.

## Sources

- Jira and epic linked above; current behavior: [Platform API authorization](authz.md).
- Original intent: workspace `docs/ROSA -Seaplane-Authorization-Service.md` and
  [target design](authz-design.md); their broader linking/CLI scope is excluded.
- [Platform authorization delivery](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/platform-api-authorization.md) and
  [GitOps configuration](https://github.com/openshift-online/rosa-seaplane/blob/main/docs/design/gitops-cluster-configuration.md).
- [AWS multi-account Global Tables](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/globaltables-MultiAccount.html)
  and [CloudFormation support](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-dynamodb-globaltable.html).
- [Pinned provider implementation](https://github.com/hashicorp/terraform-provider-aws/blob/v6.71.0/internal/service/dynamodb/table_replica.go),
  [support request](https://github.com/hashicorp/terraform-provider-aws/issues/46877)
  and [proposed implementation](https://github.com/hashicorp/terraform-provider-aws/pull/46393).
- [Cedar Go limitations](https://github.com/cedar-policy/cedar-go/tree/v1.8.0).
