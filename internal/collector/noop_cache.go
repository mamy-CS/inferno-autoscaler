package collector

import "time"

// noOpCache is a no-op cache implementation used when caching is disabled
type noOpCache struct{}

func (n *noOpCache) Get(key CacheKey) (*CachedMetrics, bool) {
	return nil, false
}

func (n *noOpCache) Set(key CacheKey, data interface{}, ttl time.Duration) {
	// No-op
}

func (n *noOpCache) Invalidate(key CacheKey) {
	// No-op
}

func (n *noOpCache) InvalidateForModel(modelID, namespace string) {
	// No-op
}

func (n *noOpCache) InvalidateForVariant(modelID, namespace, variantName string) {
	// No-op
}

func (n *noOpCache) Clear() {
	// No-op
}

func (n *noOpCache) Size() int {
	return 0
}
