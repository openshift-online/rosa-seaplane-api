# Konflux container images on Quay

This repository builds container images on Konflux (`rosa-tenant` on `kflux-prd-rh02`). Builds are defined under [`.tekton/`](../../.tekton/).

| Component                | Quay repository                                                    | PipelineRun names                                                          |
| ------------------------ | ------------------------------------------------------------------ | -------------------------------------------------------------------------- |
| `rosa-seaplane-api`      | `quay.io/redhat-user-workloads/rosa-tenant/rosa-seaplane-api`      | `rosa-seaplane-api-on-pull-request`, `rosa-seaplane-api-on-push`           |
| `rosa-seaplane-operator` | `quay.io/redhat-user-workloads/rosa-tenant/rosa-seaplane-operator` | `rosa-seaplane-operator-on-pull-request`, `rosa-seaplane-operator-on-push` |

Pull-request and push builds **share the same Quay repository** (one ImageRepository per component). That is expected Konflux behavior, not a separate “PR” vs “release” repo.

Each Quay repository is named after its Konflux **component** (`rosa-seaplane-api`, `rosa-seaplane-operator`).

## When pipelines run

| Pipeline                                 | Trigger (summary)                                                                                           |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `rosa-seaplane-api-on-push`              | Every push to `main`                                                                                        |
| `rosa-seaplane-api-on-pull-request`      | Pull requests targeting `main`                                                                              |
| `rosa-seaplane-operator-on-push`         | Push to `main` only when `seaplane-operator/`, `seaplane-db/`, or related Tekton/Containerfile paths change |
| `rosa-seaplane-operator-on-pull-request` | Same path filter on pull requests                                                                           |

A commit SHA tag for `rosa-seaplane-operator` exists on Quay **only if** that commit triggered the operator on-push (or on-pull-request) pipeline. A `seaplane-api`-only merge still produces a new `rosa-seaplane-api:<sha>` image on every push to `main`, but does not necessarily rebuild the operator image at that SHA.

## Tag naming

### Pull requests

- **Tag:** `on-pr-<full-git-commit-sha>` (for example `on-pr-a1b2c3d4…`)
- **Expiration:** 5 days (`image-expires-after: 5d` in the pull-request PipelineRun)
- **Purpose:** Validate the change in CI; not intended for long-lived pins

### Merges to `main` (on-push)

- **Primary tag:** `<full-git-commit-sha>` — the **deployable container image** (manifest list / image index)
- **Expiration:** none (tags are retained)
- **Revision:** the push pipeline sets `output-image` to `…/seaplane-api:{{revision}}` (or `seaplane-operator:{{revision}}`), where `revision` is the commit on `main`

Do **not** use Quay `:latest` as the source of truth. Konflux push pipelines tag by git SHA; `latest` in the UI is not a reliable pointer to “what main built last.”

## Why one merge creates several Quay “tags”

A single successful on-push run pushes more than the runnable image. The default multi-platform OCI pipeline also publishes **trusted-build artifacts** into the same repository, using suffix tags on the same commit:

| Tag pattern      | Meaning                                          |
| ---------------- | ------------------------------------------------ |
| `<sha>`          | Runnable container image — **use this for pins** |
| `<sha>.git`      | Git/source artifact for the trusted build chain  |
| `<sha>.prefetch` | Prefetched dependencies (hermetic / gomod cache) |

Source images may be enabled (`build-source-image: true`). Quay’s tag list can look like several entries for one pipeline run; only the plain `<sha>` tag (no suffix) is the image to deploy or reference from `rosa-seaplane` Helm values.

PR builds use the same suffix pattern under `on-pr-<sha>` (for example `on-pr-<sha>.git`).

## Choosing an image to pin

Use this when updating deployment repos (for example `argocd/config/regional-cluster/seaplane-api/values.yaml` in `rosa-seaplane`) or when debugging e2e that depend on a specific binary.

1. Pick the **git commit on `main`** that contains the change you need.
2. Confirm the matching Konflux build succeeded for that component:
   - **Pull requests:** GitHub check `Konflux kflux-prd-rh02 / rosa-seaplane-api-on-pull-request` or `…/rosa-seaplane-operator-on-pull-request` (when the path filter applies).
   - **After merge:** `Konflux kflux-prd-rh02 / rosa-seaplane-api-on-push` or `…/rosa-seaplane-operator-on-push`. If the check is missing, that pipeline did not run for that commit (for example, operator on-push on a commit that only changed `seaplane-api/`).
3. Set the image tag to the **full commit SHA** (no `on-pr-` prefix, no `.git` / `.prefetch` suffix):

   ```yaml
   image:
     repository: quay.io/redhat-user-workloads/rosa-tenant/rosa-seaplane-api
     tag: "<full-commit-sha>"
   ```

4. For operator images, use the `rosa-seaplane-operator` repository and the same SHA rules, but only at commits where the operator pipeline actually ran (see **When pipelines run** above).

If e2e or regional tests fail after a change merges to `seaplane-api` on `main` but pins in `rosa-seaplane` still point at an older SHA, update the pin — pre-merge CI may have used a freshly built image while deployment values still point to an older image.

## Quick reference in Quay

| What you see                  | Interpretation                                                          |
| ----------------------------- | ----------------------------------------------------------------------- |
| `on-pr-*`                     | Pull-request build (short-lived)                                        |
| Plain 40-character hex SHA    | `main` push (or the commit that built the image) — **deployable image** |
| `<sha>.git`, `<sha>.prefetch` | Pipeline artifacts — ignore for deploy pins                             |

## Related configuration

- Pipeline definitions: [`.tekton/rosa-seaplane-api-pull-request.yaml`](../../.tekton/rosa-seaplane-api-pull-request.yaml), [`.tekton/rosa-seaplane-api-push.yaml`](../../.tekton/rosa-seaplane-api-push.yaml), and the matching `rosa-seaplane-operator-*` files
- Tenant ImageRepository registration: [`konflux-release-data`](https://gitlab.cee.redhat.com/releng/konflux-release-data) overlays under `tenants-config/.../rosa-tenant/`
- Deployment pins today: `openshift-online/rosa-seaplane` (`argocd/config/regional-cluster/seaplane-api/values.yaml`, `…/seaplane/values.yaml`)

Automated bump PRs (for example after each merge to `main`) should update pins to the plain `<sha>` tag only.
