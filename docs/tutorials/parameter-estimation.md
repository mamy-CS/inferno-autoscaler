# Guide to Offline Benchmarking for WVA's Model Analyzer

> **⚠️ DEPRECATED**: This guide describes parameter estimation for the `modelProfile` field, which has been removed from the VariantAutoscaling CRD. The model-based optimization feature that used these parameters is not currently available in the CRD specification. This documentation is kept for reference purposes only.

This guide explains how to collect performance parameters (alpha, beta, gamma, delta) for WVA's model analyzer using offline benchmarking. We will use `vllm` for model serving and `guidellm` for benchmarking, all deployed on a cluster.

## Overview

WVA requires four performance parameters to optimize inference workloads:
- **Alpha (α) and Beta (β)**: Decode phase parameters for ITL (Inter-Token Latency)
- **Gamma (γ) and Delta (δ)**: Prefill phase parameters for TTFT (Time To First Token)

These parameters are estimated by running two benchmark jobs:
1. **Synchronous benchmark**: Measures ITL and TTFT at batch size = 1
2. **Throughput benchmark**: Measures ITL and TTFT at maximum batch size (e.g., 64)

---

## Prerequisites

1. **Cluster**: With GPU nodes available
2. **vLLM Deployment**: Serving your model (see [vllm-samples.md](vllm-samples.md))
3. **Guidellm Image**: `ghcr.io/vllm-project/guidellm:latest` (publicly available - latest is fine, as long as the image has synchronous and throughput rate types.)
4. **HuggingFace Token**: If using gated models (stored as Kubernetes Secret)
5. **Persistent Volume Claim**: For model cache (optional but recommended)

---

## Step 1: Deploy vLLM for Benchmarking

### Option 1: Use Template from hack Folder (Recommended)

A complete vLLM deployment template is available in `hack/vllm-benchmark-deployment.yaml`.

1. **Copy and customize the template**:
   ```bash
   cp hack/vllm-benchmark-deployment.yaml vllm-benchmark-deployment.yaml
   ```

2. **Replace placeholders** in the file:
   - `<namespace>` → Your namespace (e.g., `vllm-test`)
   - `<vllm-service-name>` → Your service name (e.g., `vllm`)
   - `<model-id>` → Your model (e.g., `unsloth/Meta-Llama-3.1-8B`)
   - `<secret-name>` → Your HF token secret (e.g., `hf-token-secret`)
   - `<pvc-name>` → Your PVC name (e.g., `vllm-models-cache`)

3. **Important: Set max batch size**:
   Ensure `--max-num-seqs` matches your desired maximum batch size (default: 64).
   This value will be used in the throughput benchmark.

4. **Deploy**:
   ```bash
   kubectl apply -f vllm-benchmark-deployment.yaml
   ```

5. **Wait for vLLM to be ready**:
   ```bash
   kubectl wait --for=condition=ready pod -n <namespace> -l app=<vllm-service-name> --timeout=2400s
   ```

   **Note**: First deployment may take 15-35 minutes for model download.

### Option 2: Manual Deployment

See [vllm-samples.md](vllm-samples.md) for detailed manual deployment instructions.

**Key requirement**: Your vLLM deployment must include `--max-num-seqs <batch-size>` in the args (e.g., `--max-num-seqs 64`).

---

## Step 2: Prepare Benchmark Jobs

### Use Template from hack Folder (Recommended)

1. **Copy and customize the benchmark template**:
   ```bash
   cp hack/benchmark-jobs-template.yaml benchmark-jobs.yaml
   ```

2. **Replace placeholders** in the file:
   - `<namespace>` → Your namespace (e.g., `vllm-test`)
   - `<vllm-service-name>` → Your vLLM service name (e.g., `vllm`)
   - `<model-id>` → Your model (e.g., `unsloth/Meta-Llama-3.1-8B`)
   - `<max-batch-size>` → Must match vLLM `--max-num-seqs` value (e.g., `64`)

3. **Optional: Adjust token counts**:
   The default uses `prompt_tokens=128,output_tokens=128`. Adjust if your workload differs:
   ```yaml
   - "--data"
   - "prompt_tokens=<your-input-tokens>,output_tokens=<your-output-tokens>"
   ```

---

## Step 3: Understanding the Parameters

### Decode Parameters (Alpha and Beta)

**Formula**: `ITL = α + β × batch_size`

- **Alpha (α)**: Fixed overhead per token generation (ms)
- **Beta (β)**: Variable overhead per additional request in batch (ms)

**What ITL measures**: Time between consecutive tokens during generation.

### Prefill Parameters (Gamma and Delta)

**Formula**: `TTFT = γ + δ × inputTokens × batch_size`

- **Gamma (γ)**: Fixed overhead per prefill operation (ms)
- **Delta (δ)**: Variable overhead per input token per batch member (ms per token per batch member)

**What TTFT measures**: Time to first token, including prompt processing and queue wait time.

---

## Step 4: Run Synchronous Benchmark (Batch Size = 1)

### Deploy Synchronous Benchmark Job

```bash
kubectl apply -f benchmark-jobs.yaml
```

**Note**: Only the synchronous job will start first. The throughput job will run after the synchronous job completes.

### Wait for Completion

```bash
# Monitor the job
kubectl get jobs -n <namespace> | grep sync

# Wait for completion
kubectl wait --for=condition=complete job/<model-id>-sync-benchmark -n <namespace> --timeout=600s
```

### Extract ITL and TTFT Values

```bash
# View logs
kubectl logs job/<model-id>-sync-benchmark -n <namespace>

# Extract ITL value (look for "ITL (ms)" section)
kubectl logs job/<model-id>-sync-benchmark -n <namespace> | grep -A 3 "ITL (ms)" | grep "mean"

# Extract TTFT value (look for "TTFT (ms)" section)
kubectl logs job/<model-id>-sync-benchmark -n <namespace> | grep -A 3 "TTFT (ms)" | grep "mean"
```

### Expected Output Example

```
╭─ Benchmarks ─────────────────────────────────────────────────────────────────╮
│ ... Tok:  140.7 gen/s,  423.4 tot/s,  15.0ms TTFT,    7.0ms ITL,  ... │
╰──────────────────────────────────────────────────────────────────────────────╯

Benchmarks Stats:
... | TTFT (ms)       ||| ITL (ms)        ||| ...
... | mean| median|  p99| mean| median| p99| ...
... | 15.0|   14.8| 17.0|  7.0|    7.0| 7.2| ...
```

**Record these values**:
- `ITL_sync = <mean ITL value>` (e.g., 7.0 ms)
- `TTFT_sync = <mean TTFT value>` (e.g., 15.0 ms)

---

## Step 5: Run Throughput Benchmark (Maximum Batch Size)

### Delete Throughput Job (if it auto-started)

If both jobs started simultaneously, delete the throughput job to run sequentially:

```bash
kubectl delete job <model-id>-throughput-benchmark -n <namespace>
```

### Deploy Throughput Benchmark Job

After synchronous job completes, create the throughput job:

```bash
# Edit benchmark-jobs.yaml and remove synchronous job, or create separately
kubectl apply -f benchmark-jobs.yaml
```

### Wait for Completion

```bash
# Monitor the job
kubectl get jobs -n <namespace> | grep throughput

# Wait for completion
kubectl wait --for=condition=complete job/<model-id>-throughput-benchmark -n <namespace> --timeout=600s
```

### Extract ITL and TTFT Values

```bash
# View logs
kubectl logs job/<model-id>-throughput-benchmark -n <namespace>

# Extract ITL value
kubectl logs job/<model-id>-throughput-benchmark -n <namespace> | grep -A 3 "ITL (ms)" | grep "mean"

# Extract TTFT value
kubectl logs job/<model-id>-throughput-benchmark -n <namespace> | grep -A 3 "TTFT (ms)" | grep "mean"
```

### Expected Output Example

```
╭─ Benchmarks ─────────────────────────────────────────────────────────────────╮
│ ... Tok: 7251.6 gen/s, 21819.9 tot/s,  26.0ms TTFT,    8.7ms ITL,  ... │
╰──────────────────────────────────────────────────────────────────────────────╯

Benchmarks Stats:
... | TTFT (ms)       ||| ITL (ms)        ||| ...
... | mean| median|  p99| mean| median| p99| ...
... | 26.0|   25.5| 35.4|  8.7|    8.6| 8.8| ...
```

**Record these values**:
- `ITL_throughput = <mean ITL value>` (e.g., 8.7 ms)
- `TTFT_throughput = <mean TTFT value>` (e.g., 26.0 ms)
- `maxBatchSize = <value from vLLM --max-num-seqs>` (e.g., 64)
- `inputTokens = <value from --data>` (e.g., 128)

---

## Step 6: Calculate Alpha and Beta (Decode Parameters)

### Equations

You now have two data points for ITL:
1. `ITL_sync = α + β` (batch_size = 1)
2. `ITL_throughput = α + (β × maxBatchSize)` (batch_size = maxBatchSize)

### Solving for Beta (β)

$$\beta = \frac{ITL_\text{throughput} - ITL_\text{synchronous}}{maxBatchSize - 1}$$

**Example**:
```
β = (8.7 - 7.0) / (64 - 1)
β = 1.7 / 63
β = 0.027 ms
```

### Solving for Alpha (α)

$$\alpha = ITL_\text{synchronous} - \beta$$

**Example**:
```
α = 7.0 - 0.027
α = 6.973 ms
```

### Verification

Verify your calculations:
```
ITL(batch=1)  = α + β = 6.973 + 0.027 = 7.000 ms (should match ITL_sync)
ITL(batch=64) = α + (β × 64) = 6.973 + (0.027 × 64) = 8.701 ms (should match ITL_throughput)
```

---

## Step 7: Calculate Gamma and Delta (Prefill Parameters)

### Equations

You now have two data points for TTFT (from the same benchmark runs):
1. `TTFT_sync = γ + (δ × inputTokens × 1)` (batch_size = 1)
2. `TTFT_throughput = γ + (δ × inputTokens × maxBatchSize)` (batch_size = maxBatchSize)

### Solving for Delta (δ)

$$\delta = \frac{TTFT_\text{throughput} - TTFT_\text{synchronous}}{inputTokens \times (maxBatchSize - 1)}$$

**Example** (assuming inputTokens = 128):
```
δ = (26.0 - 15.0) / (128 × (64 - 1))
δ = 11.0 / (128 × 63)
δ = 11.0 / 8064
δ = 0.001364 ms
```

### Solving for Gamma (γ)

$$\gamma = TTFT_\text{synchronous} - (\delta \times inputTokens \times 1)$$

**Example** (assuming inputTokens = 128):
```
γ = 15.0 - (0.001364 × 128 × 1)
γ = 15.0 - 0.175
γ = 14.825 ms
```

### Verification

Verify your calculations:
```
TTFT(batch=1)  = γ + (δ × 128 × 1) = 14.825 + (0.001364 × 128) = 15.000 ms
TTFT(batch=64) = γ + (δ × 128 × 64) = 14.825 + (0.001364 × 8192) = 26.000 ms
```

---

## Step 8: Summary and Usage

### Final Parameters

After completing all steps, you should have:

**Decode Parameters**:
- **Alpha (α)**: Fixed ITL overhead (ms)
- **Beta (β)**: Variable ITL per batch member (ms)

**Prefill Parameters**:
- **Gamma (γ)**: Fixed TTFT overhead (ms)
- **Delta (δ)**: Variable TTFT per token per batch member (ms)

### Use in VariantAutoscaling

> **Note**: The `modelProfile` field shown below is not currently part of the VariantAutoscaling CRD specification. This section is kept for reference only. If you need model-based optimization features, please check the latest WVA documentation or contact the maintainers.

The parameters calculated in this tutorial were intended for the following configuration:

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: <your-variant-name>
  namespace: <your-namespace>
spec:
  modelID: <your-model-id>
  # Note: modelProfile field is not currently supported in the CRD
  # modelProfile:
  #   accelerators:
  #     - acc: "<accelerator-type>"  # e.g., A100
  #       accCount: 1
  #       perfParms:
  #         decodeParms:
  #           alpha: "<calculated-alpha>"  # e.g., "6.973"
  #           beta: "<calculated-beta>"    # e.g., "0.027"
  #         prefillParms:
  #           gamma: "<calculated-gamma>"  # e.g., "14.825"
  #           delta: "<calculated-delta>"  # e.g., "0.001364"
  #       maxBatchSize: <max-batch-size>   # e.g., 64
```

---

## Quick Reference

### Benchmark Data Collection

| Benchmark | Batch Size | Measures | Data Points |
|-----------|------------|----------|-------------|
| Synchronous | 1 | ITL, TTFT | ITL_sync, TTFT_sync |
| Throughput | maxBatchSize | ITL, TTFT | ITL_throughput, TTFT_throughput |

### Calculation Formulas

**Beta (decode)**:
```
β = (ITL_throughput - ITL_sync) / (maxBatchSize - 1)
```

**Alpha (decode)**:
```
α = ITL_sync - β
```

**Delta (prefill)**:
```
δ = (TTFT_throughput - TTFT_sync) / (inputTokens × (maxBatchSize - 1))
```

**Gamma (prefill)**:
```
γ = TTFT_sync - (δ × inputTokens × 1)
```

---

## Example: Complete Workflow

```bash
# 1. Set variables
export NAMESPACE=vllm-test
export MODEL_ID=unsloth/Meta-Llama-3.1-8B
export VLLM_SERVICE=vllm
export MAX_BATCH_SIZE=64
export INPUT_TOKENS=128

# 2. Customize and deploy vLLM
sed "s/<namespace>/$NAMESPACE/g; s/<vllm-service-name>/$VLLM_SERVICE/g; s/<model-id>/$MODEL_ID/g" \
  hack/vllm-benchmark-deployment.yaml > vllm-deployment.yaml
kubectl apply -f vllm-deployment.yaml

# 3. Wait for vLLM ready
kubectl wait --for=condition=ready pod -n $NAMESPACE -l app=$VLLM_SERVICE --timeout=2400s

# 4. Customize and deploy benchmark jobs
sed "s/<namespace>/$NAMESPACE/g; s/<vllm-service-name>/$VLLM_SERVICE/g; s/<model-id>/$MODEL_ID/g; s/<max-batch-size>/$MAX_BATCH_SIZE/g" \
  hack/benchmark-jobs-template.yaml > benchmark-jobs.yaml
kubectl apply -f benchmark-jobs.yaml

# 5. Delete throughput job (run sequentially)
kubectl delete job ${MODEL_ID}-throughput-benchmark -n $NAMESPACE

# 6. Wait for synchronous benchmark
kubectl wait --for=condition=complete job/${MODEL_ID}-sync-benchmark -n $NAMESPACE --timeout=600s

# 7. Extract synchronous results
ITL_SYNC=$(kubectl logs job/${MODEL_ID}-sync-benchmark -n $NAMESPACE | grep -A 3 "ITL (ms)" | grep "mean" | awk '{print $NF}' | head -1)
TTFT_SYNC=$(kubectl logs job/${MODEL_ID}-sync-benchmark -n $NAMESPACE | grep -A 3 "TTFT (ms)" | grep "mean" | awk '{print $NF}' | head -1)

# 8. Deploy throughput benchmark
kubectl apply -f benchmark-jobs.yaml

# 9. Wait for throughput benchmark
kubectl wait --for=condition=complete job/${MODEL_ID}-throughput-benchmark -n $NAMESPACE --timeout=600s

# 10. Extract throughput results
ITL_THROUGHPUT=$(kubectl logs job/${MODEL_ID}-throughput-benchmark -n $NAMESPACE | grep -A 3 "ITL (ms)" | grep "mean" | awk '{print $NF}' | head -1)
TTFT_THROUGHPUT=$(kubectl logs job/${MODEL_ID}-throughput-benchmark -n $NAMESPACE | grep -A 3 "TTFT (ms)" | grep "mean" | awk '{print $NF}' | head -1)

# 11. Calculate parameters
python << EOF
itl_sync = $ITL_SYNC
itl_throughput = $ITL_THROUGHPUT
ttft_sync = $TTFT_SYNC
ttft_throughput = $TTFT_THROUGHPUT
max_batch = $MAX_BATCH_SIZE
input_tokens = $INPUT_TOKENS

# Decode parameters
beta = (itl_throughput - itl_sync) / (max_batch - 1)
alpha = itl_sync - beta

# Prefill parameters
delta = (ttft_throughput - ttft_sync) / (input_tokens * (max_batch - 1))
gamma = ttft_sync - (delta * input_tokens)

print(f"Alpha: {alpha:.6f}")
print(f"Beta: {beta:.6f}")
print(f"Gamma: {gamma:.6f}")
print(f"Delta: {delta:.6f}")
EOF
```

---

## Additional Resources

- **vLLM Deployment Guide**: [vllm-samples.md](vllm-samples.md)
- **Guidellm Setup**: [guidellm-sample.md](guidellm-sample.md)
- **Template Files**: `hack/vllm-benchmark-deployment.yaml`, `hack/benchmark-jobs-template.yaml`
