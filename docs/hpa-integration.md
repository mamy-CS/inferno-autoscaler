# HPA Integration with Inferno Autoscaler

This guide shows how to integrate Kubernetes Horizontal Pod Autoscaler (HPA) with Inferno Autoscaler using the existing deployment.

## Overview

After deploying Inferno, this integration allows:
- **Inferno**: Analyzes workload and emits `inferno_desired_replicas` metrics
- **HPA**: Reads those metrics and scales Kubernetes Deployments accordingly
- **Prometheus Adapter**: Bridges Prometheus metrics to Kubernetes custom metrics API

## Prerequisites

- Inferno deployed (follow treadme for guide on deployment)
- Prometheus stack already running in `inferno-autoscaler-monitoring` namespace
- All components must be fully ready before proceeding (allow 2-3 minutes after deployment)

## Quick Setup

> **Note**: The required RBAC permissions for Prometheus to access Inferno's secure HTTPS metrics endpoint are automatically deployed via `config/rbac/prometheus_metrics_auth_role_binding.yaml`.

### 1. Create Prometheus CA ConfigMap

Prometheus is deployed with TLS (HTTPS) for security. The Prometheus Adapter needs to connect to Prometheus at https://kube-prometheus-stack-prometheus...But Prometheus uses self-signed certificates (not trusted by default). We use a CA configmap for TLS Certificate Verification.

```bash
# Extract the TLS certificate from the prometheus-tls secret
kubectl get secret prometheus-tls -n inferno-autoscaler-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt

# Create ConfigMap with the certificate
kubectl create configmap prometheus-ca --from-file=ca.crt=/tmp/prometheus-ca.crt -n inferno-autoscaler-monitoring
```

### 2. Deploy Prometheus Adapter

Note: The yaml snippet is found at the bottom of this page. Take a look at Prometheus Adapter Values yaml. Create a config file in /config/samples dir. 

```bash
# Add Prometheus community helm repo
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Deploy Prometheus Adapter with Inferno metrics configuration
helm install prometheus-adapter prometheus-community/prometheus-adapter \
  -n inferno-autoscaler-monitoring \
  -f config/samples/prometheus-adapter-values.yaml
```

### 3. Wait for Prometheus Discovery

```bash
# Wait for Prometheus to discover and scrape Inferno metrics (30-60 seconds)
sleep 60
```

### 4. Create the VariantAutoscaling resource

```bash
# Apply the VariantAutoscaling resource if not already done
kubectl apply -f hack/vllme/deploy/vllme-setup/vllme-variantautoscaling.yaml
```

### 5. Deploy HPA Resources

Note: The yaml snippet is found at the bottom of this page. Take a look at HPA Configuration Example yaml. Create a config file in /config/samples dir. 

```bash
# Deploy HPA for your deployments
kubectl apply -f config/samples/hpa-integration.yaml
```

### 6. Verify Integration

```bash
# Wait for all components to be ready (1-2 minutes total)
sleep 90

# Check HPA status - should show actual values, not <unknown>
kubectl get hpa -n llm-d-sim

# Check VariantAutoscaling
kubectl get variantautoscaling -n llm-d-sim

# Check if external metrics are available
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1"
```

**Expected Timeline (much faster now):**
- Prometheus Adapter ready: ~30 seconds
- Metrics scraping starts: ~30-60 seconds  
- External Metrics API active: ~60-90 seconds
- HPA shows values: ~90 seconds

## How It Works

```
Inferno Controller → Prometheus → Prometheus Adapter → HPA → Deployment
(emits metrics)     (scrapes)    (exposes via API)   (scales)  (replicas)
```

1. **Inferno Controller** processes VariantAutoscaling and emits `inferno_desired_replicas` metrics
2. **Prometheus** scrapes these metrics from Inferno's `/metrics` endpoint using TLS
3. **Prometheus Adapter** exposes them to Kubernetes external metrics API
4. **HPA** reads `inferno_desired_replicas` and adjusts Deployment replicas accordingly

## Configuration Files

### Prometheus Adapter Values (`config/samples/prometheus-adapter-values.yaml`)
```yaml
prometheus:
  url: https://kube-prometheus-stack-prometheus.inferno-autoscaler-monitoring.svc.cluster.local
  port: 9090

rules:
  external:
  - seriesQuery: 'inferno_desired_replicas{variant_name!="",exported_namespace!=""}'
    resources:
      overrides:
        exported_namespace: {resource: "namespace"}
        variant_name: {resource: "deployment"}  
    name:
      matches: "^inferno_desired_replicas"
      as: "inferno_desired_replicas"
    metricsQuery: 'inferno_desired_replicas{<<.LabelMatchers>>}'

replicas: 2
logLevel: 4

tls:
  enable: false # Inbound TLS (Client → Adapter)

extraVolumes:
  - name: prometheus-ca
    configMap:
      name: prometheus-ca

extraVolumeMounts:
  - name: prometheus-ca
    mountPath: /etc/prometheus-ca
    readOnly: true

extraArguments:
  - --prometheus-ca-file=/etc/prometheus-ca/ca.crt
```

### HPA Configuration Example
```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllme-deployment-hpa
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: vllme-deployment
  maxReplicas: 10
  metrics:
  - type: External
    external:
      metric:
        name: inferno_desired_replicas
        selector:
          matchLabels:
            variant_name: vllme-deployment
      target:
        type: AverageValue
        averageValue: "1"
```

## Expected Behavior

### Successful Integration
```bash
$ kubectl get hpa -n llm-d-sim
NAME                   REFERENCE                     TARGETS   MINPODS   MAXPODS   REPLICAS
vllme-deployment-hpa   Deployment/vllme-deployment   1/1       1         10        1

$ kubectl get variantautoscaling -n llm-d-sim
NAME               MODEL             ACCELERATOR   CURRENTREPLICAS   OPTIMIZED
vllme-deployment   default/default   A100          1                 1
```

### Scaling Flow
1. **Inferno optimizes** and determines target replicas (e.g., 3)
2. **Inferno emits** `inferno_desired_replicas{variant_name="vllme-deployment"} 3`
3. **HPA detects** change and scales Deployment from 1 to 3 replicas
4. **Deployment scales** and Inferno sees the new current state

## Verification Commands

### Check HPA Metrics Integration
```bash
# Basic HPA status - look for actual values instead of <unknown>
kubectl get hpa -n llm-d-sim

# Detailed HPA status with conditions
kubectl describe hpa vllme-deployment-hpa -n llm-d-sim

# Test external metrics API directly
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-sim/inferno_desired_replicas?labelSelector=variant_name%3Dvllme-deployment"
```

## Files Used

- `config/samples/prometheus-adapter-values.yaml` - Prometheus Adapter configuration with TLS
- `config/samples/hpa-integration.yaml` - HPA resource definitions  
- `hack/vllme/deploy/vllme-setup/vllme-variantautoscaling.yaml` - VariantAutoscaling example

Note: Inferno Autoscaler can leverage Kubernetes HPA's alpha feature for scale to zero functionality, enabling complete resource optimization by scaling deployments down to zero replicas when no load is detected.