#!/usr/bin/env bash
#
# Shared namespace creation for deploy/install.sh environment plugins.
# Each platform must define materialize_namespace(ns) before sourcing this file,
# then call create_namespaces_shared_loop from its create_namespaces().

create_namespaces_shared_loop() {
    log_info "Creating namespaces..."

    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        local ns_exists=false
        local ns_terminating=false

        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status
            ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi

        materialize_namespace "$ns"
        log_success "Namespace $ns created"
    done
}
