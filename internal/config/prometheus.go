package config

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	collectorconfig "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

const (
	// ConfigMapName is the name of the ConfigMap containing autoscaler configuration
	ConfigMapName = "workload-variant-autoscaler-variantautoscaling-config"
)

func getNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "workload-variant-autoscaler-system"
}

// GetPrometheusConfig retrieves Prometheus configuration from environment variables or ConfigMap
func GetPrometheusConfig(ctx context.Context, k8sClient client.Client) (*interfaces.PrometheusConfig, error) {
	// Try environment variables first
	config, err := GetPrometheusConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to get config from environment: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// Try ConfigMap second
	config, err = GetPrometheusConfigFromConfigMap(ctx, k8sClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get config from ConfigMap: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// No configuration found
	ctrl.Log.Info("No Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
	return nil, fmt.Errorf("no Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
}

// GetPrometheusConfigFromEnv retrieves Prometheus configuration from environment variables
func GetPrometheusConfigFromEnv() (*interfaces.PrometheusConfig, error) {
	promAddr := os.Getenv("PROMETHEUS_BASE_URL")
	if promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	ctrl.Log.Info("Using Prometheus configuration from environment variables", "address", promAddr)
	return ParsePrometheusConfigFromEnv(), nil
}

// GetPrometheusConfigFromConfigMap retrieves Prometheus configuration from ConfigMap
func GetPrometheusConfigFromConfigMap(ctx context.Context, k8sClient client.Client) (*interfaces.PrometheusConfig, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, k8sClient, ConfigMapName, getNamespace(), &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap for Prometheus config: %w", err)
	}

	promAddr, exists := cm.Data["PROMETHEUS_BASE_URL"]
	if !exists || promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	ctrl.Log.Info("Using Prometheus configuration from ConfigMap", "address", promAddr)

	config := &interfaces.PrometheusConfig{
		BaseURL: promAddr,
	}

	// Parse TLS configuration from ConfigMap (TLS is always enabled for HTTPS-only support)
	config.InsecureSkipVerify = GetConfigValue(cm.Data, "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "") == "true"
	config.CACertPath = GetConfigValue(cm.Data, "PROMETHEUS_CA_CERT_PATH", "")
	config.ClientCertPath = GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_CERT_PATH", "")
	config.ClientKeyPath = GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_KEY_PATH", "")
	config.ServerName = GetConfigValue(cm.Data, "PROMETHEUS_SERVER_NAME", "")

	// Add bearer token if provided
	if bearerToken, exists := cm.Data["PROMETHEUS_BEARER_TOKEN"]; exists && bearerToken != "" {
		config.BearerToken = bearerToken
	}

	return config, nil
}

// ReadPrometheusCacheConfig reads Prometheus collector cache configuration from the ConfigMap
func ReadPrometheusCacheConfig(ctx context.Context, k8sClient client.Client) (*collectorconfig.CacheConfig, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, k8sClient, ConfigMapName, getNamespace(), &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap for Prometheus cache config: %w", err)
	}

	// Initialize with defaults including freshness thresholds
	defaultThresholds := collectorconfig.DefaultFreshnessThresholds()
	config := &collectorconfig.CacheConfig{
		Enabled:             true, // default
		TTL:                 30 * time.Second,
		CleanupInterval:     1 * time.Minute,
		FetchInterval:       30 * time.Second, // default fetch interval
		FreshnessThresholds: defaultThresholds,
	}

	// PROMETHEUS_METRICS_CACHE_ENABLED (default: true)
	config.Enabled = ParseBoolFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_ENABLED", true)

	// PROMETHEUS_METRICS_CACHE_TTL (default: 30s)
	config.TTL = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_TTL", 30*time.Second)

	// PROMETHEUS_METRICS_CACHE_CLEANUP_INTERVAL (default: 1m)
	config.CleanupInterval = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_CLEANUP_INTERVAL", 1*time.Minute)

	// PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL (default: 30s, 0 = disable background fetching)
	config.FetchInterval = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_FETCH_INTERVAL", 30*time.Second)

	// Freshness thresholds
	// PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD (default: 1m)
	config.FreshnessThresholds.FreshThreshold = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_FRESH_THRESHOLD", 1*time.Minute)

	// PROMETHEUS_METRICS_CACHE_STALE_THRESHOLD (default: 2m)
	config.FreshnessThresholds.StaleThreshold = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_STALE_THRESHOLD", 2*time.Minute)

	// PROMETHEUS_METRICS_CACHE_UNAVAILABLE_THRESHOLD (default: 5m)
	config.FreshnessThresholds.UnavailableThreshold = ParseDurationFromConfig(cm.Data, "PROMETHEUS_METRICS_CACHE_UNAVAILABLE_THRESHOLD", 5*time.Minute)

	return config, nil
}

// ParsePrometheusConfigFromEnv parses Prometheus configuration from environment variables.
// Supports both direct values and file paths for flexible deployment scenarios.
func ParsePrometheusConfigFromEnv() *interfaces.PrometheusConfig {
	config := &interfaces.PrometheusConfig{
		BaseURL: os.Getenv("PROMETHEUS_BASE_URL"),
	}

	// TLS is always enabled for HTTPS-only support
	config.InsecureSkipVerify = os.Getenv("PROMETHEUS_TLS_INSECURE_SKIP_VERIFY") == "true"
	config.CACertPath = os.Getenv("PROMETHEUS_CA_CERT_PATH")
	config.ClientCertPath = os.Getenv("PROMETHEUS_CLIENT_CERT_PATH")
	config.ClientKeyPath = os.Getenv("PROMETHEUS_CLIENT_KEY_PATH")
	config.ServerName = os.Getenv("PROMETHEUS_SERVER_NAME")

	// Support both direct bearer token and token path
	config.BearerToken = os.Getenv("PROMETHEUS_BEARER_TOKEN")
	config.TokenPath = os.Getenv("PROMETHEUS_TOKEN_PATH")

	return config
}
