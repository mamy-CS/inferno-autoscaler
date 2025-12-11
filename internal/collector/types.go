package collector

import "time"

// CacheConfig holds configuration for the metrics cache.
// This is part of the public API - used by controllers and factory.
//
// NOTE: This type is duplicated in each collector plugin package (e.g., prometheus.CacheConfig)
// to avoid import cycles. The factory (factory.go) converts between these types at the boundary.
// When adding a new collector plugin (e.g., EPP), it should define its own CacheConfig type
// following the same structure. The factory will handle conversion from this public API type.
type CacheConfig struct {
	Enabled         bool
	TTL             time.Duration
	MaxSize         int
	CleanupInterval time.Duration
	// FetchInterval is how often to fetch metrics in background (0 = disable background fetching)
	FetchInterval time.Duration
	// FreshnessThresholds define when metrics are considered fresh/stale/unavailable
	FreshnessThresholds FreshnessThresholds
}

// FreshnessThresholds defines when metrics are considered fresh, stale, or unavailable.
// This is part of the public API - used by controllers.
//
// NOTE: This type is duplicated in each collector plugin package (e.g., prometheus.FreshnessThresholds)
// to avoid import cycles. The factory (factory.go) converts between these types at the boundary.
// When adding a new collector plugin (e.g., EPP), it should define its own FreshnessThresholds type
// following the same structure.
type FreshnessThresholds struct {
	FreshThreshold       time.Duration // Metrics are fresh if age < this (default: 1 minute)
	StaleThreshold       time.Duration // Metrics are stale if age >= this but < unavailable (default: 2 minutes)
	UnavailableThreshold time.Duration // Metrics are unavailable if age >= this (default: 5 minutes)
}

// DefaultFreshnessThresholds returns default freshness thresholds
func DefaultFreshnessThresholds() FreshnessThresholds {
	return FreshnessThresholds{
		FreshThreshold:       1 * time.Minute,
		StaleThreshold:       2 * time.Minute,
		UnavailableThreshold: 5 * time.Minute,
	}
}
