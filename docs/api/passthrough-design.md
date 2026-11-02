# Passthrough Codegen Design

## Overview

The passthrough codegen pipeline generates Go struct types that mirror upstream HyperShift API types (`HostedClusterSpec`, `NodePoolSpec`) into the Seaplane `api/v1alpha1` package. Each mirrored struct (e.g., `HostedClusterSpecPassthrough`) carries curated markers that control field visibility, mutability, and feature gating. These markers drive all downstream codegen: OpenAPI schemas, REST types, conversion functions, and CRD variants.

## Goals

1. When HyperShift adds, removes, or renames fields, `make codegen-passthrough` picks up the change and adds it with safe defaults.
2. Human-curated markers (visibility, write-mode) survive the regeneration round-trip.
3. A single source of truth (the committed passthrough file) feeds both the field registry and the regenerated output.
4. The pipeline is verifiable: `git diff --exit-code` after regeneration confirms nothing drifted.

## File roles

| File                                                | Role                                                                                                                                                                                                                                                              |
| --------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `api/v1alpha1/zz_generated.passthrough.go`          | The committed passthrough types. Human-curated markers live here. This is the source of truth for field policy. Despite the `zz_generated` prefix, this file is intentionally hand-edited to curate markers, then regenerated to pick up upstream struct changes. |
| `api/v1alpha1/configuration.go`                     | Local mirror of `ClusterConfiguration` with granular markers on nested fields (machineConfig). Used by the scanner for nested field marker extraction.                                                                                                   |
| `hack/api-codegen/pkg/registry/field_metadata.json` | Generated field registry (JSON). Produced by `marker-scanner` from the passthrough file. Consumed by `passthrough-gen`, `conversion-gen`, and `openapi-gen`.                                                                                                      |
| `hack/api-codegen/pkg/registry/field_metadata.go`   | Generated field registry (Go). Same data as the JSON, importable by Go code.                                                                                                                                                                                      |

## Pipeline

```
                     ┌──────────────────────────────────────────┐
                     │  api/v1alpha1/zz_generated.passthrough.go │
                     │  (committed, human-curated markers)       │
                     └─────────────┬────────────────────────────┘
                                   │
                          make codegen-registry
                        (marker-scanner scans markers)
                                   │
                                   ▼
                     ┌──────────────────────────────────────┐
                     │  hack/api-codegen/pkg/registry/       │
                     │    field_metadata.json                 │
                     │    field_metadata.go                   │
                     └─────────────┬────────────────────────┘
                                   │
                          make codegen-passthrough
                     (passthrough-gen reads registry +
                      HyperShift source via go list)
                                   │
                                   ▼
                     ┌──────────────────────────────────────────┐
                     │  api/v1alpha1/zz_generated.passthrough.go │
                     │  (regenerated, markers preserved)         │
                     └──────────────────────────────────────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    ▼              ▼              ▼
             codegen-conversion  generate-openapi  codegen (CRD verify)
```

The loop is closed: the committed file feeds the registry, and the registry feeds regeneration. When the round-trip is correct, `git diff` after regeneration shows no changes.

## Curation workflow

### Adding markers to an existing field

1. Edit `api/v1alpha1/zz_generated.passthrough.go` — change markers on the field (e.g., set `+k8s:openapi-gen=true` to make it visible).
2. Run `make codegen-registry` to update the registry.
3. Run `make codegen-passthrough` to verify the round-trip (should produce no diff).
4. Run downstream codegen (`make codegen-conversion`, `make generate-openapi`) to propagate the change to REST types and OpenAPI.
5. Commit.

### Picking up new fields from a HyperShift bump

1. Update the HyperShift dependency in `api/go.mod` and `hack/api-codegen/go.mod`.
2. Run `make codegen-registry` (unchanged — scans existing committed file).
3. Run `make codegen-passthrough` — new fields appear with safe defaults:
   - `+k8s:openapi-gen=false` (hidden from public API)
   - `+seaplane:write-mode=service-set` (not customer-writable)
4. Review the diff. Curate markers on any new fields that should be visible or mutable.
5. Re-run `make codegen-registry && make codegen-passthrough` to verify round-trip.
6. Run downstream codegen and commit.

### Removing fields after a HyperShift bump

If HyperShift removes a field from `HostedClusterSpec` or `NodePoolSpec`, `make codegen-passthrough` will regenerate the file without that field. The registry will contain a stale entry for it, but that is harmless — unused registry entries don't affect codegen. The stale entry is cleaned up the next time `make codegen-registry` runs after the regenerated file is committed.

## Marker types

The registry captures the following marker categories from the passthrough file:

| Marker                                                   | Registry field               | Purpose                                                                                                        |
| -------------------------------------------------------- | ---------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `+k8s:openapi-gen=false`                                 | `hidden: true`               | Suppresses automatic Kubernetes OpenAPI generation; mutable fields remain in Seaplane's custom public schema |
| `+seaplane:write-mode=mutable\|immutable\|service-set` | `writeMode`                  | Controls customer mutability                                                                                   |
| `+openshift:enable:FeatureGate=X`                        | `featureGate`                | Field gated behind a feature flag                                                                              |
| `+seaplane:validation:FeatureGateAwareWriteMode:...`   | `featureGateAwareWriteModes` | Write-mode varies by active feature gates                                                                      |

Upstream markers like `+optional` and `+required` are propagated directly from HyperShift source by `passthrough-gen` via `isForwardedMarker()` — they do not go through the registry.

## Safe defaults

When `passthrough-gen` encounters a field from HyperShift that has no entry in the registry (i.e., a newly added field), it applies safe defaults:

- `+k8s:openapi-gen=false` — hidden from the public API until explicitly curated
- `+seaplane:write-mode=service-set` — not customer-writable until explicitly allowed

This ensures new upstream fields don't accidentally become visible or mutable.

## Resolved gaps

The following issues existed in main and were fixed:

1. **Stale embedded registry** (fixed): `passthrough-gen` embedded a copy of `field_metadata.json` via `//go:embed`. Removed the embedded copy and made `-registry` mandatory.

2. **Scanner skips `zz_generated.passthrough.go`** (fixed): Added an exception for `zz_generated.passthrough.go` in the marker scanner's file filter.

3. **Conversion-gen skips `zz_generated.passthrough.go`** (fixed): Same file filter fix in `conversion-gen`'s `parseTypes()`.

4. **Cyclomatic complexity** (fixed): Extracted `parseStructType()` from `conversion-gen`'s `parseTypes()` to bring complexity under the lint threshold.

## Makefile targets

```
make codegen-passthrough    # registry → passthrough-gen → zz_generated.passthrough.go
make codegen-registry       # marker-scanner → field_metadata.{json,go}
make generate               # full pipeline (registry + conversion + openapi + clientset)
make verify-codegen         # CI check: codegen outputs match committed files
```

### Dependency chain

```
codegen-passthrough
  └─ codegen-registry
       ├─ generate-deepcopy    (controller-gen deepcopy)
       └─ build-api-codegen    (builds passthrough-gen, marker-scanner, etc.)
```

`codegen-passthrough` depends on `codegen-registry` to ensure the registry JSON
is fresh before regeneration, then rescans the regenerated passthrough file so
downstream generators consume the final curated markers.

## Verification

The round-trip is verified by:

```bash
make codegen-registry
make codegen-passthrough
git diff --exit-code api/v1alpha1/zz_generated.passthrough.go
```

CI runs this via `make verify-codegen`.
