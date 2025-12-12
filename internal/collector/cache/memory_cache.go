package cache

import (
	"context"
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

// MemoryCache is an in-memory thread-safe cache implementation
type MemoryCache struct {
	// cache stores the cached metrics
	cache sync.Map // map[CacheKey]*CachedMetrics

	// configuredTTL is the time-to-live for cache entries configured at creation time.
	// This value is used when Set() is called with ttl=0 (meaning "use configured TTL").
	// It comes from CacheConfig.TTL but is stored here to avoid keeping a reference to the config.
	configuredTTL time.Duration

	// cleanupInterval is how often to run cleanup of expired entries
	cleanupInterval time.Duration

	// stopCleanup is used to stop the background cleanup goroutine
	stopCleanup context.CancelFunc
	cleanupOnce sync.Once
}

// NewMemoryCache creates a new in-memory cache
func NewMemoryCache(configuredTTL time.Duration, cleanupInterval time.Duration) *MemoryCache {
	ctx, cancel := context.WithCancel(context.Background())
	c := &MemoryCache{
		configuredTTL:   configuredTTL,
		cleanupInterval: cleanupInterval,
		stopCleanup:     cancel,
	}

	// Start background cleanup if interval is set
	if cleanupInterval > 0 {
		go c.startCleanup(ctx)
	}

	return c
}

// Get retrieves cached metrics by key
func (mc *MemoryCache) Get(key CacheKey) (*CachedMetrics, bool) {
	value, ok := mc.cache.Load(key)
	if !ok {
		return nil, false
	}

	cached, ok := value.(*CachedMetrics)
	if !ok {
		return nil, false
	}

	// Check if expired
	if cached.IsExpired() {
		// Remove expired entry
		mc.cache.Delete(key)
		return nil, false
	}

	return cached, true
}

// Set stores metrics in the cache with a TTL
func (mc *MemoryCache) Set(key CacheKey, data interface{}, ttl time.Duration) {
	// Use configured TTL if not specified (ttl=0 means "use configured value")
	if ttl == 0 {
		ttl = mc.configuredTTL
	}

	cached := &CachedMetrics{
		Data:        data,
		CollectedAt: time.Now(),
		TTL:         ttl,
	}

	mc.cache.Store(key, cached)
}

// Invalidate removes a specific cache entry
func (mc *MemoryCache) Invalidate(key CacheKey) {
	mc.cache.Delete(key)
}

// InvalidateByPrefix removes all cache entries whose keys start with the given prefix
func (mc *MemoryCache) InvalidateByPrefix(prefix string) {
	mc.cache.Range(func(key, value interface{}) bool {
		cacheKey, ok := key.(CacheKey)
		if !ok {
			// Invalid key type, delete it
			mc.cache.Delete(key)
			return true
		}
		keyStr := string(cacheKey)
		// Check if key starts with prefix
		if len(keyStr) >= len(prefix) && keyStr[:len(prefix)] == prefix {
			mc.cache.Delete(key)
		}
		return true
	})
}

// Clear removes all cache entries
func (mc *MemoryCache) Clear() {
	mc.cache.Range(func(key, value interface{}) bool {
		mc.cache.Delete(key)
		return true
	})
}

// Size returns the number of entries in the cache
func (mc *MemoryCache) Size() int {
	count := 0
	mc.cache.Range(func(key, value interface{}) bool {
		count++
		return true
	})
	return count
}

// startCleanup runs a background goroutine to periodically clean up expired entries
func (mc *MemoryCache) startCleanup(ctx context.Context) {
	ticker := time.NewTicker(mc.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mc.cleanupExpired()
		}
	}
}

// cleanupExpired removes all expired entries from the cache
func (mc *MemoryCache) cleanupExpired() {
	expiredCount := 0
	mc.cache.Range(func(key, value interface{}) bool {
		cached, ok := value.(*CachedMetrics)
		if !ok {
			mc.cache.Delete(key)
			expiredCount++
			return true
		}

		if cached.IsExpired() {
			mc.cache.Delete(key)
			expiredCount++
		}
		return true
	})

	if expiredCount > 0 && logger.Log != nil {
		logger.Log.Debugw("Cache cleanup: removed expired entries", "count", expiredCount)
	}
}

// Stop stops the background cleanup goroutine
func (mc *MemoryCache) Stop() {
	mc.cleanupOnce.Do(func() {
		if mc.stopCleanup != nil {
			mc.stopCleanup()
		}
	})
}
