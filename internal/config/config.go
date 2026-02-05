package config

import (
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
)

// configCache holds typed caches for all configuration lookups.
// This avoids unsafe type assertions and provides better type safety.
type configCache struct {
	// saturationCache caches saturation config per namespace
	// Key: namespace (empty string for global)
	saturationCache map[string]map[string]interfaces.SaturationScalingConfig

	// scaleToZeroCache caches scale-to-zero config per namespace
	// Key: namespace (empty string for global)
	scaleToZeroCache map[string]ScaleToZeroConfigData

	// optimizationIntervalCache caches the optimization interval (single value, not namespace-keyed)
	optimizationIntervalCache *time.Duration

	// prometheusCacheConfigCache caches the Prometheus cache config (single value, not namespace-keyed)
	prometheusCacheConfigCache *CacheConfig
}

// Config is the unified configuration structure for the WVA controller.
// It contains both static (immutable after startup) and dynamic (runtime-updatable) configuration.
type Config struct {
	// Static configuration (immutable after startup)
	Static StaticConfig

	// Dynamic configuration (can be updated via ConfigMap changes)
	// Protected by mutex for thread-safe updates
	mu      sync.RWMutex
	Dynamic DynamicConfig

	// Cache for expensive namespace-aware config lookups
	// Always initialized (never nil) to avoid nil checks
	cache configCache
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
//
// NamespaceConfig is exported for use in DynamicConfig and testing. Production code should
// use the resolved getter methods (e.g., SaturationConfigForNamespace, ScaleToZeroConfigForNamespace)
// rather than accessing NamespaceConfig directly. These getters handle namespace-local resolution
// and thread-safe access automatically.
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

// OptimizationInterval returns the current optimization interval.
// Thread-safe. Results are cached to avoid repeated mutex contention.
func (c *Config) OptimizationInterval() time.Duration {
	// Check cache first (read lock)
	c.mu.RLock()
	if c.cache.optimizationIntervalCache != nil {
		interval := *c.cache.optimizationIntervalCache
		c.mu.RUnlock()
		return interval
	}
	c.mu.RUnlock()

	// Cache miss - resolve and cache (write lock)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine might have cached it)
	if c.cache.optimizationIntervalCache != nil {
		return *c.cache.optimizationIntervalCache
	}

	// Cache the value
	interval := c.Dynamic.OptimizationInterval
	c.cache.optimizationIntervalCache = &interval
	return interval
}

// SaturationConfig returns the current global saturation scaling configuration.
// Thread-safe. Returns a copy to prevent external modifications.
// For namespace-aware lookups, use SaturationConfigForNamespace instead.
func (c *Config) SaturationConfig() map[string]interfaces.SaturationScalingConfig {
	return c.SaturationConfigForNamespace("")
}

// resolveNamespaceConfig is a generic helper that resolves namespace-local or global configuration.
// It implements the common resolution logic: namespace-local > global.
// Must be called while holding at least a read lock.
// Returns the source config (not a copy) - callers are responsible for copying if needed.
func (c *Config) resolveNamespaceConfig(namespace string, getter func(*NamespaceConfig) interface{}) interface{} {
	// Check namespace-local first (if namespace is provided)
	if namespace != "" {
		if nsConfig, exists := c.Dynamic.NamespaceConfigs[namespace]; exists {
			if result := getter(nsConfig); result != nil {
				return result
			}
		}
	}

	// Fall back to global if no namespace-local config found
	if c.Dynamic.Global != nil {
		return getter(c.Dynamic.Global)
	}

	return nil
}

// resolveCached is a generic helper that implements the double-checked locking pattern
// for cached configuration lookups. It checks the cache, resolves if needed, copies the result,
// and caches it for future lookups.
// T is the return type (e.g., map[string]interfaces.SaturationScalingConfig or ScaleToZeroConfigData).
// Must be called without holding any locks.
// The resolver function should return the source config (may be nil/zero value).
// The copier function should handle nil/zero values and return an appropriate default.
func resolveCached[T any](
	cache *map[string]T,
	mu *sync.RWMutex,
	namespace string,
	resolver func() T,
	copier func(T) T,
) T {
	// Check cache first (read lock)
	mu.RLock()
	if *cache != nil {
		if cached, ok := (*cache)[namespace]; ok {
			// Cache hit - return cached copy
			mu.RUnlock()
			return copier(cached)
		}
	}
	mu.RUnlock()

	// Cache miss - resolve and cache (write lock)
	mu.Lock()
	defer mu.Unlock()

	// Initialize cache if nil (defensive programming)
	if *cache == nil {
		*cache = make(map[string]T)
	}

	// Double-check after acquiring write lock (another goroutine might have cached it)
	if cached, ok := (*cache)[namespace]; ok {
		return copier(cached)
	}

	// Resolve configuration (must be called while holding write lock for resolveNamespaceConfig)
	sourceConfig := resolver()

	// Create copy for return and cache (copier handles nil/zero values)
	result := copier(sourceConfig)

	// Cache the result
	(*cache)[namespace] = result

	return result
}

// SaturationConfigForNamespace returns the saturation scaling configuration for the given namespace.
// Resolution order: namespace-local > global
// Thread-safe. Returns a copy to prevent external modifications.
// If namespace is empty, returns global config.
// Results are cached to avoid repeated resolution and copying.
func (c *Config) SaturationConfigForNamespace(namespace string) map[string]interfaces.SaturationScalingConfig {
	return resolveCached(
		&c.cache.saturationCache,
		&c.mu,
		namespace,
		func() map[string]interfaces.SaturationScalingConfig {
			var sourceConfig map[string]interfaces.SaturationScalingConfig
			if resolved := c.resolveNamespaceConfig(namespace, func(ns *NamespaceConfig) interface{} {
				if len(ns.SaturationConfig) > 0 {
					return ns.SaturationConfig
				}
				return nil
			}); resolved != nil {
				sourceConfig = resolved.(map[string]interfaces.SaturationScalingConfig)
			}
			if sourceConfig == nil {
				return make(map[string]interfaces.SaturationScalingConfig)
			}
			return sourceConfig
		},
		copySaturationConfig,
	)
}

// copySaturationConfig creates a deep copy of the saturation config map.
func copySaturationConfig(src map[string]interfaces.SaturationScalingConfig) map[string]interfaces.SaturationScalingConfig {
	if src == nil {
		return make(map[string]interfaces.SaturationScalingConfig)
	}
	result := make(map[string]interfaces.SaturationScalingConfig, len(src))
	for k, v := range src {
		result[k] = v
	}
	return result
}

// ScaleToZeroConfig returns the current global scale-to-zero configuration.
// Thread-safe.
// For namespace-aware lookups, use ScaleToZeroConfigForNamespace instead.
func (c *Config) ScaleToZeroConfig() ScaleToZeroConfigData {
	return c.ScaleToZeroConfigForNamespace("")
}

// ScaleToZeroConfigForNamespace returns the scale-to-zero configuration for the given namespace.
// Resolution order: namespace-local > global
// Thread-safe. Returns a copy to prevent external modifications.
// If namespace is empty, returns global config.
// Results are cached to avoid repeated resolution and copying.
func (c *Config) ScaleToZeroConfigForNamespace(namespace string) ScaleToZeroConfigData {
	return resolveCached(
		&c.cache.scaleToZeroCache,
		&c.mu,
		namespace,
		func() ScaleToZeroConfigData {
			var sourceConfig ScaleToZeroConfigData
			if resolved := c.resolveNamespaceConfig(namespace, func(ns *NamespaceConfig) interface{} {
				if len(ns.ScaleToZeroConfig) > 0 {
					return ns.ScaleToZeroConfig
				}
				return nil
			}); resolved != nil {
				sourceConfig = resolved.(ScaleToZeroConfigData)
			}
			if sourceConfig == nil {
				return make(ScaleToZeroConfigData)
			}
			return sourceConfig
		},
		copyScaleToZeroConfig,
	)
}

// copyScaleToZeroConfig creates a deep copy of the scale-to-zero config map.
func copyScaleToZeroConfig(src ScaleToZeroConfigData) ScaleToZeroConfigData {
	if src == nil {
		return make(ScaleToZeroConfigData)
	}
	result := make(ScaleToZeroConfigData, len(src))
	for k, v := range src {
		result[k] = v
	}
	return result
}

// PrometheusCacheConfig returns the current Prometheus cache configuration.
// Thread-safe.
// PrometheusCacheConfig returns the current Prometheus cache configuration.
// Thread-safe. Returns a copy to prevent external modifications.
// Results are cached to avoid repeated mutex contention and copying.
func (c *Config) PrometheusCacheConfig() *CacheConfig {
	// Check cache first (read lock)
	c.mu.RLock()
	if c.cache.prometheusCacheConfigCache != nil {
		// Return a copy of the cached value
		cp := *c.cache.prometheusCacheConfigCache
		c.mu.RUnlock()
		return &cp
	}
	c.mu.RUnlock()

	// Cache miss - resolve and cache (write lock)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine might have cached it)
	if c.cache.prometheusCacheConfigCache != nil {
		cp := *c.cache.prometheusCacheConfigCache
		return &cp
	}

	// Resolve and cache
	if c.Dynamic.PrometheusCache == nil {
		// Cache nil to avoid repeated lookups
		c.cache.prometheusCacheConfigCache = nil
		return nil
	}

	// Create copy for cache and return
	cp := *c.Dynamic.PrometheusCache
	c.cache.prometheusCacheConfigCache = &cp
	return &cp
}

// UpdateDynamicConfig updates the entire dynamic configuration.
// Thread-safe. Should be called when ConfigMap changes are detected.
func (c *Config) UpdateDynamicConfig(dynamic DynamicConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Dynamic = dynamic
	// Invalidate all caches since the entire dynamic config changed
	c.invalidateAllCaches()
}

// invalidateAllCaches invalidates all cached configurations.
// Must be called while holding the write lock.
func (c *Config) invalidateAllCaches() {
	// Clear all cache entries by reassigning empty maps (more efficient than deleting keys)
	c.cache.saturationCache = make(map[string]map[string]interfaces.SaturationScalingConfig)
	c.cache.scaleToZeroCache = make(map[string]ScaleToZeroConfigData)
	// Clear single-value caches
	c.cache.optimizationIntervalCache = nil
	c.cache.prometheusCacheConfigCache = nil
}

// UpdateOptimizationInterval updates the optimization interval.
// Thread-safe. Invalidates the cache to ensure subsequent reads return the new value.
func (c *Config) UpdateOptimizationInterval(interval time.Duration) {
	c.mu.Lock()
	oldInterval := c.Dynamic.OptimizationInterval
	c.Dynamic.OptimizationInterval = interval
	// Invalidate cache
	c.cache.optimizationIntervalCache = nil
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

	// Invalidate cache for this namespace and global (global changes affect all namespaces)
	c.invalidateNamespaceCache(namespace, true)
}

// invalidateNamespaceCache invalidates cached configurations for the given namespace.
// If invalidateGlobal is true and namespace is not empty, also invalidates global cache.
// This is needed because namespace-local changes might affect resolution logic.
// Must be called while holding the write lock.
func (c *Config) invalidateNamespaceCache(namespace string, invalidateGlobal bool) {
	// Invalidate both caches for the namespace
	delete(c.cache.saturationCache, namespace)
	delete(c.cache.scaleToZeroCache, namespace)

	// Also invalidate global cache if requested (for namespace-local updates)
	if invalidateGlobal && namespace != "" {
		delete(c.cache.saturationCache, "")
		delete(c.cache.scaleToZeroCache, "")
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

	// Invalidate cache for this namespace and global (global changes affect all namespaces)
	c.invalidateNamespaceCache(namespace, true)
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
			// Invalidate cache for this namespace (will now fall back to global)
			// Don't invalidate global cache - we're just removing namespace-local override
			c.invalidateNamespaceCache(namespace, false)
		}
	}
}

// UpdatePrometheusCacheConfig updates the Prometheus cache configuration.
// Thread-safe.
// UpdatePrometheusCacheConfig updates the Prometheus cache configuration.
// Thread-safe. Invalidates the cache to ensure subsequent reads return the new value.
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
	// Invalidate cache
	c.cache.prometheusCacheConfigCache = nil
}

// DynamicConfig returns a copy of the current dynamic configuration.
// Thread-safe.
func (c *Config) DynamicConfig() DynamicConfig {
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
		cache: configCache{
			saturationCache:            make(map[string]map[string]interfaces.SaturationScalingConfig),
			scaleToZeroCache:           make(map[string]ScaleToZeroConfigData),
			optimizationIntervalCache:  nil,
			prometheusCacheConfigCache: nil,
		},
	}
}
