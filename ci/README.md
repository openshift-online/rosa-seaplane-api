# Prow build root

`ci/Containerfile` is this repository's own definition of the Prow build root
image that runs `lint.sh`, `unit-tests.sh`, `verify.sh`, and
`integration-tests.sh`. It uses the same digest-pinned Hummingbird FIPS Go
builder as the production images, matching `rosa-seaplane-kube-applier` and
`rosa-seaplane-ecr-creds-sync`.

That builder already includes Bash, Git, Make, GCC, and GNU findutils, so the
lint, unit-test, and verify workflows need no additional packages: every
generated-output check in `make verify` compares with `git diff --exit-code`,
and the code generators, `golangci-lint`, and `setup-envtest` are all built
from this repository's own Go modules. Its Go toolchain (1.27.x, with
`GOTOOLCHAIN=local`) satisfies every module's Go 1.26.x minimum, and its FIPS
settings (`GODEBUG=fips140=on`) are inherited unchanged.

`make test-integration` starts PostgreSQL and DynamoDB Local containers through
nested rootless podman, which the Hummingbird builder does not ship. `podman`
and `fuse-overlayfs` are therefore still installed on top of it, together with
the `newuidmap`/`newgidmap` capabilities and the `/etc/subuid` and
`/etc/subgid` ranges that rootless podman needs. Both packages come from the
image's own Hummingbird repository, so no extra repository configuration is
required.

The cache directories are made writable by arbitrary UIDs because ci-operator
runs test containers under an unpredictable UID. `GOPATH`, `GOCACHE`, and
`GOMODCACHE` therefore point outside the cloned source tree. The CI scripts
additionally create their own isolated temporary caches per run, so no
cross-run cache persistence is implied by this setup.

Renovate discovers this Containerfile through the Dockerfile manager, so the
existing digest-update rules keep the CI builder reference in step with the
production images.

## Relationship to `openshift/release`

The ci-operator configuration for `main` currently selects the build root with
`build_root.from_repository: true`, which reads the image reference from
`.ci-operator.yaml`, and builds a separate `integration-runner` image for the
`integration` job. Those jobs do not consume `ci/Containerfile` yet, so this
file is the prepared replacement rather than the live build root; switching to
it requires a `build_root.project_image.dockerfile_path: ci/Containerfile`
change in `openshift/release`. Keep `.ci-operator.yaml` until that change
merges.
