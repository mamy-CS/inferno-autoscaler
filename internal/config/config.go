package config

import (
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

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
	// EPP metric reader bearer token for pod scraping
	MetricReaderBearerToken string // EPP_METRIC_READER_BEARER_TOKEN
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
	oldInterval := c.Dynamic.OptimizationInterval
	c.Dynamic.OptimizationInterval = interval
	c.mu.Unlock()
	if oldInterval != interval {
		ctrl.Log.Info("Updated optimization interval", "old", oldInterval, "new", interval)
	}
}

// UpdateSaturationConfig updates the saturation scaling configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateSaturationConfig(config map[string]interfaces.SaturationScalingConfig) {
	c.mu.Lock()
	oldCount := len(c.Dynamic.SaturationConfig)
	c.mu.Unlock()
	// Make a copy to prevent external modifications
	newConfig := make(map[string]interfaces.SaturationScalingConfig, len(config))
	for k, v := range config {
		newConfig[k] = v
	}
	c.mu.Lock()
	c.Dynamic.SaturationConfig = newConfig
	newCount := len(c.Dynamic.SaturationConfig)
	c.mu.Unlock()
	if oldCount != newCount {
		ctrl.Log.Info("Updated saturation config", "oldEntries", oldCount, "newEntries", newCount)
	}
}

// UpdateScaleToZeroConfig updates the scale-to-zero configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateScaleToZeroConfig(config ScaleToZeroConfigData) {
	c.mu.Lock()
	oldCount := len(c.Dynamic.ScaleToZeroConfig)
	c.mu.Unlock()
	// Make a copy to prevent external modifications
	newConfig := make(ScaleToZeroConfigData, len(config))
	for k, v := range config {
		newConfig[k] = v
	}
	c.mu.Lock()
	c.Dynamic.ScaleToZeroConfig = newConfig
	newCount := len(c.Dynamic.ScaleToZeroConfig)
	c.mu.Unlock()
	if oldCount != newCount {
		ctrl.Log.Info("Updated scale-to-zero config", "oldModels", oldCount, "newModels", newCount)
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

// NewTestConfig creates a minimal Config for testing purposes.
// It provides sensible defaults for all required fields.
// This helper is intended for use in unit tests, integration tests, and e2e tests
// where a valid Config instance is needed but full configuration is not required.
// NOTE: This function is exported for testing purposes only and should not be used in production code.
func NewTestConfig() *Config {
	return &Config{
		Static: StaticConfig{
			MetricsAddr:                 "0",
			ProbeAddr:                   ":8081",
			EnableLeaderElection:        false,
			LeaderElectionID:            "test-election-id",
			LeaseDuration:               60 * time.Second,
			RenewDeadline:               50 * time.Second,
			RetryPeriod:                 10 * time.Second,
			RestTimeout:                 60 * time.Second,
			SecureMetrics:               false,
			EnableHTTP2:                 false,
			WatchNamespace:              "",
			LoggerVerbosity:             0,
			ScaleToZeroEnabled:          false,
			LimitedModeEnabled:          false,
			ScaleFromZeroMaxConcurrency: 10,
		},
		Dynamic: DynamicConfig{
			OptimizationInterval: 60 * time.Second,
			SaturationConfig:     make(map[string]interfaces.SaturationScalingConfig),
			ScaleToZeroConfig:    make(ScaleToZeroConfigData),
		},
	}
}
