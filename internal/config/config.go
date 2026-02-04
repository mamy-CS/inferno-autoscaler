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

// NamespaceConfig holds configuration for a specific namespace.
// This structure is used both for global defaults and namespace-local overrides.
type NamespaceConfig struct {
	// Saturation scaling configuration (per-accelerator)
	SaturationConfig map[string]interfaces.SaturationScalingConfig

	// Scale-to-zero configuration (per-model)
	ScaleToZeroConfig ScaleToZeroConfigData
}

// DynamicConfig holds configuration that can be updated at runtime via ConfigMap changes.
// All access must be protected by the Config's mutex.
//
// The structure supports namespace-local ConfigMap overrides:
// - Global: Default configuration used when no namespace-local override exists
// - NamespaceConfigs: Per-namespace overrides (keyed by namespace name)
//
// Resolution order: namespace-local > global
type DynamicConfig struct {
	// Optimization settings (global only, not namespace-specific)
	OptimizationInterval time.Duration // GLOBAL_OPT_INTERVAL

	// Prometheus metrics cache configuration (global only, not namespace-specific)
	PrometheusCache *CacheConfig

	// Global default configuration (used when namespace-local override doesn't exist)
	Global *NamespaceConfig

	// Namespace-local configuration overrides (keyed by namespace name)
	// Only populated when namespace-local ConfigMaps are detected
	NamespaceConfigs map[string]*NamespaceConfig
}

// GetOptimizationInterval returns the current optimization interval.
// Thread-safe.
func (c *Config) GetOptimizationInterval() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Dynamic.OptimizationInterval
}

// GetSaturationConfig returns the current global saturation scaling configuration.
// Thread-safe. Returns a copy to prevent external modifications.
// For namespace-aware lookups, use GetSaturationConfigForNamespace instead.
func (c *Config) GetSaturationConfig() map[string]interfaces.SaturationScalingConfig {
	return c.GetSaturationConfigForNamespace("")
}

// GetSaturationConfigForNamespace returns the saturation scaling configuration for the given namespace.
// Resolution order: namespace-local > global
// Thread-safe. Returns a copy to prevent external modifications.
// If namespace is empty, returns global config.
func (c *Config) GetSaturationConfigForNamespace(namespace string) map[string]interfaces.SaturationScalingConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var sourceConfig map[string]interfaces.SaturationScalingConfig

	// Check namespace-local first (if namespace is provided)
	if namespace != "" {
		if nsConfig, exists := c.Dynamic.NamespaceConfigs[namespace]; exists {
			if len(nsConfig.SaturationConfig) > 0 {
				sourceConfig = nsConfig.SaturationConfig
			}
		}
	}

	// Fall back to global if no namespace-local config found
	if sourceConfig == nil {
		if c.Dynamic.Global != nil {
			sourceConfig = c.Dynamic.Global.SaturationConfig
		}
	}

	// Return a copy to prevent external modifications
	if sourceConfig == nil {
		return make(map[string]interfaces.SaturationScalingConfig)
	}
	result := make(map[string]interfaces.SaturationScalingConfig, len(sourceConfig))
	for k, v := range sourceConfig {
		result[k] = v
	}
	return result
}

// GetScaleToZeroConfig returns the current global scale-to-zero configuration.
// Thread-safe.
// For namespace-aware lookups, use GetScaleToZeroConfigForNamespace instead.
func (c *Config) GetScaleToZeroConfig() ScaleToZeroConfigData {
	return c.GetScaleToZeroConfigForNamespace("")
}

// GetScaleToZeroConfigForNamespace returns the scale-to-zero configuration for the given namespace.
// Resolution order: namespace-local > global
// Thread-safe. Returns a copy to prevent external modifications.
// If namespace is empty, returns global config.
func (c *Config) GetScaleToZeroConfigForNamespace(namespace string) ScaleToZeroConfigData {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var sourceConfig ScaleToZeroConfigData

	// Check namespace-local first (if namespace is provided)
	if namespace != "" {
		if nsConfig, exists := c.Dynamic.NamespaceConfigs[namespace]; exists {
			if len(nsConfig.ScaleToZeroConfig) > 0 {
				sourceConfig = nsConfig.ScaleToZeroConfig
			}
		}
	}

	// Fall back to global if no namespace-local config found
	if sourceConfig == nil {
		if c.Dynamic.Global != nil {
			sourceConfig = c.Dynamic.Global.ScaleToZeroConfig
		}
	}

	// Return a copy to prevent external modifications
	if sourceConfig == nil {
		return make(ScaleToZeroConfigData)
	}
	result := make(ScaleToZeroConfigData, len(sourceConfig))
	for k, v := range sourceConfig {
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

// UpdateSaturationConfig updates the global saturation scaling configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
// For namespace-local updates, use UpdateSaturationConfigForNamespace instead.
func (c *Config) UpdateSaturationConfig(config map[string]interfaces.SaturationScalingConfig) {
	c.UpdateSaturationConfigForNamespace("", config)
}

// UpdateSaturationConfigForNamespace updates the saturation scaling configuration for the given namespace.
// If namespace is empty, updates global config.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateSaturationConfigForNamespace(namespace string, config map[string]interfaces.SaturationScalingConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Make a copy to prevent external modifications
	newConfig := make(map[string]interfaces.SaturationScalingConfig, len(config))
	for k, v := range config {
		newConfig[k] = v
	}

	var oldCount int
	if namespace == "" {
		// Update global
		if c.Dynamic.Global == nil {
			c.Dynamic.Global = &NamespaceConfig{}
		}
		oldCount = len(c.Dynamic.Global.SaturationConfig)
		c.Dynamic.Global.SaturationConfig = newConfig
		newCount := len(c.Dynamic.Global.SaturationConfig)
		if oldCount != newCount {
			ctrl.Log.Info("Updated global saturation config", "oldEntries", oldCount, "newEntries", newCount)
		}
	} else {
		// Update namespace-local
		if c.Dynamic.NamespaceConfigs == nil {
			c.Dynamic.NamespaceConfigs = make(map[string]*NamespaceConfig)
		}
		if c.Dynamic.NamespaceConfigs[namespace] == nil {
			c.Dynamic.NamespaceConfigs[namespace] = &NamespaceConfig{}
		}
		oldCount = len(c.Dynamic.NamespaceConfigs[namespace].SaturationConfig)
		c.Dynamic.NamespaceConfigs[namespace].SaturationConfig = newConfig
		newCount := len(c.Dynamic.NamespaceConfigs[namespace].SaturationConfig)
		if oldCount != newCount {
			ctrl.Log.Info("Updated namespace-local saturation config", "namespace", namespace, "oldEntries", oldCount, "newEntries", newCount)
		}
	}
}

// UpdateScaleToZeroConfig updates the global scale-to-zero configuration.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
// For namespace-local updates, use UpdateScaleToZeroConfigForNamespace instead.
func (c *Config) UpdateScaleToZeroConfig(config ScaleToZeroConfigData) {
	c.UpdateScaleToZeroConfigForNamespace("", config)
}

// UpdateScaleToZeroConfigForNamespace updates the scale-to-zero configuration for the given namespace.
// If namespace is empty, updates global config.
// Thread-safe. Takes a copy of the provided map to prevent external modifications.
func (c *Config) UpdateScaleToZeroConfigForNamespace(namespace string, config ScaleToZeroConfigData) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Make a copy to prevent external modifications
	newConfig := make(ScaleToZeroConfigData, len(config))
	for k, v := range config {
		newConfig[k] = v
	}

	var oldCount int
	if namespace == "" {
		// Update global
		if c.Dynamic.Global == nil {
			c.Dynamic.Global = &NamespaceConfig{}
		}
		oldCount = len(c.Dynamic.Global.ScaleToZeroConfig)
		c.Dynamic.Global.ScaleToZeroConfig = newConfig
		newCount := len(c.Dynamic.Global.ScaleToZeroConfig)
		if oldCount != newCount {
			ctrl.Log.Info("Updated global scale-to-zero config", "oldModels", oldCount, "newModels", newCount)
		}
	} else {
		// Update namespace-local
		if c.Dynamic.NamespaceConfigs == nil {
			c.Dynamic.NamespaceConfigs = make(map[string]*NamespaceConfig)
		}
		if c.Dynamic.NamespaceConfigs[namespace] == nil {
			c.Dynamic.NamespaceConfigs[namespace] = &NamespaceConfig{}
		}
		oldCount = len(c.Dynamic.NamespaceConfigs[namespace].ScaleToZeroConfig)
		c.Dynamic.NamespaceConfigs[namespace].ScaleToZeroConfig = newConfig
		newCount := len(c.Dynamic.NamespaceConfigs[namespace].ScaleToZeroConfig)
		if oldCount != newCount {
			ctrl.Log.Info("Updated namespace-local scale-to-zero config", "namespace", namespace, "oldModels", oldCount, "newModels", newCount)
		}
	}
}

// RemoveNamespaceConfig removes the namespace-local configuration for the given namespace.
// This is called when a namespace-local ConfigMap is deleted, allowing fallback to global config.
// Thread-safe.
func (c *Config) RemoveNamespaceConfig(namespace string) {
	if namespace == "" {
		return // Don't remove global config
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Dynamic.NamespaceConfigs != nil {
		if _, exists := c.Dynamic.NamespaceConfigs[namespace]; exists {
			delete(c.Dynamic.NamespaceConfigs, namespace)
			ctrl.Log.Info("Removed namespace-local config", "namespace", namespace)
		}
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
			Global: &NamespaceConfig{
				SaturationConfig:  make(map[string]interfaces.SaturationScalingConfig),
				ScaleToZeroConfig: make(ScaleToZeroConfigData),
			},
			NamespaceConfigs: make(map[string]*NamespaceConfig),
		},
	}
}
