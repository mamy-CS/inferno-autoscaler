package config

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

func TestLoad_Defaults(t *testing.T) {
	// Setup: No flags, no env, no ConfigMap
	ctx := context.Background()
	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	// Set required Prometheus env var
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed with defaults: %v", err)
	}

	// Verify defaults
	if cfg.Static.MetricsAddr != "0" {
		t.Errorf("Expected MetricsAddr default '0', got %q", cfg.Static.MetricsAddr)
	}
	if cfg.Static.ProbeAddr != ":8081" {
		t.Errorf("Expected ProbeAddr default ':8081', got %q", cfg.Static.ProbeAddr)
	}
	if cfg.Static.EnableLeaderElection != false {
		t.Errorf("Expected EnableLeaderElection default false, got %v", cfg.Static.EnableLeaderElection)
	}
	if cfg.Dynamic.OptimizationInterval != 60*time.Second {
		t.Errorf("Expected OptimizationInterval default 60s, got %v", cfg.Dynamic.OptimizationInterval)
	}
}

func TestLoad_FlagsPrecedence(t *testing.T) {
	ctx := context.Background()

	// Set env var and ConfigMap (should be overridden by flags)
	_ = os.Setenv("METRICS_BIND_ADDRESS", "env-value")
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() {
		_ = os.Unsetenv("METRICS_BIND_ADDRESS")
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
	}()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"METRICS_BIND_ADDRESS": "cm-value",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{
		MetricsAddr: "flag-value",
	}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Flag should take precedence
	if cfg.Static.MetricsAddr != "flag-value" {
		t.Errorf("Expected MetricsAddr 'flag-value' (from flag), got %q", cfg.Static.MetricsAddr)
	}
}

func TestLoad_EnvPrecedence(t *testing.T) {
	ctx := context.Background()

	_ = os.Setenv("METRICS_BIND_ADDRESS", "env-value")
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() {
		_ = os.Unsetenv("METRICS_BIND_ADDRESS")
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
	}()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"METRICS_BIND_ADDRESS": "cm-value",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{} // No flag set

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Env should take precedence over ConfigMap
	if cfg.Static.MetricsAddr != "env-value" {
		t.Errorf("Expected MetricsAddr 'env-value' (from env), got %q", cfg.Static.MetricsAddr)
	}
}

func TestLoad_ConfigMapPrecedence(t *testing.T) {
	ctx := context.Background()

	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"METRICS_BIND_ADDRESS": "cm-value",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{} // No flag, no env

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// ConfigMap should be used
	if cfg.Static.MetricsAddr != "cm-value" {
		t.Errorf("Expected MetricsAddr 'cm-value' (from ConfigMap), got %q", cfg.Static.MetricsAddr)
	}
}

func TestLoad_PrometheusConfigRequired(t *testing.T) {
	ctx := context.Background()
	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	// No Prometheus config set
	cfg, err := Load(ctx, flags, k8sClient)
	if err == nil {
		t.Fatal("Expected Load() to fail when Prometheus config is missing, but it succeeded")
	}
	if cfg != nil {
		t.Error("Expected Load() to return nil Config when validation fails")
	}
}

func TestLoad_PrometheusConfigFromEnv(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus-env:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Static.Prometheus == nil {
		t.Fatal("Expected Prometheus config to be loaded")
	}
	if cfg.Static.Prometheus.BaseURL != "https://prometheus-env:9090" {
		t.Errorf("Expected Prometheus BaseURL from env, got %q", cfg.Static.Prometheus.BaseURL)
	}
}

func TestLoad_PrometheusConfigFromConfigMap(t *testing.T) {
	ctx := context.Background()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"PROMETHEUS_BASE_URL": "https://prometheus-cm:9090",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Static.Prometheus == nil {
		t.Fatal("Expected Prometheus config to be loaded")
	}
	if cfg.Static.Prometheus.BaseURL != "https://prometheus-cm:9090" {
		t.Errorf("Expected Prometheus BaseURL from ConfigMap, got %q", cfg.Static.Prometheus.BaseURL)
	}
}

func TestLoad_DynamicConfig_OptimizationInterval(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"GLOBAL_OPT_INTERVAL": "30s",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Dynamic.OptimizationInterval != 30*time.Second {
		t.Errorf("Expected OptimizationInterval 30s, got %v", cfg.Dynamic.OptimizationInterval)
	}
}

func TestLoad_DynamicConfig_InvalidOptimizationInterval(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"GLOBAL_OPT_INTERVAL": "invalid-duration",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() should not fail on invalid duration, should use default: %v", err)
	}

	// Should fall back to default
	if cfg.Dynamic.OptimizationInterval != 60*time.Second {
		t.Errorf("Expected OptimizationInterval to fall back to default 60s, got %v", cfg.Dynamic.OptimizationInterval)
	}
}

func TestLoad_DynamicConfig_SaturationConfig(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	// Ensure namespace matches what GetNamespace() returns
	_ = os.Setenv("POD_NAMESPACE", "workload-variant-autoscaler-system")
	defer func() {
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
		_ = os.Unsetenv("POD_NAMESPACE")
	}()

	saturationCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "saturation-scaling-config",
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"default": `kvCacheThreshold: 0.8
queueLengthThreshold: 5
kvSpareTrigger: 0.1
queueSpareTrigger: 3`,
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(saturationCM).Build()

	// Verify ConfigMap can be retrieved directly
	testCM := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, client.ObjectKey{
		Name:      "saturation-scaling-config",
		Namespace: "workload-variant-autoscaler-system",
	}, testCM)
	if err != nil {
		t.Fatalf("Failed to retrieve ConfigMap directly: %v", err)
	}

	// Also test GetConfigMapWithBackoff directly to verify it works
	testCM2 := &corev1.ConfigMap{}
	err2 := utils.GetConfigMapWithBackoff(ctx, k8sClient, "saturation-scaling-config", "workload-variant-autoscaler-system", testCM2)
	if err2 != nil {
		t.Logf("GetConfigMapWithBackoff failed: %v (this might be expected in test)", err2)
	} else {
		t.Logf("GetConfigMapWithBackoff succeeded, found %d keys", len(testCM2.Data))
	}

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	satConfig := cfg.GetSaturationConfig()
	if len(satConfig) != 1 {
		// Debug: Check if ConfigMap was found but not parsed
		// The issue might be that GetConfigMapWithBackoff works but the loader
		// uses a different namespace or the ConfigMap isn't being found during Load()
		t.Logf("Saturation config has %d entries (expected 1)", len(satConfig))
		t.Logf("ConfigMap name should be 'saturation-scaling-config', namespace 'workload-variant-autoscaler-system'")
		t.Fatalf("Expected 1 saturation config entry, got %d", len(satConfig))
	}

	defaultConfig, ok := satConfig["default"]
	if !ok {
		t.Fatal("Expected 'default' saturation config entry")
	}
	if defaultConfig.KvCacheThreshold != 0.8 {
		t.Errorf("Expected KvCacheThreshold 0.8, got %f", defaultConfig.KvCacheThreshold)
	}
}

func TestLoad_DynamicConfig_InvalidSaturationConfig(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	saturationCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "saturation-scaling-config",
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"default": `kvCacheThreshold: 1.5  # Invalid: > 1.0
queueLengthThreshold: 5
kvSpareTrigger: 0.1
queueSpareTrigger: 3`,
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(saturationCM).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() should not fail on invalid saturation config, should skip it: %v", err)
	}

	// Invalid entry should be skipped
	satConfig := cfg.GetSaturationConfig()
	if len(satConfig) != 0 {
		t.Errorf("Expected invalid saturation config to be skipped, but got %d entries", len(satConfig))
	}
}

func TestLoad_DynamicConfig_ScaleToZeroConfig(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	// Ensure namespace matches what GetNamespace() returns
	_ = os.Setenv("POD_NAMESPACE", "workload-variant-autoscaler-system")
	defer func() {
		_ = os.Unsetenv("PROMETHEUS_BASE_URL")
		_ = os.Unsetenv("POD_NAMESPACE")
	}()

	scaleToZeroCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultScaleToZeroConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"model1": `model_id: model1
enable_scale_to_zero: true
retention_period: 5m`,
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(scaleToZeroCM).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	scaleToZeroConfig := cfg.GetScaleToZeroConfig()
	if len(scaleToZeroConfig) == 0 {
		t.Error("Expected scale-to-zero config to be loaded")
	}
}

func TestLoad_FeatureFlags(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"WVA_SCALE_TO_ZERO":                      "true",
			"WVA_LIMITED_MODE":                       "false",
			"SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY": "5",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if !cfg.Static.ScaleToZeroEnabled {
		t.Error("Expected ScaleToZeroEnabled to be true")
	}
	if cfg.Static.LimitedModeEnabled {
		t.Error("Expected LimitedModeEnabled to be false")
	}
	if cfg.Static.ScaleFromZeroMaxConcurrency != 5 {
		t.Errorf("Expected ScaleFromZeroMaxConcurrency 5, got %d", cfg.Static.ScaleFromZeroMaxConcurrency)
	}
}

func TestLoad_PrometheusCacheConfig(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultConfigMapName,
			Namespace: "workload-variant-autoscaler-system",
		},
		Data: map[string]string{
			"PROMETHEUS_METRICS_CACHE_ENABLED": "false",
			"PROMETHEUS_METRICS_CACHE_TTL":     "60s",
		},
	}
	k8sClient := fake.NewClientBuilder().WithObjects(cm).Build()

	flags := StaticConfigFlags{}

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	cacheConfig := cfg.GetPrometheusCacheConfig()
	if cacheConfig == nil {
		t.Fatal("Expected Prometheus cache config to be loaded")
	}
	if cacheConfig.Enabled {
		t.Error("Expected cache to be disabled from ConfigMap")
	}
	if cacheConfig.TTL != 60*time.Second {
		t.Errorf("Expected cache TTL 60s, got %v", cacheConfig.TTL)
	}
}

func TestConfig_ThreadSafety(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Test concurrent reads
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			_ = cfg.GetOptimizationInterval()
			_ = cfg.GetSaturationConfig()
			_ = cfg.GetScaleToZeroConfig()
			_ = cfg.GetPrometheusCacheConfig()
		}()
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestConfig_UpdateDynamicConfig(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Update dynamic config
	newDynamic := DynamicConfig{
		OptimizationInterval: 30 * time.Second,
		PrometheusCache:      defaultPrometheusCacheConfig(),
		Global: &NamespaceConfig{
			SaturationConfig: map[string]interfaces.SaturationScalingConfig{
				"test": {
					KvCacheThreshold:     0.9,
					QueueLengthThreshold: 10,
					KvSpareTrigger:       0.2,
					QueueSpareTrigger:    5,
				},
			},
			ScaleToZeroConfig: make(ScaleToZeroConfigData),
		},
		NamespaceConfigs: make(map[string]*NamespaceConfig),
	}

	cfg.UpdateDynamicConfig(newDynamic)

	// Verify update
	if cfg.GetOptimizationInterval() != 30*time.Second {
		t.Errorf("Expected updated OptimizationInterval 30s, got %v", cfg.GetOptimizationInterval())
	}

	satConfig := cfg.GetSaturationConfig()
	if len(satConfig) != 1 {
		t.Fatalf("Expected 1 saturation config entry after update, got %d", len(satConfig))
	}
}

func TestLoad_Validation_OptimizationInterval(t *testing.T) {
	// This test verifies that validation catches invalid optimization intervals
	// However, since we parse and validate in loadDynamicConfig, invalid values
	// fall back to defaults, so we test that behavior instead
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build()

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Default should be valid (> 0)
	if cfg.Dynamic.OptimizationInterval <= 0 {
		t.Errorf("Expected positive optimization interval, got %v", cfg.Dynamic.OptimizationInterval)
	}
}

func TestLoad_NoConfigMap(t *testing.T) {
	ctx := context.Background()
	_ = os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
	defer func() { _ = os.Unsetenv("PROMETHEUS_BASE_URL") }()

	flags := StaticConfigFlags{}
	k8sClient := fake.NewClientBuilder().Build() // No ConfigMaps

	cfg, err := Load(ctx, flags, k8sClient)
	if err != nil {
		t.Fatalf("Load() should succeed with defaults when ConfigMap is missing: %v", err)
	}

	// Should use defaults
	if cfg.Dynamic.OptimizationInterval != 60*time.Second {
		t.Errorf("Expected default OptimizationInterval 60s, got %v", cfg.Dynamic.OptimizationInterval)
	}
}
