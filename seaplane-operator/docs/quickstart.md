# Quickstart

Deploy the seaplane-operator and connect it to PostgreSQL and DynamoDB.

## Prerequisites

- A PostgreSQL database (14+)
- DynamoDB tables for each management cluster
- An IAM role for the operator with DynamoDB permissions described below

## 1. PostgreSQL

The operator stores all Custom Resources in PostgreSQL via pgruntime. Create a database and provide the connection string via the `POSTGRES_DSN` environment variable:

```
postgres://user:password@host:5432/seaplane?sslmode=require
```

pgruntime runs schema migrations automatically on startup.

## 2. IAM Role

Create an IAM role that the operator's ServiceAccount will assume via [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html) or IRSA.

### DynamoDB

Per management cluster, there are 6 tables. The operator writes to specs tables and reads from status tables:

| Action                      | Tables                                                                                                        |
| --------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `dynamodb:PutItem`          | `{mc}-specs-applydesires`, `{mc}-specs-deletedesires`, `{mc}-specs-readdesires`                               |
| `dynamodb:DeleteItem`       | `{mc}-specs-applydesires`, `{mc}-specs-deletedesires`, `{mc}-specs-readdesires`                               |
| `dynamodb:GetItem`          | `{mc}-specs-applydesires`, `{mc}-status-applydesires`, `{mc}-status-deletedesires`, `{mc}-status-readdesires` |
| `dynamodb:DescribeTable`    | `{mc}-status-applydesires`, `{mc}-status-readdesires`                                                         |
| `dynamodb:DescribeStream`   | `{mc}-status-applydesires`, `{mc}-status-readdesires` (stream ARN)                                            |
| `dynamodb:GetShardIterator` | `{mc}-status-applydesires`, `{mc}-status-readdesires` (stream ARN)                                            |
| `dynamodb:GetRecords`       | `{mc}-status-applydesires`, `{mc}-status-readdesires` (stream ARN)                                            |

The operator also needs `GetItem` on its own specs tables (for the write-through cache hash check). The role should **not** have write access to status tables — that is kube-applier-aws's domain.

## 3. DynamoDB Tables

Create 6 DynamoDB tables per management cluster, each with a single string partition key `documentID`:

```
{mc}-specs-applydesires
{mc}-specs-deletedesires
{mc}-specs-readdesires
{mc}-status-applydesires
{mc}-status-deletedesires
{mc}-status-readdesires
```

Use on-demand billing (`PAY_PER_REQUEST`).

The operator uses GSI two-speed polling on the status tables to detect updates from kube-applier-aws. No DynamoDB Streams configuration is required.

## 4. Install the Helm Chart

```bash
helm install seaplane-operator charts/seaplane-operator \
  --namespace seaplane-operator --create-namespace \
  --set awsRegion=us-east-1 \
  --set baseDomain=example.com
```

### Required Values

| Value        | Description                  |
| ------------ | ---------------------------- |
| `awsRegion`  | AWS region for DynamoDB      |
| `baseDomain` | DNS base domain for clusters |

### Optional Values

| Value                        | Default                                      | Description                        |
| ---------------------------- | -------------------------------------------- | ---------------------------------- |
| `image.repository`           | `quay.io/cbusse_openshift/seaplane-operator` | Container image                    |
| `image.tag`                  | `latest`                                     | Image tag                          |
| `serviceAccount.annotations` | `{}`                                         | SA annotations (set IAM role ARN)  |
| `replicaCount`               | `1`                                          | Number of replicas (= shard count) |

## 5. Create a ManagementCluster

Register management clusters by creating ManagementCluster CRs (the operator reads these from PostgreSQL):

```yaml
apiVersion: seaplane.io/v1alpha1
kind: ManagementCluster
metadata:
  name: mc01
spec:
  region: us-east-1
  accountId: "123456789012"
```

## Verify

Check the operator logs:

```bash
kubectl logs -n seaplane-operator sts/seaplane-operator -f
```

Create a test Cluster CR (namespace = AWS account ID) and verify the operator creates a Placement and writes ApplyDesires to DynamoDB.
