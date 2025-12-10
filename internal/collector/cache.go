package collector

import (
	"fmt"
	"time"
)

// CacheKey represents a unique key for cached metrics
// Format: {modelID}/{namespace}/{variantName}/{metricType}
type CacheKey string

// NewCacheKey creates a cache key from components
func NewCacheKey(modelID, namespace, variantName, metricType string) CacheKey {
	return CacheKey(fmt.Sprintf("%s/%s/%s/%s", modelID, namespace, variantName, metricType))
}

// CachedMetrics holds cached metric data with metadata
type CachedMetrics struct {
	// Data is the actual cached metric value
	// For Allocation: stores the Allocation struct
	// For ReplicaMetrics: stores the slice of ReplicaMetrics
	Data interface{}

	// CollectedAt is when the data was collected
	CollectedAt time.Time

	// TTL is the time-to-live for this cache entry
	TTL time.Duration
}

// IsExpired checks if the cached metrics have expired
func (cm *CachedMetrics) IsExpired() bool {
	if cm == nil {
		return true
	}
	age := time.Since(cm.CollectedAt)
	return age > cm.TTL
}

// Age returns how old the cached data is
func (cm *CachedMetrics) Age() time.Duration {
	if cm == nil {
		return 0
	}
	return time.Since(cm.CollectedAt)
}

// MetricsCache defines the interface for caching metrics
// This is internal to the collector package - consumers don't see it
type MetricsCache interface {
	// Get retrieves cached metrics by key
	// Returns the cached metrics and true if found and not expired, false otherwise
	Get(key CacheKey) (*CachedMetrics, bool)

	// Set stores metrics in the cache with a TTL
	Set(key CacheKey, data interface{}, ttl time.Duration)

	// Invalidate removes a specific cache entry
	Invalidate(key CacheKey)

	// InvalidateForModel removes all cache entries for a specific model
	InvalidateForModel(modelID, namespace string)

	// InvalidateForVariant removes all cache entries for a specific variant
	InvalidateForVariant(modelID, namespace, variantName string)

	// Clear removes all cache entries
	Clear()

	// Size returns the number of entries in the cache
	Size() int
}
