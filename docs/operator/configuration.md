# Seaplane Operator Configuration

The seaplane-operator is configured through a combination of CLI flags and
environment variables. Flags are set in the Deployment args; environment
variables are set in the Deployment env section.

## CLI flags

| Flag                          | Required | Default                    | Description                               |
| ----------------------------- | -------- | -------------------------- | ----------------------------------------- |
| `--aws-region`                | Yes      | —                          | AWS region for DynamoDB and EKS           |
| `--base-domain`               | Yes      | —                          | DNS base domain suffix for HostedClusters |
| `--max-concurrent-reconciles` | No       | `10`                       | Concurrent reconciles per controller      |
| `--dynamo-poll-interval`      | No       | `15s`                      | DynamoDB GSI fast-poller interval         |
| `--dynamo-relist-interval`    | No       | `5m`                       | DynamoDB full consistent relist interval  |
| `--dynamo-max-lookback`       | No       | `--dynamo-relist-interval` | Maximum lookback for the GSI poller       |

## Environment variables

### Required

| Variable       | Description                                  |
| -------------- | -------------------------------------------- |
| `POSTGRES_DSN` | PostgreSQL connection string for seaplane-db |

### Optional

| Variable                       | Default | Description                                                                                                                                                                             |
| ------------------------------ | ------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CONTROL_PLANE_OPERATOR_IMAGE` | —       | Development override: when set, every HostedCluster is pinned to this CPO image via the `hypershift.openshift.io/control-plane-operator-image` annotation. Empty disables the override. |
| `ALERTMANAGER_URL`             | —       | Alertmanager v2 API base URL (e.g. `https://alertmanager.example.com`). When set, the cluster silence reconciler is registered. When unset, lifecycle silencing is disabled.            |
| `REPLICA_COUNT`                | `1`     | Number of operator replicas for sharded reconciliation. Each pod uses its StatefulSet ordinal to claim a shard.                                                                         |

### ECR image mirror

When configured, the operator populates `HostedCluster.spec.imageContentSources`
for every cluster, redirecting OCP image pulls from `quay.io` to the
configured zero-egress ECR registry. This is a platform-managed override —
customers do not set `imageContentSources` directly.

All five variables must be set together, or none at all. Setting some but not
all — including omitting `ECR_MIRROR_BASE_SECRET_NAME` — causes the operator to
exit with an error at startup. Setting `ECR_MIRROR_BASE_SECRET_NAME` to
`"pull-secret"` is also rejected; use a different name such as
`base-pull-secret`.

| Variable                      | Description                                                                 |
| ----------------------------- | --------------------------------------------------------------------------- |
| `ECR_MIRROR_REGISTRY_ACCOUNT` | AWS account ID of the ECR registry                                          |
| `ECR_MIRROR_REGISTRY_REGION`  | AWS region of the ECR registry                                              |
| `ECR_MIRROR_REPOSITORY`       | ECR repository name                                                         |
| `ECR_MIRROR_SOURCES`          | Comma-separated list of source registry paths to redirect to the ECR mirror |
| `ECR_MIRROR_BASE_SECRET_NAME` | Target Secret name for the ESO ExternalSecret. See note below.              |

> **ESO coordination:** `ECR_MIRROR_BASE_SECRET_NAME` must be set to a name
> other than `pull-secret` (e.g. `base-pull-secret`). The operator redirects
> the ESO ExternalSecret to that name so ESO writes the SSM credentials there
> while `ecr-creds-sync` reads from it and writes the merged result (SSM
> credentials + ECR token) to `pull-secret`. This prevents ESO and
> `ecr-creds-sync` from overwriting each other.

**Example:**

```
ECR_MIRROR_REGISTRY_ACCOUNT=123456789012
ECR_MIRROR_REGISTRY_REGION=us-east-1
ECR_MIRROR_REPOSITORY=example-org/ocp-mirror
ECR_MIRROR_SOURCES=source-registry-a.example.com/org/repo,source-registry-b.example.com/org/repo
ECR_MIRROR_BASE_SECRET_NAME=base-pull-secret
```

**Startup log confirmation:**

The operator logs one of the following on startup:

- `ECR mirror disabled` — none of the five variables are set; clusters use original image sources
- `ECR mirror enabled account=… region=… repository=… sources=N` — mirror is active
- Error and exit — partial configuration detected; missing variables are named

**How it works:**

The operator sets `spec.imageContentSources` on the HyperShift `HostedCluster`
to redirect pulls for each source listed in `ECR_MIRROR_SOURCES` to the ECR
mirror. This generates `/etc/containers/registries.conf` on dataplane nodes via
the ignition config, so nodes pull OCP images from ECR without requiring
internet access.
