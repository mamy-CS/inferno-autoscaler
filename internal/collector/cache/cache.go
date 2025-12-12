package cache

import (
	"time"
)

// CacheKey represents a unique key for cached metrics.
// The cache is generic and does not enforce any specific key format.
// Each collector implementation is responsible for constructing keys in its own format.
type CacheKey string

// CachedMetrics holds cached metric data with metadata
type CachedMetrics struct {
	// Data is the actual cached metric value
	// For Allocation: stores the Allocation struct (used by Model-based optimizer for proactive scaling)
	// For ReplicaMetrics: stores the slice of ReplicaMetrics (Used by the saturation analyzer for reactive scaling)
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

	// InvalidateByPrefix removes all cache entries whose keys start with the given prefix.
	// This allows collectors to invalidate groups of related cache entries without
	// the cache needing to know about domain-specific concepts (e.g., models, variants).
	InvalidateByPrefix(prefix string)

	// Clear removes all entries from the cache
	Clear()

	// Size returns the number of entries in the cache
	Size() int
}
