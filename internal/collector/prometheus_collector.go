package collector

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrometheusCollector implements MetricsCollector interface for Prometheus backend
type PrometheusCollector struct {
	promAPI   promv1.API
	k8sClient client.Client
	cache     MetricsCache // Internal cache for metrics
}

// CacheConfig holds configuration for the metrics cache
type CacheConfig struct {
	Enabled         bool
	TTL             time.Duration
	MaxSize         int
	CleanupInterval time.Duration
}

// getDefaultCacheConfig returns default cache configuration
func getDefaultCacheConfig() CacheConfig {
	return CacheConfig{
		Enabled:         true, // enabled by default
		TTL:             30 * time.Second,
		MaxSize:         0, // unlimited by default
		CleanupInterval: 1 * time.Minute,
	}
}

// NewPrometheusCollector creates a new Prometheus metrics collector with default cache config
// Deprecated: Use NewPrometheusCollectorWithConfig instead
func NewPrometheusCollector(promAPI promv1.API) *PrometheusCollector {
	return NewPrometheusCollectorWithConfig(promAPI, nil)
}

// NewPrometheusCollectorWithConfig creates a new Prometheus metrics collector
// If cacheConfig is nil, uses default cache settings
func NewPrometheusCollectorWithConfig(promAPI promv1.API, cacheConfig *CacheConfig) *PrometheusCollector {
	// Use provided config or defaults
	config := getDefaultCacheConfig()
	if cacheConfig != nil {
		config = *cacheConfig
	}

	var cache MetricsCache

	if config.Enabled {
		cache = NewMemoryCache(config.TTL, config.MaxSize, config.CleanupInterval)
		logger.Log.Infof("Metrics cache enabled: TTL=%v, MaxSize=%d, CleanupInterval=%v",
			config.TTL, config.MaxSize, config.CleanupInterval)
	} else {
		// Use a no-op cache implementation when disabled
		cache = &noOpCache{}
		logger.Log.Info("Metrics cache disabled")
	}

	return &PrometheusCollector{
		promAPI:   promAPI,
		k8sClient: nil, // Will be set when available
		cache:     cache,
	}
}

// SetK8sClient sets the Kubernetes client for pod ownership lookups
func (pc *PrometheusCollector) SetK8sClient(k8sClient client.Client) {
	pc.k8sClient = k8sClient
}

// InvalidateCacheForVariant invalidates cache entries for a specific variant
// This should be called when replica counts change or deployments are updated
func (pc *PrometheusCollector) InvalidateCacheForVariant(modelID, namespace, variantName string) {
	if pc.cache != nil {
		pc.cache.InvalidateForVariant(modelID, namespace, variantName)
		logger.Log.Debugf("Invalidated cache for variant: model=%s, namespace=%s, variant=%s",
			modelID, namespace, variantName)
	}
}

// InvalidateCacheForModel invalidates all cache entries for a model
// This should be called when model-level changes occur
func (pc *PrometheusCollector) InvalidateCacheForModel(modelID, namespace string) {
	if pc.cache != nil {
		pc.cache.InvalidateForModel(modelID, namespace)
		logger.Log.Debugf("Invalidated cache for model: model=%s, namespace=%s",
			modelID, namespace)
	}
}

// ValidateMetricsAvailability implements MetricsCollector interface
func (pc *PrometheusCollector) ValidateMetricsAvailability(
	ctx context.Context,
	modelName string,
	namespace string,
) interfaces.MetricsValidationResult {
	// Query for basic vLLM metric to validate scraping is working
	// Try with namespace label first (real vLLM), fall back to just model_name (vllme emulator)
	testQuery := fmt.Sprintf(`%s{model_name="%s",namespace="%s"}`, constants.VLLMNumRequestRunning, modelName, namespace)

	val, _, err := utils.QueryPrometheusWithBackoff(ctx, pc.promAPI, testQuery)
	if err != nil {
		logger.Log.Error(err, "Error querying Prometheus for metrics validation",
			"model", modelName, "namespace", namespace)
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
		}
	}

	// Check if we got any results
	if val.Type() != model.ValVector {
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
			Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check ServiceMonitor configuration and ensure vLLM pods are exposing /metrics endpoint", modelName, namespace),
		}
	}

	vec := val.(model.Vector)
	// If no results with namespace label, try without it (for vllme emulator compatibility)
	if len(vec) == 0 {
		testQueryFallback := fmt.Sprintf(`%s{model_name="%s"}`, constants.VLLMNumRequestRunning, modelName)
		val, _, err = utils.QueryPrometheusWithBackoff(ctx, pc.promAPI, testQueryFallback)
		if err != nil {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
			}
		}

		if val.Type() == model.ValVector {
			vec = val.(model.Vector)
		}

		// If still no results, metrics are truly missing
		if len(vec) == 0 {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
				Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check: (1) ServiceMonitor exists in monitoring namespace, (2) ServiceMonitor selector matches vLLM service labels, (3) vLLM pods are running and exposing /metrics endpoint, (4) Prometheus is scraping the monitoring namespace", modelName, namespace),
			}
		}
	}

	// Check if metrics are stale (older than 5 minutes)
	for _, sample := range vec {
		age := time.Since(sample.Timestamp.Time())
		if age > 5*time.Minute {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsStale,
				Message:   fmt.Sprintf("vLLM metrics for model '%s' are stale (last update: %v ago). ServiceMonitor may not be scraping correctly.", modelName, age),
			}
		}
	}

	return interfaces.MetricsValidationResult{
		Available: true,
		Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
		Message:   "vLLM metrics are available and up-to-date",
	}
}

// AddMetricsToOptStatus implements MetricsCollector interface
func (pc *PrometheusCollector) AddMetricsToOptStatus(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
) (llmdVariantAutoscalingV1alpha1.Allocation, error) {
	deployNamespace := deployment.Namespace
	modelName := va.Spec.ModelID
	variantName := va.Name

	// Check cache first
	cacheKey := NewCacheKey(modelName, deployNamespace, variantName, "allocation")
	if cached, found := pc.cache.Get(cacheKey); found {
		logger.Log.Debugf("Cache hit for allocation metrics: model=%s, variant=%s, age=%v",
			modelName, variantName, cached.Age())
		// Type assert to Allocation
		if alloc, ok := cached.Data.(llmdVariantAutoscalingV1alpha1.Allocation); ok {
			return alloc, nil
		}
		// If type assertion fails, continue to query
		logger.Log.Warnf("Cache entry has wrong type, querying Prometheus: model=%s, variant=%s",
			modelName, variantName)
	}

	logger.Log.Debugf("Cache miss for allocation metrics, querying Prometheus: model=%s, variant=%s",
		modelName, variantName)

	// --- 1. Define Queries ---

	// Metric 1: Arrival rate (requests per minute)
	arrivalQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 2: Average prompt length (Input Tokens)
	avgPromptToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestPromptTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestPromptTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 3: Average decode length (Output Tokens)
	avgDecToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestGenerationTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestGenerationTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 4: Average TTFT (Time to First Token) ms
	ttftQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 5: Average ITL (Inter-Token Latency) ms
	itlQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimePerOutputTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimePerOutputTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// --- 2. Execute Queries ---

	arrivalVal, err := pc.queryAndExtractMetric(ctx, arrivalQuery, "ArrivalRate")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	arrivalVal *= 60 // convert from req/sec to req/min

	avgInputTokens, err := pc.queryAndExtractMetric(ctx, avgPromptToksQuery, "AvgInputTokens")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}

	avgOutputTokens, err := pc.queryAndExtractMetric(ctx, avgDecToksQuery, "AvgOutputTokens")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}

	ttftAverageTime, err := pc.queryAndExtractMetric(ctx, ttftQuery, "TTFTAverageTime")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	ttftAverageTime *= 1000 // convert to msec

	itlAverage, err := pc.queryAndExtractMetric(ctx, itlQuery, "ITLAverage")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	itlAverage *= 1000 // convert to msec

	// --- 3. Collect K8s and Static Info ---

	// number of replicas
	numReplicas := int(*deployment.Spec.Replicas)

	// accelerator type - strict validation required
	acc := ""
	if val, ok := va.Labels["inference.optimization/acceleratorName"]; ok && val != "" {
		acc = val
	} else {
		return llmdVariantAutoscalingV1alpha1.Allocation{},
			fmt.Errorf("missing or empty acceleratorName label on VariantAutoscaling object: %s", va.Name)
	}

	// cost
	discoveredCost := float64(*deployment.Spec.Replicas) * acceleratorCostVal

	// max batch size
	// TODO: collect value from server
	maxBatch := 256

	// --- 4. Populate Allocation Status ---

	// Format metric values, ensuring they meet CRD validation regex '^\\d+(\\.\\d+)?$'
	// If values are 0, FormatFloat will produce "0.00" which is valid
	variantCostStr := strconv.FormatFloat(discoveredCost, 'f', 2, 64)
	ttftAverageStr := strconv.FormatFloat(ttftAverageTime, 'f', 2, 64)
	itlAverageStr := strconv.FormatFloat(itlAverage, 'f', 2, 64)
	arrivalRateStr := strconv.FormatFloat(arrivalVal, 'f', 2, 64)
	avgInputTokensStr := strconv.FormatFloat(avgInputTokens, 'f', 2, 64)
	avgOutputTokensStr := strconv.FormatFloat(avgOutputTokens, 'f', 2, 64)

	// populate current alloc
	currentAlloc := llmdVariantAutoscalingV1alpha1.Allocation{
		Accelerator: acc,
		NumReplicas: numReplicas,
		MaxBatch:    maxBatch,
		VariantCost: variantCostStr,
		TTFTAverage: ttftAverageStr,
		ITLAverage:  itlAverageStr,
		Load: llmdVariantAutoscalingV1alpha1.LoadProfile{
			ArrivalRate:     arrivalRateStr,
			AvgInputTokens:  avgInputTokensStr,
			AvgOutputTokens: avgOutputTokensStr,
		},
	}

	// Store in cache (use default TTL from cache config)
	pc.cache.Set(cacheKey, currentAlloc, 0) // 0 means use cache's default TTL
	logger.Log.Debugf("Cached allocation metrics: model=%s, variant=%s",
		modelName, variantName)

	return currentAlloc, nil
}

// CollectReplicaMetrics implements MetricsCollector interface
// This method delegates to the SaturationMetricsCollector logic
func (pc *PrometheusCollector) CollectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	// Check cache first (cache key is model-level, not variant-level)
	cacheKey := NewCacheKey(modelID, namespace, "all", "replica-metrics")
	if cached, found := pc.cache.Get(cacheKey); found {
		logger.Log.Debugf("Cache hit for replica metrics: model=%s, namespace=%s, age=%v",
			modelID, namespace, cached.Age())
		// Type assert to []ReplicaMetrics
		if replicaMetrics, ok := cached.Data.([]interfaces.ReplicaMetrics); ok {
			return replicaMetrics, nil
		}
		// If type assertion fails, continue to query
		logger.Log.Warnf("Cache entry has wrong type, querying Prometheus: model=%s, namespace=%s",
			modelID, namespace)
	}

	logger.Log.Debugf("Cache miss for replica metrics, querying Prometheus: model=%s, namespace=%s",
		modelID, namespace)

	// Use the existing SaturationMetricsCollector implementation
	// We'll refactor this to be part of PrometheusCollector later, but for now
	// we maintain compatibility by using the existing struct
	saturationCollector := &SaturationMetricsCollector{
		promAPI:   pc.promAPI,
		k8sClient: pc.k8sClient,
	}
	replicaMetrics, err := saturationCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, err
	}

	// Store in cache (use default TTL from cache config)
	pc.cache.Set(cacheKey, replicaMetrics, 0) // 0 means use cache's default TTL
	logger.Log.Debugf("Cached replica metrics: model=%s, namespace=%s, count=%d",
		modelID, namespace, len(replicaMetrics))

	return replicaMetrics, nil
}

// queryAndExtractMetric performs a Prometheus query and extracts the float value
func (pc *PrometheusCollector) queryAndExtractMetric(ctx context.Context, query string, metricName string) (float64, error) {
	val, warn, err := utils.QueryPrometheusWithBackoff(ctx, pc.promAPI, query)
	if err != nil {
		return 0.0, fmt.Errorf("failed to query Prometheus for %s: %w", metricName, err)
	}

	if warn != nil {
		logger.Log.Warn("Prometheus warnings", "metric", metricName, "warnings", warn)
	}

	// Check if the result type is a Vector
	if val.Type() != model.ValVector {
		logger.Log.Debug("Prometheus query returned non-vector type", "metric", metricName, "type", val.Type().String())
		return 0.0, nil
	}

	vec := val.(model.Vector)
	resultVal := 0.0
	if len(vec) > 0 {
		resultVal = float64(vec[0].Value)
		// Handle NaN or Inf values
		fixValue(&resultVal)
	}

	return resultVal, nil
}

// fixValue handles if a value is NaN or infinite
func fixValue(x *float64) {
	if math.IsNaN(*x) || math.IsInf(*x, 0) {
		*x = 0
	}
}
