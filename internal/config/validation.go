package config

import (
	"fmt"
	"strings"
)

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

// ImmutableParameterChange represents a detected attempt to change an immutable parameter.
type ImmutableParameterChange struct {
	Key       string
	OldValue  string
	NewValue  string
	Parameter string // Human-readable parameter name
}

// DetectImmutableParameterChanges detects if a ConfigMap is attempting to change
// immutable parameters that require a controller restart.
//
// Immutable parameters (require restart):
// - PROMETHEUS_BASE_URL (connection endpoint)
// - METRICS_ADDR (infrastructure)
// - PROBE_ADDR (infrastructure)
// - LEADER_ELECTION_ID (coordination)
// - TLS certificate paths (security-sensitive)
//
// Returns:
// - A list of detected immutable parameter changes
// - An error if any immutable parameters are being changed
//
// This function should be called by the ConfigMap handler before applying updates
// to detect and reject attempts to change immutable parameters at runtime.
func DetectImmutableParameterChanges(cfg *Config, configMapData map[string]string) ([]ImmutableParameterChange, error) {
	var changes []ImmutableParameterChange

	// Check PROMETHEUS_BASE_URL
	if newURL, ok := configMapData["PROMETHEUS_BASE_URL"]; ok {
		currentURL := ""
		if cfg.Static.Prometheus != nil {
			currentURL = cfg.Static.Prometheus.BaseURL
		}
		if newURL != currentURL {
			changes = append(changes, ImmutableParameterChange{
				Key:       "PROMETHEUS_BASE_URL",
				OldValue:  currentURL,
				NewValue:  newURL,
				Parameter: "Prometheus BaseURL",
			})
		}
	}

	// Check METRICS_ADDR
	if newAddr, ok := configMapData["METRICS_ADDR"]; ok {
		currentAddr := cfg.Static.MetricsAddr
		if newAddr != currentAddr {
			changes = append(changes, ImmutableParameterChange{
				Key:       "METRICS_ADDR",
				OldValue:  currentAddr,
				NewValue:  newAddr,
				Parameter: "Metrics bind address",
			})
		}
	}

	// Check PROBE_ADDR
	if newAddr, ok := configMapData["PROBE_ADDR"]; ok {
		currentAddr := cfg.Static.ProbeAddr
		if newAddr != currentAddr {
			changes = append(changes, ImmutableParameterChange{
				Key:       "PROBE_ADDR",
				OldValue:  currentAddr,
				NewValue:  newAddr,
				Parameter: "Health probe bind address",
			})
		}
	}

	// Check LEADER_ELECTION_ID
	if newID, ok := configMapData["LEADER_ELECTION_ID"]; ok {
		currentID := cfg.Static.LeaderElectionID
		if newID != currentID {
			changes = append(changes, ImmutableParameterChange{
				Key:       "LEADER_ELECTION_ID",
				OldValue:  currentID,
				NewValue:  newID,
				Parameter: "Leader election ID",
			})
		}
	}

	// Check TLS certificate paths (if they exist in ConfigMap)
	// Note: These are typically set via CLI flags, but we check for completeness
	tlsKeys := []struct {
		key       string
		current   string
		paramName string
	}{
		{"WEBHOOK_CERT_PATH", cfg.Static.WebhookCertPath, "Webhook certificate path"},
		{"WEBHOOK_CERT_NAME", cfg.Static.WebhookCertName, "Webhook certificate name"},
		{"WEBHOOK_CERT_KEY", cfg.Static.WebhookCertKey, "Webhook certificate key"},
		{"METRICS_CERT_PATH", cfg.Static.MetricsCertPath, "Metrics certificate path"},
		{"METRICS_CERT_NAME", cfg.Static.MetricsCertName, "Metrics certificate name"},
		{"METRICS_CERT_KEY", cfg.Static.MetricsCertKey, "Metrics certificate key"},
	}

	for _, tlsKey := range tlsKeys {
		if newValue, ok := configMapData[tlsKey.key]; ok {
			if newValue != tlsKey.current {
				changes = append(changes, ImmutableParameterChange{
					Key:       tlsKey.key,
					OldValue:  tlsKey.current,
					NewValue:  newValue,
					Parameter: tlsKey.paramName,
				})
			}
		}
	}

	// If any immutable changes detected, return error
	if len(changes) > 0 {
		var changeList []string
		for _, change := range changes {
			changeList = append(changeList, fmt.Sprintf("%s (old: %q, new: %q)", change.Parameter, change.OldValue, change.NewValue))
		}
		return changes, fmt.Errorf("attempted to change immutable parameters that require controller restart: %s. Please restart the controller to apply these changes", strings.Join(changeList, "; "))
	}

	return nil, nil
}
