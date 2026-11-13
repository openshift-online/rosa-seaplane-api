# E2E Tests

End-to-end integration and functional tests for the Seaplane Platform API.

## Prerequisites

```bash
go install github.com/onsi/ginkgo/v2/ginkgo@latest
```

## Running Tests

```bash
# Run with ginkgo
cd test/e2e-api
ginkgo -v

# Run with go test
cd test/e2e-api
go test -v
```

## Environment Variables

- `E2E_BASE_URL`: Base URL of the API server (default: `http://localhost:8000`)
- `E2E_TOKEN`: Authentication token for API requests
- `E2E_RHOBS_API_URL`: RHOBS API Gateway URL for observability tests (optional — tests are skipped if unset)

## Local authorization test

`make test-e2e-authz` builds the API binary and runs `TestAuthzHTTP` against
disposable PostgreSQL 16 and pinned DynamoDB Local containers. Podman must
have both images cached. The test uses dummy AWS credentials and dynamic
loopback ports. It refuses an external `PGCTL_DSN` and stops only its own
containers and processes.

The test seeds an `admin` role management grant directly into DynamoDB, in the
[pre-production seed format](../../docs/authz/storage-contract.md#pre-production-seed).
It enrolls resource accounts, then seeds `testdata/resource-grants.json` through
management HTTP as `admin` role sessions. The API never reads that fixture.
Enrollment alone grants no resource permissions.

The suite checks resource CRUD, labels, pagination, account isolation,
Cedar context and forbids, runtime failure diagnostics, and metrics. Live
policy edits and revocation require no restart. An in-flight request retains
a previously read DynamoDB policy while a fresh request sees the edit.
Stale management revisions and invalid or missing startup configuration fail.
Logs go to `test-results/authz-http-*`.

## Note

These are integration/functional tests, separate from unit tests in `pkg/`.
