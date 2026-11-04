# Authorization design and remaining work

This document explains the target architecture and unresolved obligations. It is
not an endpoint or deployment contract. Canonical references are:

- [Runtime authorization](README.md) for shipped resource behavior and Cedar examples.
- [Management HTTP contract](management.md) for bootstrap, delegation, and CRUD.
- [Storage contract](storage-contract.md) for persistence, consistency, and
  infrastructure acceptance requirements.
- Platform setup in `rosa-seaplane/docs/platform-api-authorization.md`.

## Current boundary

AWS IAM identity reaches the API through API Gateway. Cedar evaluates in-process
with `cedar-go`, without an external authorization service on the request path.
The gateway-only network boundary remains unverified, not a deployment guarantee.

Management reads DynamoDB policies and attachments. Resource requests still use
an immutable startup bundle until resolver activation in ROSAENG-67498.
Configured account-scoped administrators bootstrap management without Red Hat
linking or legacy `/accounts` enrollment. They receive no implicit resource access.
`AccessEntry` and `/api/v0/authz/check` are not implemented.

## Target architecture

Each customer account owns its custom policies and attachments. Resources and
policy evaluation stay regional. Policies and global attachments replicate to
local readers through DynamoDB Global Tables. Regional attachments stay local.
A policy can restrict regions through `context.region`.

Policy administration is global because editing a shared policy can change access
in every region. Regional attachment delegation does not confer policy or global
attachment authority. Management defines the exact action groups and grant scopes.

Cedar denies without a permit, and any matching forbid wins. This is not a boundary
on administrators who can remove the forbid. `PolicyAdmin` can manage every
Seaplane policy and attachment for its account. Bootstrap administrators bypass broken
management grants so they can repair them. Neither mechanism bypasses resource
authorization.

Managed policies are immutable catalog entries, not a promised catalog of future
roles. Prefer role attachments for durable grants. STS omits role paths, so paths
cannot isolate roles with the same name. See [principal matching](README.md#principal-matching).

## Activation and production obligations

- Activate persistent resource resolution deliberately, with admission-only
  enrollment and explicit grants. Remove the legacy full-access allowlist only
  after migrating its consumers. Never fall back to stale bundle grants after a
  persistent revocation.
- Preserve account isolation, stored resource ownership, role/session matching,
  forbid precedence, and fail-closed evaluation as resource types and actions grow.
  Update the shipped `Seaplane` schema and tests together. Old `ROSA::` catalogs and
  label-map examples are not contracts.
- Meet the storage contract's writer-fencing, IAM, replication, recovery, and
  teardown gates. Local commits and strong reads do not prove global freshness or
  bounded revocation. Deployment evidence remains outstanding.
- Prove gateway-only reachability, bootstrap administrator identity, and CI customer
  identity before shared rollout. Management authority is not a CI resource grant.
- Verify observability and integrated behavior at matching API, operator, and test
  revisions. Local tests do not prove deployed metrics delivery.

## Deferred identity and client decisions

The former proposal required AWS account-to-Red Hat organization linking and
IAM principal-to-Red Hat user linking, including Org Admin or `ROSAAdmin` authority.
Those flows and hypothetical `rosactl` commands are removed from current delivery.
They are not bootstrap prerequisites or implemented API promises.

Any future linking design must decide organization ownership, export-control and
ban enforcement, and the effect on existing resources and cluster API access.
IAM-internal MFA condition keys and session tags are not available in the current
gateway context. Do not treat caller-controlled user-agent values as identity
proof. Future authorization-check APIs and cross-region CLI discovery need their
own contracts, not restoration of the old examples.
