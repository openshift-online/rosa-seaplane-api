# ROSA Seaplane API

ROSA HCP regional cluster management — platform API, operator, and backing database library.

| Directory              | Description                                             |
| ---------------------- | ------------------------------------------------------- |
| `api/`                 | CRD types and API definitions (v1alpha1)                |
| `seaplane-api/`        | REST gateway (SigV4 callers, Cedar authz)               |
| `seaplane-operator/` | Kubernetes operator (cluster lifecycle controller for Cluster, NodePool, DNSReservation, OidcConfig, Placement, ManagementCluster, Manifest, and other CRDs) |
| `seaplane-db/`       | PostgreSQL-backed controller-runtime library            |
| `clientset/`           | Generated typed Kubernetes client for Seaplane CRDs   |
| `hack/`                | Code generation tools and dev tooling                   |
| `test/`                | E2E tests (API, CLI, monitoring)                        |

## Quick Start

```bash
make build   # all components → bin/
make test    # all unit tests
make lint    # golangci-lint
make help    # full target list
```

## Module Layout

```
seaplane-db/go.mod             ← standalone
api/go.mod                       ← standalone (CRD types)
seaplane-operator/go.mod       ← requires: seaplane-db, api
seaplane-api/go.mod              ← requires: seaplane-db, api
```

## Docs

- [Platform API Helm chart](platform-api/charts/README.md)
- [OpenAPI spec](api/v1alpha1/public/openapi.yaml)
- [Authorization](docs/authz.md) ([target design](docs/authz-design.md))
- [Konflux / Quay image tags](docs/konflux/quay-image-tags.md)
