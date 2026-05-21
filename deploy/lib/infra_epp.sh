#!/usr/bin/env bash
#
# EPP and InferencePool setup for all environments (kind-emulator, kubernetes, openshift).
# Installs Gateway API CRDs, GAIE CRDs, and the GAIE standalone chart (EPP + InferencePool).
#
# Required vars: LLM_D_RELEASE, GAIE_VERSION, LLMD_NS, WVA_PROJECT
# Required funcs: log_info, log_success, log_warning
#

GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-"v1.2.0"}

deploy_epp() {
    log_info "Deploying EPP infrastructure (GAIE standalone chart v${GAIE_VERSION})..."

    local llm_d_dir="$WVA_PROJECT/llm-d"

    # Clone llm-d/llm-d at pinned release for guide values files.
    if [ ! -d "$llm_d_dir/.git" ]; then
        log_info "Cloning llm-d/llm-d at $LLM_D_RELEASE into $llm_d_dir..."
        git clone -b "$LLM_D_RELEASE" -- https://github.com/llm-d/llm-d.git "$llm_d_dir"
    else
        log_info "Using existing llm-d checkout at $llm_d_dir"
    fi

    # Gateway API CRDs (required for InferencePool).
    log_info "Installing Gateway API CRDs (${GATEWAY_API_VERSION})..."
    kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" || \
        log_warning "Gateway API CRD install returned non-zero — may already be present"

    # GAIE CRDs (InferencePool, InferenceModel, InferenceObjective).
    log_info "Installing GAIE CRDs (ref=${GAIE_VERSION})..."
    kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || \
        log_warning "GAIE CRD install returned non-zero — may already be present"

    # llm-d namespace and dummy HF token secret for emulated environments.
    log_info "Creating llm-d namespace ($LLMD_NS)..."
    kubectl create namespace "$LLMD_NS" --dry-run=client -o yaml | kubectl apply -f -
    # Create HF token secret only if it does not already exist (preserves real token on OpenShift CI).
    if ! kubectl get secret llm-d-hf-token -n "$LLMD_NS" &>/dev/null; then
        local hf_token="${HF_TOKEN:-dummy-token}"
        kubectl create secret generic llm-d-hf-token \
            --from-literal="HF_TOKEN=${hf_token}" \
            -n "$LLMD_NS"
        log_info "Created llm-d-hf-token secret in $LLMD_NS"
    else
        log_info "llm-d-hf-token secret already exists in $LLMD_NS — skipping"
    fi

    # GAIE standalone chart — bundles EPP + Envoy proxy; no external gateway controller needed.
    # Override chart's production resource requests (4CPU/8Gi per container) to fit a kind cluster.
    log_info "Installing GAIE standalone chart (release=optimized-baseline)..."

    # When scale-to-zero is enabled, add flowControl feature gate to EPP config so the
    # scale-from-zero engine can read inference_extension_flow_control_queue_size metrics.
    local extra_helm_args=()
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "ENABLE_SCALE_TO_ZERO=true: enabling EPP flowControl feature gate..."
        extra_helm_args=(-f "$(dirname "${BASH_SOURCE[0]}")/epp-flow-control.values.yaml")
    fi

    helm upgrade --install optimized-baseline \
        oci://registry.k8s.io/gateway-api-inference-extension/charts/standalone \
        -f "$llm_d_dir/guides/recipes/scheduler/base.values.yaml" \
        -f "$llm_d_dir/guides/optimized-baseline/scheduler/optimized-baseline.values.yaml" \
        "${extra_helm_args[@]}" \
        --set inferenceExtension.resources.requests.cpu=100m \
        --set inferenceExtension.resources.requests.memory=256Mi \
        --set inferenceExtension.resources.limits.cpu=500m \
        --set inferenceExtension.resources.limits.memory=512Mi \
        --set inferenceExtension.sidecar.resources.requests.cpu=100m \
        --set inferenceExtension.sidecar.resources.requests.memory=128Mi \
        --set inferenceExtension.sidecar.resources.limits.cpu=500m \
        --set inferenceExtension.sidecar.resources.limits.memory=256Mi \
        -n "$LLMD_NS" --version "$GAIE_VERSION" --create-namespace

    # Grant EPP SA permission to create tokenreviews/subjectaccessreviews so its
    # metrics endpoint authentication works (otherwise /metrics returns 500).
    log_info "Applying EPP tokenreview RBAC..."
    kubectl apply -f "$(dirname "${BASH_SOURCE[0]}")/epp-tokenreview-rbac.yaml"
    kubectl create clusterrolebinding optimized-baseline-epp-tokenreview \
        --clusterrole=optimized-baseline-epp-tokenreview \
        --serviceaccount="$LLMD_NS:optimized-baseline-epp" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Wait for EPP to be available.
    log_info "Waiting for EPP deployment (optimized-baseline-epp) in $LLMD_NS..."
    kubectl wait --for=condition=Available deployment/optimized-baseline-epp \
        -n "$LLMD_NS" --timeout=120s || \
        log_warning "EPP not ready yet — check 'kubectl get pods -n $LLMD_NS'"

    log_success "EPP infrastructure deployed (GAIE standalone v${GAIE_VERSION})"
}

undeploy_epp() {
    log_info "Removing EPP infrastructure..."
    helm uninstall optimized-baseline -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    kubectl delete secret llm-d-hf-token -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrolebinding optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    kubectl delete clusterrole optimized-baseline-epp-tokenreview --ignore-not-found 2>/dev/null || true
    log_success "EPP infrastructure removed"
}
