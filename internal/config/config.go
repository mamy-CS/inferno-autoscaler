package config

import (
	"sync"
	"time"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
)

// Config is the unified configuration structure for the WVA controller.
// It contains both static (immutable after startup) and dynamic (runtime-updatable) configuration.
type Config struct {
	// Static configuration (immutable after startup)
	Static StaticConfig

	// Dynamic configuration (can be updated via ConfigMap changes)
	// Protected by mutex for thread-safe updates
	mu      sync.RWMutex
	Dynamic DynamicConfig
}

// StaticConfig holds configuration that is immutable after startup.
// These settings are loaded once at startup and cannot be changed at runtime.
type StaticConfig struct {
	// Infrastructure settings (CLI flags)
	MetricsAddr          string // Metrics bind address (e.g., ":8443", "0" to disable)
	ProbeAddr            string // Health probe bind address (e.g., ":8081")
	EnableLeaderElection bool   // Whether leader election is enabled
	LeaderElectionID     string // Leader election ID
	LeaseDuration        time.Duration
	RenewDeadline        time.Duration
	RetryPeriod          time.Duration
	RestTimeout          time.Duration
	SecureMetrics        bool   // Whether metrics endpoint uses HTTPS
	EnableHTTP2          bool   // Whether HTTP/2 is enabled
	WatchNamespace       string // Namespace to watch (empty = all namespaces)
	LoggerVerbosity      int    // Logger verbosity level

	// TLS certificate paths
	WebhookCertPath string
	WebhookCertName string
	WebhookCertKey  string
	MetricsCertPath string
	MetricsCertName string
	MetricsCertKey  string

	// Connection settings (ConfigMap)
	Prometheus *interfaces.PrometheusConfig // Prometheus connection config (required)
	EPPConfig  *EPPConfig                   // EPP integration config (optional)

	// Feature flags (ConfigMap)
	ScaleToZeroEnabled          bool // WVA_SCALE_TO_ZERO feature flag
	LimitedModeEnabled          bool // WVA_LIMITED_MODE feature flag
	ScaleFromZeroMaxConcurrency int  // SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY
}

// EPPConfig holds EPP (Endpoint Pool) integration configuration.
type EPPConfig struct {
	// EPP-specific settings
	// Add fields as needed
}

// DynamicConfig holds configuration that can be updated at runtime via ConfigMap changes.
// All access must be protected by the Config's mutex.
type DynamicConfig struct {
	// Optimization settings
	OptimizationInterval time.Duration // GLOBAL_OPT_INTERVAL

	// Saturation scaling configuration (per-accelerator)
	SaturationConfig map[string]interfaces.SaturationScalingConfig

	// Scale-to-zero configuration (per-model)
	ScaleToZeroConfig ScaleToZeroConfigData

	// Prometheus metrics cache configuration
	PrometheusCache *CacheConfig
}

// GetOptimizationInterval returns the current optimization interval.
// Thread-safe.
func (c *Config) GetOptimizationInterval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Dynamic.OptimizationInterval
}

// GetSaturationConfig returns the current saturation scaling configuration.
// Thread-safe. Returns a copy to prevent external modifications.
func (c *Config) GetSaturationConfig() map[string]interfaces.SaturationScalingConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Return a copy to prevent external modifications
	result := make(map[string]interfaces.SaturationScalingConfig, len(c.Dynamic.SaturationConfig))
	for k, v := range c.Dynamic.SaturationConfig {
		result[k] = v
	}
	return result
}

// GetScaleToZeroConfig returns the current scale-to-zero configuration.
// Thread-safe.
func (c *Config) GetScaleToZeroConfig() ScaleToZeroConfigData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Dynamic.ScaleToZeroConfig == nil {
		return make(ScaleToZeroConfigData)
	}
	// Return a copy to prevent external modifications
	result := make(ScaleToZeroConfigData, len(c.Dynamic.ScaleToZeroConfig))
	for k, v := range c.Dynamic.ScaleToZeroConfig {
		result[k] = v
	}
	return result
}

// GetPrometheusCacheConfig returns the current Prometheus cache configuration.
// Thread-safe.
func (c *Config) GetPrometheusCacheConfig() *CacheConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Dynamic.PrometheusCache == nil {
		return nil
	}
	// Return a copy
	cp := *c.Dynamic.PrometheusCache
	return &cp
}

// UpdateDynamicConfig updates the entire dynamic configuration.
// Thread-safe. Should be called when ConfigMap changes are detected.
func (c *Config) UpdateDynamicConfig(dynamic DynamicConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Dynamic = dynamic
}

// UpdateOptimizationInterval updates the optimization interval.
// Thread-safe.
func (c *Config) UpdateOptimizationInterval(interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Dynamic.OptimizationInterval = interval
}

// UpdateSaturationConfig updates the saturation scaling configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateSaturationConfig(config map[string]interfaces.SaturationScalingConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Make a copy to prevent external modifications
	c.Dynamic.SaturationConfig = make(map[string]interfaces.SaturationScalingConfig, len(config))
	for k, v := range config {
		c.Dynamic.SaturationConfig[k] = v
	}
}

// UpdateScaleToZeroConfig updates the scale-to-zero configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateScaleToZeroConfig(config ScaleToZeroConfigData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Make a copy to prevent external modifications
	c.Dynamic.ScaleToZeroConfig = make(ScaleToZeroConfigData, len(config))
	for k, v := range config {
		c.Dynamic.ScaleToZeroConfig[k] = v
	}
}

// UpdatePrometheusCacheConfig updates the Prometheus cache configuration.
// Thread-safe.
func (c *Config) UpdatePrometheusCacheConfig(cacheConfig *CacheConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cacheConfig == nil {
		c.Dynamic.PrometheusCache = nil
	} else {
		// Make a copy
		cp := *cacheConfig
		c.Dynamic.PrometheusCache = &cp
	}
}

// GetDynamicConfig returns a copy of the current dynamic configuration.
// Thread-safe.
func (c *Config) GetDynamicConfig() DynamicConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Dynamic
}
