/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	yaml "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
)

// handleConfigMapEvent processes ConfigMap events and updates configuration.
// This is the main entry point for ConfigMap watching.
func (r *VariantAutoscalingReconciler) handleConfigMapEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	// We expect a ConfigMap but check to be safe
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}

	logger := ctrl.LoggerFrom(ctx)
	name := cm.GetName()
	namespace := cm.GetNamespace()
	expectedNamespace := config.Namespace()

	// Use r.Config to update configuration
	if r.Config == nil {
		logger.Error(nil, "Config is nil in reconciler - cannot update configuration")
		return nil
	}

	// Check if ConfigMap is being deleted
	isDeleted := !cm.DeletionTimestamp.IsZero()

	// Determine if this is a global or namespace-local ConfigMap
	isGlobal := namespace == expectedNamespace
	isNamespaceLocal := !isGlobal && r.shouldWatchNamespaceLocalConfigMap(ctx, namespace)

	// Handle deletion events
	if isDeleted {
		r.handleConfigMapDeletion(ctx, cm, name, namespace, isNamespaceLocal)
		return nil
	}

	// Only process namespace-local ConfigMaps if namespace is tracked
	if !isGlobal && !isNamespaceLocal {
		return nil
	}

	// Route to appropriate handler based on ConfigMap name
	switch name {
	case config.ConfigMapName():
		r.handleMainConfigMap(ctx, cm, namespace, isGlobal)
	case config.SaturationConfigMapName():
		r.handleSaturationConfigMap(ctx, cm, namespace, isGlobal)
	case config.DefaultScaleToZeroConfigMapName:
		r.handleScaleToZeroConfigMap(ctx, cm, namespace, isGlobal)
	}

	// Config updates are handled by the Engine loop which reads the new configuration.
	// No need to trigger immediate reconciliation for individual VAs.
	return nil
}

// handleConfigMapDeletion handles ConfigMap deletion events.
func (r *VariantAutoscalingReconciler) handleConfigMapDeletion(ctx context.Context, cm *corev1.ConfigMap, name, namespace string, isNamespaceLocal bool) {
	logger := ctrl.LoggerFrom(ctx)

	if !isNamespaceLocal {
		return // Only handle namespace-local ConfigMap deletions
	}

	// Remove namespace-local config on deletion
	if name == config.SaturationConfigMapName() {
		r.Config.UpdateSaturationConfigForNamespace(namespace, make(map[string]interfaces.SaturationScalingConfig))
		logger.Info("Removed namespace-local saturation config on ConfigMap deletion", "namespace", namespace)
	} else if name == config.DefaultScaleToZeroConfigMapName {
		r.Config.UpdateScaleToZeroConfigForNamespace(namespace, make(config.ScaleToZeroConfigData))
		logger.Info("Removed namespace-local scale-to-zero config on ConfigMap deletion", "namespace", namespace)
	}

	// Remove namespace entry to allow fallback to global
	r.Config.RemoveNamespaceConfig(namespace)
}

// handleMainConfigMap handles updates to the main ConfigMap (wva-variantautoscaling-config).
// This ConfigMap is only supported globally, not per-namespace.
// If immutable parameter changes are detected, they are rejected with a warning, but mutable
// parameter updates are still applied.
func (r *VariantAutoscalingReconciler) handleMainConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := ctrl.LoggerFrom(ctx)

	if !isGlobal {
		// Main ConfigMap is only supported globally
		return
	}

	// Check for immutable parameter changes first
	immutableChanges, err := config.DetectImmutableParameterChanges(r.Config, cm.Data)
	immutableKeys := make(map[string]bool)
	if err != nil {
		// Immutable parameters detected - emit warning event and log error
		logger.Error(err, "Attempted to change immutable parameters via ConfigMap",
			"configmap", fmt.Sprintf("%s/%s", namespace, cm.GetName()),
			"changes", immutableChanges)

		// Build set of immutable keys to filter out when processing mutable parameters
		for _, change := range immutableChanges {
			immutableKeys[change.Key] = true
		}

		// Emit Kubernetes Warning event
		if r.Recorder != nil {
			var changeList []string
			for _, change := range immutableChanges {
				changeList = append(changeList, fmt.Sprintf("%s (old: %q, new: %q)", change.Parameter, change.OldValue, change.NewValue))
			}
			r.Recorder.Eventf(
				cm,
				corev1.EventTypeWarning,
				"ImmutableConfigChangeRejected",
				"ConfigMap %s/%s attempted to change immutable parameters that require controller restart: %s. These changes were rejected. Mutable parameter updates were still applied.",
				namespace,
				cm.GetName(),
				fmt.Sprintf("%v", changeList),
			)
		}

		// Continue processing mutable parameters (don't return early)
		logger.Info("Rejected immutable parameter changes, continuing with mutable parameter updates",
			"rejectedKeys", immutableKeys)
	}

	// Process mutable parameters (filter out immutable keys)
	// Optimization Config (Global Interval) - mutable parameter
	if interval, ok := cm.Data["GLOBAL_OPT_INTERVAL"]; ok && !immutableKeys["GLOBAL_OPT_INTERVAL"] {
		if parsedInterval, err := time.ParseDuration(interval); err == nil {
			r.Config.UpdateOptimizationInterval(parsedInterval)
			logger.Info("Updated global optimization config from ConfigMap", "interval", interval)
		} else {
			logger.Error(err, "Invalid GLOBAL_OPT_INTERVAL in ConfigMap", "value", interval)
		}
	}

	// Prometheus cache configuration - mutable parameter
	// Check if any cache config keys are present and not in immutable keys
	cacheConfigKeys := []string{
		"PROMETHEUS_METRICS_CACHE_ENABLED",
		"PROMETHEUS_METRICS_CACHE_TTL",
		"PROMETHEUS_METRICS_CACHE_CLEANUP_INTERVAL",
		"PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL",
		"PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD",
		"PROMETHEUS_METRICS_CACHE_STALE_THRESHOLD",
		"PROMETHEUS_METRICS_CACHE_UNAVAILABLE_THRESHOLD",
	}
	hasCacheConfig := false
	for _, key := range cacheConfigKeys {
		if _, ok := cm.Data[key]; ok && !immutableKeys[key] {
			hasCacheConfig = true
			break
		}
	}
	if hasCacheConfig {
		// Parse and update Prometheus cache config
		cacheConfig := config.ParsePrometheusCacheConfigFromData(cm.Data)
		if cacheConfig != nil {
			r.Config.UpdatePrometheusCacheConfig(cacheConfig)
			logger.Info("Updated Prometheus cache config from ConfigMap",
				"enabled", cacheConfig.Enabled,
				"ttl", cacheConfig.TTL,
				"fetchInterval", cacheConfig.FetchInterval)
		}
	}
}

// handleSaturationConfigMap handles updates to the saturation scaling ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *VariantAutoscalingReconciler) handleSaturationConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := ctrl.LoggerFrom(ctx)

	// Parse saturation scaling config entries
	configs := make(map[string]interfaces.SaturationScalingConfig)
	count := 0
	for key, yamlStr := range cm.Data {
		var satConfig interfaces.SaturationScalingConfig
		if err := yaml.Unmarshal([]byte(yamlStr), &satConfig); err != nil {
			logger.Error(err, "Failed to parse saturation scaling config entry", "key", key)
			continue
		}
		// Validate
		if err := satConfig.Validate(); err != nil {
			logger.Error(err, "Invalid saturation scaling config entry", "key", key)
			continue
		}
		configs[key] = satConfig
		count++
	}

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateSaturationConfig(configs)
		logger.Info("Updated global saturation config from ConfigMap", "entries", count)
	} else {
		r.Config.UpdateSaturationConfigForNamespace(namespace, configs)
		logger.Info("Updated namespace-local saturation config from ConfigMap", "namespace", namespace, "entries", count)
	}
}

// handleScaleToZeroConfigMap handles updates to the scale-to-zero ConfigMap.
// Supports both global and namespace-local ConfigMaps.
func (r *VariantAutoscalingReconciler) handleScaleToZeroConfigMap(ctx context.Context, cm *corev1.ConfigMap, namespace string, isGlobal bool) {
	logger := ctrl.LoggerFrom(ctx)

	// Parse scale-to-zero config
	scaleToZeroConfig := config.ParseScaleToZeroConfigMap(cm.Data)

	// Log parsed config for debugging
	logger.Info("Processing scale-to-zero ConfigMap",
		"name", cm.GetName(),
		"namespace", namespace,
		"isGlobal", isGlobal,
		"configKeys", len(cm.Data),
		"parsedModelCount", len(scaleToZeroConfig))

	// Update global or namespace-local config
	if isGlobal {
		r.Config.UpdateScaleToZeroConfig(scaleToZeroConfig)
		logger.Info("Updated global scale-to-zero config from ConfigMap", "modelCount", len(scaleToZeroConfig))
	} else {
		r.Config.UpdateScaleToZeroConfigForNamespace(namespace, scaleToZeroConfig)
		logger.Info("Updated namespace-local scale-to-zero config from ConfigMap", "namespace", namespace, "modelCount", len(scaleToZeroConfig))
	}
}

// isNamespaceConfigEnabled checks if a namespace has the opt-in label for namespace-local ConfigMaps.
// This allows namespaces to opt-in for ConfigMap watching even before VAs are created.
func (r *VariantAutoscalingReconciler) isNamespaceConfigEnabled(ctx context.Context, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not enabled
		// This is safe - we'll proceed with normal logic
		return false
	}

	labels := ns.GetLabels()
	if labels == nil {
		return false
	}

	value, exists := labels[constants.NamespaceConfigEnabledLabelKey]
	return exists && value == "true"
}

// isNamespaceExcluded checks if a namespace has the exclude annotation.
// Excluded namespaces are not watched for ConfigMaps or reconciled for VAs.
// Thread-safe (reads namespace object from API server).
func (r *VariantAutoscalingReconciler) isNamespaceExcluded(ctx context.Context, namespace string) bool {
	if namespace == "" {
		return false
	}

	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		// If namespace doesn't exist or we can't read it, default to not excluded
		// This is safe - we'll proceed with normal logic
		return false
	}

	annotations := ns.GetAnnotations()
	if annotations == nil {
		return false
	}

	value, exists := annotations[constants.NamespaceExcludeAnnotationKey]
	return exists && value == "true"
}

// shouldWatchNamespaceLocalConfigMap returns true if a namespace-local ConfigMap should be watched.
// It checks exclusion first (highest priority), then VA-based tracking (automatic), then opt-in label (explicit).
func (r *VariantAutoscalingReconciler) shouldWatchNamespaceLocalConfigMap(ctx context.Context, namespace string) bool {
	// Check exclusion first (highest priority - overrides everything)
	if r.isNamespaceExcluded(ctx, namespace) {
		return false
	}

	// Check VA-based tracking (automatic)
	if r.isNamespaceTracked(namespace) {
		return true
	}

	// Check label-based opt-in (explicit)
	return r.isNamespaceConfigEnabled(ctx, namespace)
}
