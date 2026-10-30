# CLAUDE.md

## Project Overview

ROSA Seaplane API — ROSA HCP regional cluster management.

Three components:

- **seaplane-api/** — Stateless REST gateway for API Gateway SigV4-authenticated AWS callers; Cedar authorization (`docs/authz.md`)
- **seaplane-operator/** — Kubernetes operator (Capabilities, Cluster, ClusterNetworking, ControlPlaneUpgradePolicy, DNSReservation, Index, ManagementCluster, Manifest, NodePool, OidcConfig, Placement CRDs)
- **seaplane-db/** — PostgreSQL-backed controller-runtime library

## Build & Test

```bash
make build              # All components
make test               # All unit tests
make lint               # golangci-lint v2 across all modules
make verify-mod         # go.mod tidiness
make deps               # Download and tidy all modules

make build-api          # Platform API
make build-operator     # Fleet operator (manager + compactor)
make build-seaplane-db      # FleetDB library

make test-api           # API unit tests
make test-operator      # Operator unit tests
make test-seaplane-db       # FleetDB unit tests
make test-operator-int  # Operator integration tests (Postgres + DynamoDB)

make generate           # Run all code generators (passthrough, deepcopy, registry, CRDs, conversion, clientset, openapi)
make verify             # Fail if any generated output is out of date

make manifests          # Generate CRDs (controller-gen + CEL strip)
make generate-deepcopy  # Generate deepcopy methods only
make codegen            # Full codegen pipeline (passthrough + registry + verify compile)
make generate-clientset # Regenerate typed clientset from CRD types
make generate-openapi   # Regenerate OpenAPI spec from CRD types

make verify-codegen     # Verify codegen output is up to date
make verify-clientset   # Verify clientset matches committed files
make verify-openapi     # Verify OpenAPI spec is up to date
make verify-mod         # Verify go.mod tidiness

make test-unit          # All unit tests (api, operator, codegen, clientset)
make test-integration   # Integration tests (fleetdb, operator)
make test-api-codegen   # Codegen tool tests
make test-clientset     # Clientset tests
```

## Module Layout

```
seaplane-db/go.mod                    ← standalone
api/go.mod                              ← standalone (CRD types, v1alpha1)
clientset/go.mod                        ← generated typed K8s client for Seaplane CRDs
seaplane-operator/go.mod              ← requires: fleetdb, api
seaplane-api/go.mod                     ← requires: fleetdb, api
hack/api-codegen/go.mod                 ← codegen tools (openapi-gen, crd-variants, conversion-gen)
hack/clientset/cmd/bridge-gen/go.mod      ← bridge and platform generation for clientset
hack/tools/go.mod                       ← dev tooling dependencies
```

Cross-module refs use permanent `replace` directives to sibling dirs.

## Key Conventions

- Multi-module monorepo: separate go.mod per component
- Ginkgo/Gomega for testing
- OpenAPI-first API design
- CRD types in standalone `api/` module, imported by seaplane-operator and seaplane-api
- golangci-lint v2 with custom logcheck plugin

## seaplane-db Guidelines

Changes to `seaplane-api/`, `seaplane-operator/`, or `seaplane-db/` must follow
[docs/seaplane-db-guidelines.md](docs/seaplane-db-guidelines.md).

- Read the guidelines before changing how resources are stored, read, written, owned,
  claimed, or cleaned up.
- Before finishing, check the change against the guidelines' review checklist.
- If a change has to break a rule, say which rule and why. Don't work around it
  silently.
