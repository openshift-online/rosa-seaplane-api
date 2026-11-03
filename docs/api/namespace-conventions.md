# Namespace Conventions

**Last Updated Date**: 2026-10-06

## Summary

Customer-facing FleetDB resources use an account namespace as the tenancy boundary.
Operator-managed resources that live on a management cluster use a separate
UID-derived namespace; that namespace does not determine FleetDB ownership.

## Account-Scoped FleetDB Resources

**Pattern**: `account-<accountID>`

The account ID comes from the authenticated caller. seaplane-api rejects a supplied
namespace that does not match the caller's canonical account namespace and sets the
namespace itself when the request omits it.

| Resource        | Namespace             | Name                         |
| --------------- | --------------------- | ---------------------------- |
| Cluster         | `account-<accountID>` | Client-selected DNS label    |
| NodePool        | `account-<accountID>` | `<cluster>.<child>`          |
| Placement       | `account-<accountID>` | `<cluster>.placement`        |
| OidcConfig      | `account-<accountID>` | Client-selected name         |
| DNSReservation  | `account-<accountID>` | Client-selected name         |

Names identify human-facing resources; FleetDB UIDs identify object incarnations.
NodePool resolves its parent using the Cluster-name prefix and account namespace,
then stores the parent UID in its ownerReference and `seaplane.io/cluster-uid`
label. OidcConfig and DNSReservation claims are likewise held by Cluster UID.

## Management-Cluster Resources

When the operator renders HyperShift resources onto a management cluster, it uses
`cluster-<Cluster UID>` as the management-cluster namespace. The FleetDB Cluster,
NodePool, Placement, and reservation remain in `account-<accountID>`.

## Operator-Controlled Resources

`ManagementCluster` is cluster-scoped. Internal uniqueness `Index` resources use
operator-controlled namespaces, including `dns-shard-0-reservations` for DNS
prefixes and `oidc-issuer-reservations` for issuer URLs. Index ownership is recorded
with `seaplane.io/owner-uid`.

## Name Validation

Cluster `metadata.name` is a DNS label up to 63 characters. NodePool names use
`<cluster>.<child>` with each part independently validated as a DNS label up to 63
characters. HyperShift NodePool resources use only the child portion of that name.

## Authorization

API Gateway authenticates AWS callers. Cedar authorizes every resource operation
before it reads or writes data ([authorization](../authz.md)). The account
namespace provides storage isolation; it is derived from authenticated identity,
not trusted as a client-provided authorization claim.
