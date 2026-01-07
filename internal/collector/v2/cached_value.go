// Package collector provides metrics collection functionality.
//
// This file defines the CachedValue type for caching metric values.
package collector

import (
	"sort"
	"strings"
	"time"
)

// CacheKey uniquely identifies a cached query result.
// It combines the query name with the parameter values used.
type CacheKey string

// BuildCacheKey constructs a cache key from query name and parameters.
// The key format is: "queryName:key1=value1,key2=value2" with sorted keys.
func BuildCacheKey(queryName string, params map[string]string) CacheKey {
	if len(params) == 0 {
		return CacheKey(queryName)
	}

	// Sort keys for deterministic key generation
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build key=value pairs
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+params[k])
	}

	return CacheKey(queryName + ":" + strings.Join(pairs, ","))
}

// QueryName extracts the query name from a cache key.
func (k CacheKey) QueryName() string {
	s := string(k)
	if before, _, ok := strings.Cut(s, ":"); ok {
		return before
	}
	return s
}

// CachedValue wraps a MetricResult with cache metadata.
type CachedValue struct {
	Result   *MetricResult
	CachedAt time.Time
	TTL      time.Duration
	// Params stores the parameters used to generate this cached value.
	Params map[string]string
}

// IsExpired returns true if the cached value has exceeded its TTL.
func (c *CachedValue) IsExpired() bool {
	if c == nil {
		return true
	}
	return time.Since(c.CachedAt) > c.TTL
}

// Age returns how long ago the value was cached.
func (c *CachedValue) Age() time.Duration {
	if c == nil {
		return 0
	}
	return time.Since(c.CachedAt)
}

// IsFresh returns true if the cached value is within the freshness threshold.
func (c *CachedValue) IsFresh(threshold time.Duration) bool {
	if c == nil {
		return false
	}
	return time.Since(c.CachedAt) <= threshold
}
