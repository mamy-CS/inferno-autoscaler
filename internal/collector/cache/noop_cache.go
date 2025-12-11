package cache

import "time"

// NoOpCache is a no-op cache implementation used when caching is disabled
type NoOpCache struct{}

func (n *NoOpCache) Get(key CacheKey) (*CachedMetrics, bool) {
	return nil, false
}

func (n *NoOpCache) Set(key CacheKey, data interface{}, ttl time.Duration) {
	// No-op
}

func (n *NoOpCache) Invalidate(key CacheKey) {
	// No-op
}

func (n *NoOpCache) InvalidateForModel(modelID, namespace string) {
	// No-op
}

func (n *NoOpCache) InvalidateForVariant(modelID, namespace, variantName string) {
	// No-op
}

func (n *NoOpCache) Clear() {
	// No-op
}

func (n *NoOpCache) Size() int {
	return 0
}
