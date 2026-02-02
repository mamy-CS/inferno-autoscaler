package config

import (
	"context"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

// StaticConfigFlags holds CLI flag values for static configuration.
// This is passed from main.go to Load() function.
type StaticConfigFlags struct {
	MetricsAddr          string
	ProbeAddr            string
	EnableLeaderElection bool
	LeaderElectionID     string
	LeaseDuration        time.Duration
	RenewDeadline        time.Duration
	RetryPeriod          time.Duration
	RestTimeout          time.Duration
	SecureMetrics        bool
	EnableHTTP2          bool
	WatchNamespace       string
	LoggerVerbosity      int
	WebhookCertPath      string
	WebhookCertName      string
	WebhookCertKey       string
	MetricsCertPath      string
	MetricsCertName      string
	MetricsCertKey       string
}

const (
	// Default optimization interval
	defaultOptimizationInterval = 60 * time.Second

	// Default saturation config map name
)

// Load loads and validates the unified configuration.
// Precedence: flags > env > ConfigMap > defaults
// Returns error if required configuration is missing or invalid (fail-fast).
func Load(ctx context.Context, flags StaticConfigFlags, k8sClient client.Client) (*Config, error) {
	cfg := &Config{}

	// 1. Load static config (flags > env > ConfigMap > defaults)
	if err := loadStaticConfig(ctx, &cfg.Static, flags, k8sClient); err != nil {
		return nil, fmt.Errorf("failed to load static config: %w", err)
	}

	// 2. Load initial dynamic config (ConfigMap > defaults)
	if err := loadDynamicConfig(ctx, &cfg.Dynamic, k8sClient); err != nil {
		return nil, fmt.Errorf("failed to load dynamic config: %w", err)
	}

	// 3. Validate required configuration (fail-fast)
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	ctrl.Log.Info("Configuration loaded successfully")
	return cfg, nil
}

// loadStaticConfig loads static configuration with precedence: flags > env > ConfigMap > defaults
func loadStaticConfig(ctx context.Context, static *StaticConfig, flags StaticConfigFlags, k8sClient client.Client) error {
	// Initialize with defaults
	*static = StaticConfig{
		MetricsAddr:          "0", // Disable by default
		ProbeAddr:            ":8081",
		EnableLeaderElection: false,
		LeaderElectionID:     "72dd1cf1.llm-d.ai",
		LeaseDuration:        60 * time.Second,
		RenewDeadline:        50 * time.Second,
		RetryPeriod:          10 * time.Second,
		RestTimeout:          60 * time.Second,
		SecureMetrics:        true,
		EnableHTTP2:          false,
		WatchNamespace:       "",
		LoggerVerbosity:      0,
		WebhookCertName:      "tls.crt",
		WebhookCertKey:       "tls.key",
		MetricsCertName:      "tls.crt",
		MetricsCertKey:       "tls.key",
	}

	// Load from ConfigMap (if available)
	var cmData map[string]string
	cm := &corev1.ConfigMap{}
	cmName := GetConfigMapName()
	cmNamespace := GetNamespace()
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, cmName, cmNamespace, cm); err == nil {
		cmData = cm.Data
		ctrl.Log.Info("Loaded ConfigMap for static config", "name", cmName, "namespace", cmNamespace)
	} else {
		ctrl.Log.Info("ConfigMap not found, using defaults and flags", "name", cmName, "namespace", cmNamespace, "error", err)
		cmData = make(map[string]string)
	}

	// Apply precedence: flags > env > ConfigMap > defaults
	// Infrastructure settings (CLI flags take precedence)
	static.MetricsAddr = getStringValue(flags.MetricsAddr, os.Getenv("METRICS_BIND_ADDRESS"), cmData["METRICS_BIND_ADDRESS"], static.MetricsAddr)
	static.ProbeAddr = getStringValue(flags.ProbeAddr, os.Getenv("HEALTH_PROBE_BIND_ADDRESS"), cmData["HEALTH_PROBE_BIND_ADDRESS"], static.ProbeAddr)
	static.EnableLeaderElection = getBoolValue(flags.EnableLeaderElection, parseBoolEnv("LEADER_ELECT"), ParseBoolFromConfig(cmData, "LEADER_ELECT", static.EnableLeaderElection))
	static.LeaderElectionID = getStringValue(flags.LeaderElectionID, os.Getenv("LEADER_ELECTION_ID"), cmData["LEADER_ELECTION_ID"], static.LeaderElectionID)
	static.LeaseDuration = getDurationValue(flags.LeaseDuration, parseDurationEnv("LEADER_ELECTION_LEASE_DURATION"), ParseDurationFromConfig(cmData, "LEADER_ELECTION_LEASE_DURATION", static.LeaseDuration))
	static.RenewDeadline = getDurationValue(flags.RenewDeadline, parseDurationEnv("LEADER_ELECTION_RENEW_DEADLINE"), ParseDurationFromConfig(cmData, "LEADER_ELECTION_RENEW_DEADLINE", static.RenewDeadline))
	static.RetryPeriod = getDurationValue(flags.RetryPeriod, parseDurationEnv("LEADER_ELECTION_RETRY_PERIOD"), ParseDurationFromConfig(cmData, "LEADER_ELECTION_RETRY_PERIOD", static.RetryPeriod))
	static.RestTimeout = getDurationValue(flags.RestTimeout, parseDurationEnv("REST_CLIENT_TIMEOUT"), ParseDurationFromConfig(cmData, "REST_CLIENT_TIMEOUT", static.RestTimeout))
	static.SecureMetrics = getBoolValue(flags.SecureMetrics, parseBoolEnv("METRICS_SECURE"), ParseBoolFromConfig(cmData, "METRICS_SECURE", static.SecureMetrics))
	static.EnableHTTP2 = getBoolValue(flags.EnableHTTP2, parseBoolEnv("ENABLE_HTTP2"), ParseBoolFromConfig(cmData, "ENABLE_HTTP2", static.EnableHTTP2))
	static.WatchNamespace = getStringValue(flags.WatchNamespace, os.Getenv("WATCH_NAMESPACE"), cmData["WATCH_NAMESPACE"], static.WatchNamespace)
	static.LoggerVerbosity = getIntValue(flags.LoggerVerbosity, parseIntEnv("V"), ParseIntFromConfig(cmData, "V", static.LoggerVerbosity, 0))
	static.WebhookCertPath = getStringValue(flags.WebhookCertPath, os.Getenv("WEBHOOK_CERT_PATH"), cmData["WEBHOOK_CERT_PATH"], static.WebhookCertPath)
	static.WebhookCertName = getStringValue(flags.WebhookCertName, os.Getenv("WEBHOOK_CERT_NAME"), cmData["WEBHOOK_CERT_NAME"], static.WebhookCertName)
	static.WebhookCertKey = getStringValue(flags.WebhookCertKey, os.Getenv("WEBHOOK_CERT_KEY"), cmData["WEBHOOK_CERT_KEY"], static.WebhookCertKey)
	static.MetricsCertPath = getStringValue(flags.MetricsCertPath, os.Getenv("METRICS_CERT_PATH"), cmData["METRICS_CERT_PATH"], static.MetricsCertPath)
	static.MetricsCertName = getStringValue(flags.MetricsCertName, os.Getenv("METRICS_CERT_NAME"), cmData["METRICS_CERT_NAME"], static.MetricsCertName)
	static.MetricsCertKey = getStringValue(flags.MetricsCertKey, os.Getenv("METRICS_CERT_KEY"), cmData["METRICS_CERT_KEY"], static.MetricsCertKey)

	// Load Prometheus config (required) - env > ConfigMap
	promConfig, err := GetPrometheusConfig(ctx, k8sClient)
	if err != nil {
		return fmt.Errorf("failed to load Prometheus config: %w", err)
	}
	if promConfig == nil {
		return fmt.Errorf("prometheus configuration is required but not found. set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
	}
	static.Prometheus = promConfig

	// Load feature flags from ConfigMap
	static.ScaleToZeroEnabled = ParseBoolFromConfig(cmData, "WVA_SCALE_TO_ZERO", false)
	static.LimitedModeEnabled = ParseBoolFromConfig(cmData, "WVA_LIMITED_MODE", false)
	static.ScaleFromZeroMaxConcurrency = ParseIntFromConfig(cmData, "SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY", 10, 1)

	return nil
}

// loadDynamicConfig loads dynamic configuration from ConfigMap with defaults as fallback
func loadDynamicConfig(ctx context.Context, dynamic *DynamicConfig, k8sClient client.Client) error {
	// Initialize with defaults
	*dynamic = DynamicConfig{
		OptimizationInterval: defaultOptimizationInterval,
		SaturationConfig:     make(map[string]interfaces.SaturationScalingConfig),
		ScaleToZeroConfig:    make(ScaleToZeroConfigData),
		PrometheusCache:      defaultPrometheusCacheConfig(),
	}

	// Load main ConfigMap
	cm := &corev1.ConfigMap{}
	cmName := GetConfigMapName()
	cmNamespace := GetNamespace()
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, cmName, cmNamespace, cm); err != nil {
		ctrl.Log.Info("ConfigMap not found for dynamic config, using defaults", "name", cmName, "namespace", cmNamespace, "error", err)
		// Continue to load other ConfigMaps even if main ConfigMap is not found
	}

	// Load optimization interval
	if intervalStr, ok := cm.Data["GLOBAL_OPT_INTERVAL"]; ok {
		if interval, err := time.ParseDuration(intervalStr); err == nil {
			dynamic.OptimizationInterval = interval
		} else {
			ctrl.Log.Info("Invalid GLOBAL_OPT_INTERVAL, using default", "value", intervalStr, "error", err)
		}
	}

	// Load Prometheus cache config
	if cacheConfig, err := ReadPrometheusCacheConfig(ctx, k8sClient); err == nil && cacheConfig != nil {
		dynamic.PrometheusCache = cacheConfig
	}

	// Load scale-to-zero config
	scaleToZeroCM := &corev1.ConfigMap{}
	scaleToZeroCMName := DefaultScaleToZeroConfigMapName
	// Use GetConfigMapWithBackoff which handles both fake clients (in tests) and real clients (in production)
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, scaleToZeroCMName, cmNamespace, scaleToZeroCM); err == nil {
		dynamic.ScaleToZeroConfig = ParseScaleToZeroConfigMap(scaleToZeroCM.Data)
	}

	// Load saturation scaling config
	saturationCM := &corev1.ConfigMap{}
	saturationCMName := GetSaturationConfigMapName()
	// Use GetConfigMapWithBackoff which handles both fake clients (in tests) and real clients (in production)
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, saturationCMName, cmNamespace, saturationCM); err == nil {
		configs := make(map[string]interfaces.SaturationScalingConfig)
		for key, yamlStr := range saturationCM.Data {
			var satConfig interfaces.SaturationScalingConfig
			if err := yaml.Unmarshal([]byte(yamlStr), &satConfig); err != nil {
				ctrl.Log.Info("Failed to parse saturation scaling config entry", "key", key, "error", err)
				continue
			}
			// Validate
			if err := satConfig.Validate(); err != nil {
				ctrl.Log.Info("Invalid saturation scaling config entry", "key", key, "error", err)
				continue
			}
			configs[key] = satConfig
		}
		if len(configs) > 0 {
			dynamic.SaturationConfig = configs
			ctrl.Log.Info("Loaded saturation scaling config", "entries", len(configs))
		}
	}

	return nil
}

// validateConfig validates required configuration fields

// Helper functions for precedence resolution

func getStringValue(flag, env, cm, def string) string {
	if flag != "" {
		return flag
	}
	if env != "" {
		return env
	}
	if cm != "" {
		return cm
	}
	return def
}

func getBoolValue(flag bool, env *bool, cm bool) bool {
	// Precedence: flag > env > cm
	// If flag is explicitly set (non-zero), use it
	// Note: For bool, we can't distinguish "unset" from "false", so we check env first
	if env != nil {
		return *env
	}
	// If flag is set (true), it takes precedence over cm
	if flag {
		return true
	}
	// Otherwise use cm value
	return cm
}

func getDurationValue(flag time.Duration, env *time.Duration, cm time.Duration) time.Duration {
	if env != nil {
		return *env
	}
	if cm > 0 {
		return cm
	}
	return flag
}

func getIntValue(flag int, env *int, cm int) int {
	if env != nil {
		return *env
	}
	if cm != 0 {
		return cm
	}
	return flag
}

// Helper functions for parsing env vars

func parseBoolEnv(key string) *bool {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	result := val == "true" || val == "1" || val == "yes"
	return &result
}

func parseDurationEnv(key string) *time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	if d, err := time.ParseDuration(val); err == nil {
		return &d
	}
	return nil
}

func parseIntEnv(key string) *int {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	var result int
	if _, err := fmt.Sscanf(val, "%d", &result); err == nil {
		return &result
	}
	return nil
}

// defaultPrometheusCacheConfig returns default Prometheus cache configuration
func defaultPrometheusCacheConfig() *CacheConfig {
	return &CacheConfig{
		Enabled:             true,
		TTL:                 30 * time.Second,
		CleanupInterval:     1 * time.Minute,
		FetchInterval:       30 * time.Second,
		FreshnessThresholds: DefaultFreshnessThresholds(),
	}
}
