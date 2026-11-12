# CRD manifests (`config/crd/bases`)

These YAML files are the generated OpenAPI/structural schemas for the
`seaplane.io` custom resources (Cluster, NodePool, DNSReservation, OidcConfig,
Placement, ManagementCluster, Index, …).

## What they are used for

- **Tests only.** `envtest`-backed controller tests load them via
  `CRDDirectoryPaths` (see `seaplane-operator/internal/controller/suite_test.go`),
  so the test apiserver validates resources against the real schema. (The
  `seaplane-db/test/parity` suite uses its own fixtures under
  `seaplane-db/examples/crd`, not these files.)
- They are **generated**, never hand-edited. `make manifests` regenerates
  `bases/` from the Go types in `api/v1alpha1` (controller-gen + a CEL strip
  pass). Edit the Go types, then run `make manifests`.

## What they are NOT

- **They are not applied at runtime.** The operator is **not** a Kubernetes
  controller against a kube-apiserver. It runs on `seaplane-db` (pgruntime), a
  controller-runtime implementation backed by **PostgreSQL**, and registers the
  API types through the Go scheme (`v1alpha1.AddToScheme`) — not from these
  YAMLs. There is no CRD registration step in production.
- **There is no Kubernetes RBAC.** Because the operator never calls the
  kube-apiserver, there are no Roles/ClusterRoles/bindings and no kustomize
  deploy scaffold. The deployment path is the Helm chart in
  `seaplane-operator/charts`. Do not re-add `config/rbac`, `config/default`,
  `config/manager`, `+kubebuilder:rbac` markers, or Helm `ClusterRole`/
  `ClusterRoleBinding` templates — `make verify` fails if they reappear. See
  the "No Kubernetes RBAC" note in the repo-root `CLAUDE.md`.

## Why only `bases/` lives here

The kubebuilder kustomize scaffold (`config/{default,manager,rbac,…}`) was
removed because nothing consumes it — Helm is the deploy path and the operator
needs no k8s RBAC. Only `bases/` remains because the tests above depend on it.
