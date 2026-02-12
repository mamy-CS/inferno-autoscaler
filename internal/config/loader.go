package config

import (
	"bytes"
	"context"
	"fmt"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

const (
	// defaultOptimizationInterval is the default optimization interval used when
	// GLOBAL_OPT_INTERVAL is not specified in the ConfigMap.
	defaultOptimizationInterval = 60 * time.Second
)

// flagBindings maps viper keys (= env var names = ConfigMap keys) to pflag names.
var flagBindings = map[string]string{
	"METRICS_BIND_ADDRESS":               "metrics-bind-address",
	"HEALTH_PROBE_BIND_ADDRESS":          "health-probe-bind-address",
	"LEADER_ELECT":                       "leader-elect",
	"LEADER_ELECTION_LEASE_DURATION":     "leader-election-lease-duration",
	"LEADER_ELECTION_RENEW_DEADLINE":     "leader-election-renew-deadline",
	"LEADER_ELECTION_RETRY_PERIOD":       "leader-election-retry-period",
	"REST_CLIENT_TIMEOUT":                "rest-client-timeout",
	"METRICS_SECURE":                     "metrics-secure",
	"ENABLE_HTTP2":                       "enable-http2",
	"WATCH_NAMESPACE":                    "watch-namespace",
	"V":                                  "v",
	"WEBHOOK_CERT_PATH":                  "webhook-cert-path",
	"WEBHOOK_CERT_NAME":                  "webhook-cert-name",
	"WEBHOOK_CERT_KEY":                   "webhook-cert-key",
	"METRICS_CERT_PATH":                  "metrics-cert-path",
	"METRICS_CERT_NAME":                  "metrics-cert-name",
	"METRICS_CERT_KEY":                   "metrics-cert-key",
}

// Load loads and validates the unified configuration.
// Precedence: flags > env > ConfigMap > defaults
// Returns error if required configuration is missing or invalid (fail-fast).
// flagSet may be nil (e.g. in tests that don't set CLI flags).
func Load(ctx context.Context, flagSet *flag.FlagSet, k8sClient client.Client) (*Config, error) {
	cfg := &Config{}

	// Load configuration (flags > env > ConfigMap > defaults)
	if err := loadConfig(ctx, cfg, flagSet, k8sClient); err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Validate required configuration (fail-fast)
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	ctrl.Log.Info("Configuration loaded successfully")
	return cfg, nil
}

// loadConfig loads configuration with precedence: flags > env > ConfigMap > defaults
func loadConfig(ctx context.Context, cfg *Config, flagSet *flag.FlagSet, k8sClient client.Client) error {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()

	v := viper.New()

	// Set defaults
	v.SetDefault("METRICS_BIND_ADDRESS", "0")
	v.SetDefault("HEALTH_PROBE_BIND_ADDRESS", ":8081")
	v.SetDefault("LEADER_ELECT", false)
	v.SetDefault("LEADER_ELECTION_ID", "72dd1cf1.llm-d.ai")
	v.SetDefault("LEADER_ELECTION_LEASE_DURATION", 60*time.Second)
	v.SetDefault("LEADER_ELECTION_RENEW_DEADLINE", 50*time.Second)
	v.SetDefault("LEADER_ELECTION_RETRY_PERIOD", 10*time.Second)
	v.SetDefault("REST_CLIENT_TIMEOUT", 60*time.Second)
	v.SetDefault("METRICS_SECURE", true)
	v.SetDefault("ENABLE_HTTP2", false)
	v.SetDefault("WATCH_NAMESPACE", "")
	v.SetDefault("V", 0)
	v.SetDefault("WEBHOOK_CERT_PATH", "")
	v.SetDefault("WEBHOOK_CERT_NAME", "tls.crt")
	v.SetDefault("WEBHOOK_CERT_KEY", "tls.key")
	v.SetDefault("METRICS_CERT_PATH", "")
	v.SetDefault("METRICS_CERT_NAME", "tls.crt")
	v.SetDefault("METRICS_CERT_KEY", "tls.key")
	v.SetDefault("WVA_SCALE_TO_ZERO", false)
	v.SetDefault("WVA_LIMITED_MODE", false)
	v.SetDefault("SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY", 10)
	v.SetDefault("EPP_METRIC_READER_BEARER_TOKEN", "")

	// Load from ConfigMap (if available) — sits between env and defaults in precedence
	cmName := ConfigMapName()
	cmNamespace := SystemNamespace()
	cm := &corev1.ConfigMap{}
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, cmName, cmNamespace, cm); err == nil {
		ctrl.Log.Info("Loaded ConfigMap for config", "name", cmName, "namespace", cmNamespace)
		if len(cm.Data) > 0 {
			buf := new(bytes.Buffer)
			for k, val := range cm.Data {
				fmt.Fprintf(buf, "%s=%s\n", k, val)
			}
			v.SetConfigType("dotenv")
			if err := v.ReadConfig(buf); err != nil {
				ctrl.Log.Info("Failed to parse ConfigMap data into viper", "error", err)
			}
		}
	} else {
		ctrl.Log.Info("ConfigMap not found, using defaults and flags", "name", cmName, "namespace", cmNamespace, "error", err)
	}

	// Bind environment variables (precedence above config, below flags)
	v.AutomaticEnv()

	// Bind pflag flags (highest precedence for explicitly-set flags)
	if flagSet != nil {
		for viperKey, flagName := range flagBindings {
			if f := flagSet.Lookup(flagName); f != nil {
				_ = v.BindPFlag(viperKey, f)
			}
		}
	}

	// Read resolved values into Config
	cfg.infrastructure = infrastructureConfig{
		metricsAddr:          v.GetString("METRICS_BIND_ADDRESS"),
		probeAddr:            v.GetString("HEALTH_PROBE_BIND_ADDRESS"),
		enableLeaderElection: v.GetBool("LEADER_ELECT"),
		leaderElectionID:     v.GetString("LEADER_ELECTION_ID"),
		leaseDuration:        v.GetDuration("LEADER_ELECTION_LEASE_DURATION"),
		renewDeadline:        v.GetDuration("LEADER_ELECTION_RENEW_DEADLINE"),
		retryPeriod:          v.GetDuration("LEADER_ELECTION_RETRY_PERIOD"),
		restTimeout:          v.GetDuration("REST_CLIENT_TIMEOUT"),
		secureMetrics:        v.GetBool("METRICS_SECURE"),
		enableHTTP2:          v.GetBool("ENABLE_HTTP2"),
		watchNamespace:       v.GetString("WATCH_NAMESPACE"),
		loggerVerbosity:      v.GetInt("V"),
	}

	cfg.tls = tlsConfig{
		webhookCertPath: v.GetString("WEBHOOK_CERT_PATH"),
		webhookCertName: v.GetString("WEBHOOK_CERT_NAME"),
		webhookCertKey:  v.GetString("WEBHOOK_CERT_KEY"),
		metricsCertPath: v.GetString("METRICS_CERT_PATH"),
		metricsCertName: v.GetString("METRICS_CERT_NAME"),
		metricsCertKey:  v.GetString("METRICS_CERT_KEY"),
	}

	cfg.optimization = optimizationConfig{
		interval: defaultOptimizationInterval,
	}

	cfg.features = featureFlagsConfig{
		scaleToZeroEnabled:          v.GetBool("WVA_SCALE_TO_ZERO"),
		limitedModeEnabled:          v.GetBool("WVA_LIMITED_MODE"),
		scaleFromZeroMaxConcurrency: v.GetInt("SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY"),
	}

	cfg.saturation = saturationConfig{
		global:           make(SaturationScalingConfigPerModel),
		namespaceConfigs: make(map[string]SaturationScalingConfigPerModel),
	}

	cfg.scaleToZero = scaleToZeroConfig{
		global:           make(ScaleToZeroConfigData),
		namespaceConfigs: make(map[string]ScaleToZeroConfigData),
	}

	cfg.prometheus.cache = defaultPrometheusCacheConfig()

	cfg.epp.metricReaderBearerToken = v.GetString("EPP_METRIC_READER_BEARER_TOKEN")

	// Load Prometheus config (required) - env > ConfigMap
	promConfig, err := PrometheusConfig(ctx, k8sClient)
	if err != nil {
		return fmt.Errorf("failed to load Prometheus config: %w", err)
	}
	if promConfig == nil {
		return fmt.Errorf("prometheus configuration is required but not found. set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
	}
	cfg.prometheus.baseURL = promConfig.BaseURL
	cfg.prometheus.bearerToken = promConfig.BearerToken
	cfg.prometheus.tokenPath = promConfig.TokenPath
	cfg.prometheus.insecureSkipVerify = promConfig.InsecureSkipVerify
	cfg.prometheus.caCertPath = promConfig.CACertPath
	cfg.prometheus.clientCertPath = promConfig.ClientCertPath
	cfg.prometheus.clientKeyPath = promConfig.ClientKeyPath
	cfg.prometheus.serverName = promConfig.ServerName

	// Load dynamic config
	return loadDynamicConfigNew(ctx, cfg, k8sClient)
}

// loadDynamicConfigNew loads dynamic configuration
func loadDynamicConfigNew(ctx context.Context, cfg *Config, k8sClient client.Client) error {
	// Load optimization interval
	cm := &corev1.ConfigMap{}
	cmName := ConfigMapName()
	cmNamespace := SystemNamespace()
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, cmName, cmNamespace, cm); err == nil {
		if intervalStr, ok := cm.Data["GLOBAL_OPT_INTERVAL"]; ok {
			if interval, err := time.ParseDuration(intervalStr); err == nil {
				cfg.optimization.interval = interval
			} else {
				ctrl.Log.Info("Invalid GLOBAL_OPT_INTERVAL, using default", "value", intervalStr, "error", err)
			}
		}
	}

	// Load Prometheus cache config
	if cacheConfig, err := ReadPrometheusCacheConfig(ctx, k8sClient); err == nil && cacheConfig != nil {
		cfg.prometheus.cache = cacheConfig
	}

	// Load scale-to-zero config (global)
	scaleToZeroCM := &corev1.ConfigMap{}
	scaleToZeroCMName := DefaultScaleToZeroConfigMapName
	if err := utils.GetConfigMapWithBackoff(ctx, k8sClient, scaleToZeroCMName, cmNamespace, scaleToZeroCM); err == nil {
		cfg.scaleToZero.global = ParseScaleToZeroConfigMap(scaleToZeroCM.Data)
	}

	// Load saturation scaling config (global)
	saturationCM := &corev1.ConfigMap{}
	saturationCMName := SaturationConfigMapName()
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
			cfg.saturation.global = configs
			ctrl.Log.Info("Loaded saturation scaling config", "entries", len(configs))
		}
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
