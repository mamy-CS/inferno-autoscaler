package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

// MemoryCache is an in-memory thread-safe cache implementation
type MemoryCache struct {
	// cache stores the cached metrics
	cache sync.Map // map[CacheKey]*CachedMetrics

	// defaultTTL is the default time-to-live for cache entries
	defaultTTL time.Duration

	// maxSize is the maximum number of entries (0 = unlimited)
	maxSize int

	// cleanupInterval is how often to run cleanup of expired entries
	cleanupInterval time.Duration

	// stopCleanup is used to stop the background cleanup goroutine
	stopCleanup context.CancelFunc
	cleanupCtx  context.Context
	cleanupOnce sync.Once
}

// NewMemoryCache creates a new in-memory cache
func NewMemoryCache(defaultTTL time.Duration, maxSize int, cleanupInterval time.Duration) *MemoryCache {
	ctx, cancel := context.WithCancel(context.Background())
	c := &MemoryCache{
		defaultTTL:      defaultTTL,
		maxSize:         maxSize,
		cleanupInterval: cleanupInterval,
		stopCleanup:     cancel,
		cleanupCtx:      ctx,
	}

	// Start background cleanup if interval is set
	if cleanupInterval > 0 {
		go c.startCleanup()
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
	// Use default TTL if not specified
	if ttl == 0 {
		ttl = mc.defaultTTL
	}

	// Check max size limit
	if mc.maxSize > 0 {
		if mc.Size() >= mc.maxSize {
			// Evict oldest entry (simple strategy: remove first expired, or random)
			// For now, we'll just allow it to grow (can improve later)
			logger.Log.Debugf("Cache at max size (%d), allowing growth", mc.maxSize)
		}
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

// InvalidateForModel removes all cache entries for a specific model
func (mc *MemoryCache) InvalidateForModel(modelID, namespace string) {
	prefix := fmt.Sprintf("%s/%s/", modelID, namespace)
	mc.cache.Range(func(key, value interface{}) bool {
		cacheKey := key.(CacheKey)
		keyStr := string(cacheKey)
		// Check if key starts with modelID/namespace
		if len(keyStr) >= len(prefix) && keyStr[:len(prefix)] == prefix {
			mc.cache.Delete(key)
		}
		return true
	})
}

// InvalidateForVariant removes all cache entries for a specific variant
func (mc *MemoryCache) InvalidateForVariant(modelID, namespace, variantName string) {
	prefix := fmt.Sprintf("%s/%s/%s/", modelID, namespace, variantName)
	mc.cache.Range(func(key, value interface{}) bool {
		cacheKey := key.(CacheKey)
		keyStr := string(cacheKey)
		// Check if key matches modelID/namespace/variantName
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
func (mc *MemoryCache) startCleanup() {
	ticker := time.NewTicker(mc.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mc.cleanupCtx.Done():
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

	if expiredCount > 0 {
		logger.Log.Debugf("Cache cleanup: removed %d expired entries", expiredCount)
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
