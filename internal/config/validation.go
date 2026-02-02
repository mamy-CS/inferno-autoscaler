package config

import "fmt"

// Validate performs validation on the loaded configuration.
// It returns an error if any required configuration is missing or invalid.
// This implements fail-fast behavior: the controller should not start with invalid configuration.
func Validate(cfg *Config) error {
	// Prometheus config is required
	if cfg.Static.Prometheus == nil {
		return fmt.Errorf("prometheus configuration is required")
	}
	if cfg.Static.Prometheus.BaseURL == "" {
		return fmt.Errorf("prometheus BaseURL is required")
	}

	// Optimization interval must be positive
	if cfg.Dynamic.OptimizationInterval <= 0 {
		return fmt.Errorf("optimization interval must be positive, got %v", cfg.Dynamic.OptimizationInterval)
	}

	// Scale-from-zero max concurrency must be positive
	if cfg.Static.ScaleFromZeroMaxConcurrency <= 0 {
		return fmt.Errorf("scale-from-zero max concurrency must be positive, got %d", cfg.Static.ScaleFromZeroMaxConcurrency)
	}

	return nil
}
