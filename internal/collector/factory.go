package collector

import (
	"fmt"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// convertCacheConfig converts from collector.CacheConfig to prometheus.CacheConfig.
// This breaks the import cycle by converting at the factory boundary.
//
// This is the conversion pattern used for all collector plugins:
// - Controller reads collector.CacheConfig (public API from types.go)
// - Factory converts to collector-specific type (prometheus.CacheConfig, future: epp.CacheConfig)
// - Collector plugin uses its own type internally
//
// When adding a new collector plugin (e.g., EPP), create a similar conversion function
// that converts collector.CacheConfig to the new collector's CacheConfig type.
func convertCacheConfig(cfg *CacheConfig) *prometheus.CacheConfig {
	if cfg == nil {
		return nil
	}
	return &prometheus.CacheConfig{
		Enabled:         cfg.Enabled,
		TTL:             cfg.TTL,
		MaxSize:         cfg.MaxSize,
		CleanupInterval: cfg.CleanupInterval,
		FetchInterval:   cfg.FetchInterval,
		FreshnessThresholds: prometheus.FreshnessThresholds{
			FreshThreshold:       cfg.FreshnessThresholds.FreshThreshold,
			StaleThreshold:       cfg.FreshnessThresholds.StaleThreshold,
			UnavailableThreshold: cfg.FreshnessThresholds.UnavailableThreshold,
		},
	}
}

// CollectorType represents the type of metrics collector plugin/implementation
type CollectorType string

const (
	// CollectorTypePrometheus is the Prometheus collector plugin
	CollectorTypePrometheus CollectorType = "prometheus"
	// CollectorTypeEPP is the EPP direct collector plugin (placeholder - not yet implemented)
	CollectorTypeEPP CollectorType = "epp"
)

// Config holds configuration for creating a metrics collector plugin
type Config struct {
	Type        CollectorType // Type of collector plugin to create
	PromAPI     promv1.API    // Required for Prometheus collector plugin
	CacheConfig *CacheConfig  // Optional cache configuration (nil = use defaults)
	// K8sClient will be set via SetK8sClient method after creation if needed
}

// NewMetricsCollector creates a new metrics collector plugin based on the collector type.
// Defaults to Prometheus collector if type is not specified or unknown.
func NewMetricsCollector(config Config) (interfaces.MetricsCollector, error) {
	collectorType := config.Type
	if collectorType == "" {
		collectorType = CollectorTypePrometheus // Default to Prometheus collector
	}

	switch collectorType {
	case CollectorTypePrometheus:
		if config.PromAPI == nil {
			return nil, fmt.Errorf("PromAPI is required for Prometheus collector")
		}
		return prometheus.NewPrometheusCollectorWithConfig(config.PromAPI, convertCacheConfig(config.CacheConfig)), nil
	case CollectorTypeEPP:
		return nil, fmt.Errorf("EPP collector plugin is not yet implemented")
	default:
		// Default to Prometheus collector for unknown types
		if config.PromAPI == nil {
			return nil, fmt.Errorf("PromAPI is required for Prometheus collector")
		}
		return prometheus.NewPrometheusCollectorWithConfig(config.PromAPI, convertCacheConfig(config.CacheConfig)), nil
	}
}

// NewPrometheusMetricsCollector is a convenience function to create a Prometheus collector.
// This maintains backward compatibility with existing code.
func NewPrometheusMetricsCollector(promAPI promv1.API) interfaces.MetricsCollector {
	return prometheus.NewPrometheusCollectorWithConfig(promAPI, nil) // Use defaults
}

// NewPrometheusCollector creates a new Prometheus metrics collector with default cache config.
// This is a convenience function for backward compatibility.
// Deprecated: Use collector.NewMetricsCollector or prometheus.NewPrometheusCollectorWithConfig instead.
// Returns the PrometheusCollector which implements interfaces.MetricsCollector.
func NewPrometheusCollector(promAPI promv1.API) interfaces.MetricsCollector {
	return prometheus.NewPrometheusCollector(promAPI)
}
