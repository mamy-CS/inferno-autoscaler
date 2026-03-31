#!/bin/bash
#
# Workload-Variant-Autoscaler Kubernetes Environment-Specific Configuration
# This script provides Kubernetes-specific functions and variable overrides
# It is sourced by the main install.sh script
# Note: it is NOT meant to be executed directly
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

#
# Kubernetes-specific Prometheus Configuration
# Note: overriding defaults from common script
#
PROMETHEUS_SVC_NAME="kube-prometheus-stack-prometheus"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-"true"}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-"true"}

check_specific_prerequisites() {
    log_info "No Kubernetes-specific prerequisites needed other than common prerequisites"
}

# Deploy WVA prerequisites for Kubernetes
deploy_wva_prerequisites() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # Extract Prometheus CA certificate
    log_info "Extracting Prometheus TLS certificate"
    kubectl get secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH

    if [ "$SKIP_TLS_VERIFY" = "true" ]; then
        log_warning "TLS verification NOT enabled: using values-dev.yaml for dev deployments - NOT FOR PRODUCTION USE"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values-dev.yaml"
    else
        log_info "TLS verification enabled: using values.yaml for production deployments"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values.yaml"
    fi

    log_success "WVA prerequisites complete"
}

materialize_namespace() {
    kubectl create namespace "$1"
}

create_namespaces() {
    local _deploy_lib_dir
    _deploy_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
    # shellcheck source=create_namespaces.sh
    source "${_deploy_lib_dir}/create_namespaces.sh"
    create_namespaces_shared_loop
}

_wva_deploy_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
# shellcheck source=deploy_prometheus_kube_stack.sh
source "${_wva_deploy_lib}/deploy_prometheus_kube_stack.sh"
# shellcheck source=delete_namespaces_kube_like.sh
source "${_wva_deploy_lib}/delete_namespaces_kube_like.sh"

# Deploy Prometheus on Kubernetes
deploy_prometheus_stack() {
    deploy_prometheus_kube_stack
}

# Kubernetes-specific Undeployment functions
undeploy_prometheus_stack() {
    undeploy_prometheus_kube_stack
}

delete_namespaces() {
    delete_namespaces_kube_like
}

# Environment-specific functions are now sourced by the main install.sh script
# Do not call functions directly when sourced

