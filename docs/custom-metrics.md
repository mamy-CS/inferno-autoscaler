# Custom Metrics Documentation

The Inferno Autoscaler exposes a focused set of custom metrics that provide insights into the autoscaling behavior and optimization performance. These metrics are exposed via Prometheus and can be used for monitoring, alerting, and dashboard creation.

## Metrics Overview

All custom metrics are prefixed with `inferno_` and include labels for `variant_name`, `namespace`, and other relevant dimensions to enable detailed analysis and filtering.

## Optimization Metrics

*No optimization metrics are currently exposed. Optimization timing is logged at DEBUG level.*

## Replica Management Metrics

### `inferno_current_replicas`
- **Type**: Gauge
- **Description**: Current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor current scaling state

### `inferno_desired_replicas`
- **Type**: Gauge
- **Description**: Desired number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Compare desired vs current replicas to detect scaling issues

### `inferno_replica_scaling_total`
- **Type**: Counter
- **Description**: Total number of replica scaling operations
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `direction`: Direction of scaling (up, down)
  - `reason`: Reason for scaling
- **Use Case**: Track scaling frequency and reasons

## Configuration

### Metrics Endpoint
The metrics are exposed at the `/metrics` endpoint on port 8443 (HTTPS).

### ServiceMonitor Configuration
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: inferno-autoscaler
  namespace: inferno-autoscaler-system
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
  - port: http
    scheme: http
    interval: 30s
    path: /metrics
```

## Example Queries

### Basic Queries
```promql
# Current replicas by variant
inferno_current_replicas

# Scaling frequency
rate(inferno_replica_scaling_total[5m])

# Desired replicas by variant
inferno_desired_replicas
```

### Advanced Queries
```promql
# Scaling frequency by direction
rate(inferno_replica_scaling_total{direction="scale_up"}[5m])

# Replica count mismatch
abs(inferno_desired_replicas - inferno_current_replicas)

# Scaling frequency by reason
rate(inferno_replica_scaling_total[5m]) by (reason)
```