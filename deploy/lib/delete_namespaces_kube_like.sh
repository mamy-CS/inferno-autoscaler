#!/usr/bin/env bash
#
# Shared namespace deletion for Kubernetes-like install plugins (kubernetes, kind-emulator).
# Requires log_* and kubectl. Uses LLMD_NS, WVA_NS, MONITORING_NAMESPACE and DEPLOY_LLM_D,
# DEPLOY_WVA, DEPLOY_PROMETHEUS to skip namespaces that were not part of the deploy.

delete_namespaces_kube_like() {
    log_info "Deleting namespaces..."

    for ns in $LLMD_NS $WVA_NS $MONITORING_NAMESPACE; do
        if kubectl get namespace $ns &> /dev/null; then
            if [[ "$ns" == "$LLMD_NS" && "$DEPLOY_LLM_D" == "false" ]] || [[ "$ns" == "$WVA_NS" && "$DEPLOY_WVA" == "false" ]] || [[ "$ns" == "$MONITORING_NAMESPACE" && "$DEPLOY_PROMETHEUS" == "false" ]]; then
                log_info "Skipping deletion of namespace $ns as it was not deployed"
            else
                log_info "Deleting namespace $ns..."
                kubectl delete namespace $ns 2>/dev/null || \
                    log_warning "Failed to delete namespace $ns"
            fi
        fi
    done

    log_success "Namespaces deleted"
}
