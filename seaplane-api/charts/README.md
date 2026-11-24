# platform-api Helm Chart

Helm chart for the ROSA Seaplane API.

## Overview

This chart deploys:

- Platform API application with API Gateway IAM authentication, Cedar authorization, and rate limiting
- Service exposing ports 8000 (API), 8081 (health), and 9090 (metrics)
- TargetGroupBinding for AWS Application Load Balancer integration

## Prerequisites

- Kubernetes cluster (EKS recommended)
- AWS Load Balancer Controller installed
- Target Group ARN for the Application Load Balancer

## Configuration

See [values.yaml](values.yaml) for all configuration options. Key settings:

```yaml
platformApi:
  namespace: platform-api

  app:
    name: platform-api
    image:
      registry: "" # Empty selects the shared ECR registry using global.image_registry_account_id.
      repository: rosa-seaplane-api
      tag: "<tag-or-digest>"

  targetGroup:
    arn: "PLACEHOLDER" # AWS Target Group ARN
    targetType: ip
```

For compatibility, a full registry/path in `repository` (the existing
`IMAGE_REPO` pre-merge input) is split by the chart when `registry` is empty.
The `tag` value may be a tag, a digest, or a tag followed by a digest.

## Installation

### Basic Installation

```bash
helm install platform-api ./platform-api/charts
```

### Production Installation with Custom Values

```bash
helm install platform-api ./platform-api/charts \
  --set platformApi.targetGroup.arn="arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"
```

### Using a Custom Values File

Create a `custom-values.yaml`:

```yaml
platformApi:
  app:
    image:
      tag: "v1.2.3"
    args:
      logLevel: debug

  targetGroup:
    arn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"

  deployment:
    replicas: 3

  app:
    resources:
      requests:
        cpu: 200m
        memory: 256Mi
      limits:
        cpu: 1000m
        memory: 1Gi
```

Install with custom values:

```bash
helm install platform-api ./platform-api/charts \
  -f custom-values.yaml
```

## Upgrading

```bash
helm upgrade platform-api ./platform-api/charts \
  --set platformApi.targetGroup.arn="arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/platform-api/abc123def456"
```

## Uninstallation

```bash
helm uninstall platform-api
```

To also delete the namespace:

```bash
kubectl delete namespace platform-api
```

## Parameters

### Application Configuration

| Parameter                          | Description                          | Default                            |
| ---------------------------------- | ------------------------------------ | ---------------------------------- |
| `platformApi.namespace`            | Namespace to deploy into             | `platform-api`                     |
| `platformApi.app.name`             | Application name                     | `platform-api`                     |
| `platformApi.app.image.registry`   | Explicit registry override; empty uses shared ECR | `""`              |
| `platformApi.app.image.repository` | Repository path without registry    | `rosa-seaplane-api`                |
| `platformApi.app.image.tag`        | Container image tag or digest ref   | Set by release                     |
| `platformApi.app.args.logLevel`    | Log level (debug, info, warn, error) | `info`                             |
| `platformApi.deployment.replicas`  | Number of replicas                   | `1`                                |

### Target Group Configuration

| Parameter                            | Description                  | Default         |
| ------------------------------------ | ---------------------------- | --------------- |
| `platformApi.targetGroup.arn`        | AWS Target Group ARN         | `"PLACEHOLDER"` |
| `platformApi.targetGroup.targetType` | Target type (ip or instance) | `ip`            |

## Architecture

```
┌─────────────────────────────────────────┐
│   Application Load Balancer (ALB)      │
└────────────────┬────────────────────────┘
                 │ :8000
                 │
┌────────────────▼────────────────────────┐
│           Platform API :8000            │
└────────────────┬────────────────────────┘
                 │
     ┌───────────┼───────────┐
     │           │           │
     ▼           ▼           ▼
   :8000       :8081       :9090
    API       Health      Metrics
```

## Health Checks

The application exposes health endpoints on port 8081:

- `/healthz` - Liveness probe
- `/readyz` - Readiness probe

Kubernetes probes check these endpoints directly.

## API Endpoints

All API endpoints require the `X-Amz-Account-Id` header with an allowed AWS account ID:

```bash
curl -s http://localhost:8000/api/v0/management_clusters \
  -H "X-Amz-Account-Id: 123456789012"
```

## Troubleshooting

### Check pod status

```bash
kubectl get pods -n platform-api
kubectl describe pod -n platform-api <pod-name>
```

### View logs

```bash
# Application logs
kubectl logs -n platform-api <pod-name> -c platform-api

```

### Check TargetGroupBinding

```bash
kubectl get targetgroupbinding -n platform-api
kubectl describe targetgroupbinding -n platform-api platform-api
```

### Test health endpoints

```bash
# Port-forward to test locally
kubectl port-forward -n platform-api svc/platform-api 8000:8000

curl http://localhost:8000/api/v0/live
```
