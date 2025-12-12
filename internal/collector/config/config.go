package config

import "time"

// CacheConfig holds configuration for the metrics cache.
// This is the shared configuration type used by all collector plugins (Prometheus, EPP, etc.).
type CacheConfig struct {
	Enabled         bool
	TTL             time.Duration
	CleanupInterval time.Duration
	// FetchInterval is how often to fetch metrics in background (0 = disable background fetching)
	FetchInterval time.Duration
	// FreshnessThresholds define when metrics are considered fresh/stale/unavailable
	FreshnessThresholds FreshnessThresholds
}

// FreshnessThresholds defines when metrics are considered fresh, stale, or unavailable.
// This is the shared type used by all collector plugins.
type FreshnessThresholds struct {
	FreshThreshold       time.Duration // Metrics are fresh if age < this (default: 1 minute)
	StaleThreshold       time.Duration // Metrics are stale if age >= this but < unavailable (default: 2 minutes)
	UnavailableThreshold time.Duration // Metrics are unavailable if age >= this (default: 5 minutes)
}

// DetermineStatus determines the freshness status based on age.
// Returns "fresh", "stale", or "unavailable" based on the configured thresholds.
func (ft FreshnessThresholds) DetermineStatus(age time.Duration) string {
	if age < ft.FreshThreshold {
		return "fresh"
	} else if age < ft.UnavailableThreshold {
		return "stale"
	}
	return "unavailable"
}

// DefaultFreshnessThresholds returns default freshness thresholds
func DefaultFreshnessThresholds() FreshnessThresholds {
	return FreshnessThresholds{
		FreshThreshold:       1 * time.Minute,
		StaleThreshold:       2 * time.Minute,
		UnavailableThreshold: 5 * time.Minute,
	}
}
