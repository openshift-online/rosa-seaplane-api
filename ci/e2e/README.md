# E2E suites (`ci/e2e/`)

Each subdirectory is one **e2e suite** with an executable `run` entry point. The
openshift/release runner (`on-demand-e2e-v2`) discovers every `ci/e2e/<suite>/run`
across the checked-out repos and runs the ones named in `E2E_SUITES`. Suites are
independent - no shared state, no ordering.

## Entry-point contract

Every `run` script resolves the repo root via `git`, reads inputs from the
environment (`e2e.env` / `E2E_SUITE_ENV`), runs `make test-e2e-<suite>`, and exits
non-zero on failure with JUnit + logs in `$ARTIFACT_DIR` (`TEST_OUTPUT_DIR` honors
`ARTIFACT_DIR`). It must be executable (`100755`) - discovery silently skips files
without the bit - and isolated, cleaning up anything it creates.

## Suites

| Suite | Make target | Cluster | Required env | Area |
|---|---|---|---|---|
| **api** | `test-e2e-api` | No | `BASE_URL` | Platform API over HTTP: OIDC config lifecycle, rate limiting, authorization, rendered resources. |
| **monitoring** | `test-e2e-platform-monitoring` | No | `BASE_URL`, `RHOBS_API_URL` | Observability + alerting via RHOBS/Thanos. |
| **rosa-cli** | `test-e2e-rosa-cli` | Yes | `BASE_URL`, `CUSTOMER_AWS_PROFILE` | Builds the rosa CLI and runs rosa's own seaplane e2e (CLI surface), profile/label-driven. |
| **lifecycle** | `test-e2e-lifecycle` | Yes | `BASE_URL`, `CUSTOMER_AWS_PROFILE`, `ROSA_CLI_BIN` | Core acceptance suite: provisions its own HCP via the clientset (rosa CLI for VPC/IAM/OIDC); asserts DNS reservation, managed ingress DNS + DNS01 cert, KAS DNS/TLS, alert silencing, the `hcp:hostedcluster_available` metric, nodepool scaling, and teardown. |

## Notes

- A suite in `E2E_SUITES` but absent from the checked-out source fails the job
  (`unknown suite '<name>'`). That source is the **deployed** component pin, so a
  new suite only runs once the provisioned pin advances past the commit adding its
  `run` script.
- Specs needing an optional endpoint (`E2E_ALERTMANAGER_URL`, `E2E_RHOBS_API_URL`)
  skip when it is unset rather than failing. In CI the rosa-seaplane pre-suite
  exports `E2E_ALERTMANAGER_URL` (Alertmanager tunnel); to run the `lifecycle`
  silence specs locally, `source ci/alertmanager-forward.sh && start_alertmanager_forward`
  from a rosa-seaplane checkout first.
