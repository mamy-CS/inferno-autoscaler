// Package pod provides the Pod scraping metrics source implementation.
//
// This file contains configuration types and defaults for PodScrapingSource.
package pod

import "time"

// PodScrapingSourceConfig contains configuration for pod scraping.
type PodScrapingSourceConfig struct {
	// InferencePool identification
	InferencePoolName      string
	InferencePoolNamespace string

	// Service discovery
	// The scale-from-zero engine should provide ServiceName explicitly.
	// If empty, falls back to pattern-based discovery: {poolName}-epp
	ServiceName string

	// Metrics endpoint (provided by client/engine)
	MetricsPort   int32  // provided by client
	MetricsPath   string // provided by client, default: "/metrics"
	MetricsScheme string // provided by client, default: "http"

	// Authentication
	MetricsReaderSecretName string // default: "inference-gateway-sa-metrics-reader-secret"
	MetricsReaderSecretKey  string // default: "token"
	BearerToken             string // optional: explicit token override

	// Scraping behavior
	ScrapeTimeout        time.Duration // default: 5s per pod
	MaxConcurrentScrapes int           // default: 10

	// Cache configuration
	DefaultTTL time.Duration // default: 30s
}

// DefaultPodScrapingSourceConfig returns sensible defaults.
func DefaultPodScrapingSourceConfig() PodScrapingSourceConfig {
	return PodScrapingSourceConfig{
		MetricsPath:             "/metrics",
		MetricsScheme:           "http",
		MetricsReaderSecretName: "inference-gateway-sa-metrics-reader-secret",
		MetricsReaderSecretKey:  "token",
		ScrapeTimeout:           5 * time.Second,
		MaxConcurrentScrapes:    10,
		DefaultTTL:              30 * time.Second,
	}
}
