package prometheus

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
)

// getDefaultCacheConfig returns default cache configuration
func getDefaultCacheConfig() *config.CacheConfig {
	return &config.CacheConfig{
		Enabled:             true, // enabled by default
		TTL:                 30 * time.Second,
		CleanupInterval:     1 * time.Minute,
		FetchInterval:       30 * time.Second, // Fetch every 30s by default
		FreshnessThresholds: config.DefaultFreshnessThresholds(),
	}
}

// InvalidateCacheForVariant invalidates cache entries for a specific variant
// This should be called when replica counts change or deployments are updated
func (pc *PrometheusCollector) InvalidateCacheForVariant(modelID, namespace, variantName string) {
	if pc.cache != nil {
		// Construct prefix using PrometheusCollector's key format: {modelID}/{namespace}/{variantName}/
		prefix := fmt.Sprintf("%s/%s/%s/", modelID, namespace, variantName)
		pc.cache.InvalidateByPrefix(prefix)
		ctrl.Log.V(logging.DEBUG).Info("Invalidated cache for variant", "model", modelID, "namespace", namespace, "variant", variantName)
	}
}

// InvalidateCacheForModel invalidates all cache entries for a model
// This should be called when model-level changes occur
func (pc *PrometheusCollector) InvalidateCacheForModel(modelID, namespace string) {
	if pc.cache != nil {
		// Construct prefix using PrometheusCollector's key format: {modelID}/{namespace}/
		prefix := fmt.Sprintf("%s/%s/", modelID, namespace)
		pc.cache.InvalidateByPrefix(prefix)
		ctrl.Log.V(logging.DEBUG).Info("Invalidated cache for model", "model", modelID, "namespace", namespace)
	}
}
