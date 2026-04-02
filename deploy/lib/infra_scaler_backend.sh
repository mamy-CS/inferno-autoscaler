#!/usr/bin/env bash
#
# Shared scaler-backend orchestration for deploy/install.sh.
# Uses existing deploy_keda / deploy_prometheus_adapter implementations.
# Requires vars: SCALER_BACKEND, DEPLOY_PROMETHEUS_ADAPTER.
# Requires funcs: deploy_keda(), deploy_prometheus_adapter(), log_info().
#

deploy_scaler_backend() {
    # Deploy scaler backend: KEDA, Prometheus Adapter, or none.
    # KEDA is supported on all environments. On OpenShift and CKS it is typically
    # pre-installed on the cluster; deploy_keda will detect and skip the install.
    # Use SCALER_BACKEND=none on clusters that already have an external metrics API
    # (e.g. llmd benchmark clusters with KEDA pre-installed) to avoid conflicts.
    if [ "$SCALER_BACKEND" = "keda" ]; then
        deploy_keda
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Skipping scaler backend deployment (SCALER_BACKEND=none)"
        log_info "Assumes an external metrics API (e.g. KEDA) is already installed on the cluster"
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        deploy_prometheus_adapter
    else
        log_info "Skipping Prometheus Adapter deployment (DEPLOY_PROMETHEUS_ADAPTER=false)"
    fi
}
