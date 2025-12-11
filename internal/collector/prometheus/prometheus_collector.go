package prometheus

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/cache"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/saturation"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrometheusCollector implements MetricsCollector interface for Prometheus backend
type PrometheusCollector struct {
	promAPI   promv1.API
	k8sClient client.Client
	cache     cache.MetricsCache // Internal cache for metrics

	// Background fetching
	fetchInterval       time.Duration
	freshnessThresholds config.FreshnessThresholds
	fetchExecutor       executor.Executor // Polling executor for background metric fetching
	trackedVAs          sync.Map          // map[string]*TrackedVA - tracks VAs for background fetching
	trackedModels       sync.Map          // map[string]*TrackedModel - tracks models for replica metrics background fetching
}

// TrackedVA holds information about a VA that should be fetched in background
type TrackedVA struct {
	ModelID         string
	Namespace       string
	VariantName     string
	VA              *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	Deployment      appsv1.Deployment
	AcceleratorCost float64
	LastFetch       time.Time
}

// TrackedModel holds information about a model that should have replica metrics fetched in background
type TrackedModel struct {
	ModelID   string
	Namespace string
	LastFetch time.Time
}

// getDefaultCacheConfig returns default cache configuration
func getDefaultCacheConfig() *config.CacheConfig {
	return &config.CacheConfig{
		Enabled:             true, // enabled by default
		TTL:                 30 * time.Second,
		MaxSize:             0, // unlimited by default
		CleanupInterval:     1 * time.Minute,
		FetchInterval:       30 * time.Second, // Fetch every 30s by default
		FreshnessThresholds: config.DefaultFreshnessThresholds(),
	}
}

// NewPrometheusCollector creates a new Prometheus metrics collector with default cache config
// Deprecated: Use NewPrometheusCollectorWithConfig instead
func NewPrometheusCollector(promAPI promv1.API) *PrometheusCollector {
	return NewPrometheusCollectorWithConfig(promAPI, nil)
}

// NewPrometheusCollectorWithConfig creates a new Prometheus metrics collector
// If cacheConfig is nil, uses default cache settings
func NewPrometheusCollectorWithConfig(promAPI promv1.API, cacheConfig *config.CacheConfig) *PrometheusCollector {
	// Use provided config or defaults
	cfg := getDefaultCacheConfig()
	if cacheConfig != nil {
		cfg = cacheConfig
	}

	var metricsCache cache.MetricsCache

	if cfg.Enabled {
		metricsCache = cache.NewMemoryCache(cfg.TTL, cfg.MaxSize, cfg.CleanupInterval)
		logger.Log.Infof("Metrics cache enabled: TTL=%v, MaxSize=%d, CleanupInterval=%v",
			cfg.TTL, cfg.MaxSize, cfg.CleanupInterval)
	} else {
		// Use a no-op cache implementation when disabled
		metricsCache = &cache.NoOpCache{}
		logger.Log.Info("Metrics cache disabled")
	}

	pc := &PrometheusCollector{
		promAPI:             promAPI,
		k8sClient:           nil, // Will be set when available
		cache:               metricsCache,
		fetchInterval:       cfg.FetchInterval,
		freshnessThresholds: cfg.FreshnessThresholds,
	}

	// Initialize background fetching executor if fetch interval is configured
	if cfg.FetchInterval > 0 {
		pc.fetchExecutor = executor.NewPollingExecutor(executor.PollingConfig{
			Config: executor.Config{
				OptimizeFunc: func(ctx context.Context) error {
					// Fetch both allocation metrics (per-VA) and replica metrics (per-model)
					pc.fetchTrackedVAs(ctx)
					pc.fetchTrackedModels(ctx)
					return nil // Fire-and-forget, errors are logged per-VA/per-model
				},
			},
			Interval:     cfg.FetchInterval,
			RetryBackoff: 500 * time.Millisecond, // Initial backoff for retries (doubles, capped at 4s)
		})
		logger.Log.Infof("Initialized background fetching executor with interval: %v", cfg.FetchInterval)
	}

	return pc
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

// StartBackgroundWorker starts the background worker for periodic metric fetching
// This should be called with a context that will be cancelled when the collector is stopped
func (pc *PrometheusCollector) StartBackgroundWorker(ctx context.Context) {
	if pc.fetchExecutor == nil {
		logger.Log.Info("Background worker not started: fetch executor not initialized (fetch interval is 0 or negative)")
		return
	}

	go pc.fetchExecutor.Start(ctx)
	logger.Log.Infof("Started background fetching executor with interval: %v", pc.fetchInterval)
}

// StopBackgroundWorker stops the background worker gracefully
// Note: With PollingExecutor, stopping is handled via context cancellation
func (pc *PrometheusCollector) StopBackgroundWorker() {
	logger.Log.Info("Background fetching executor will stop when context is cancelled")
}

// TrackVA registers a VA for background fetching
func (pc *PrometheusCollector) TrackVA(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, deployment appsv1.Deployment, acceleratorCost float64) {
	key := fmt.Sprintf("%s/%s/%s", va.Spec.ModelID, va.Namespace, va.Name)
	tracked := &TrackedVA{
		ModelID:         va.Spec.ModelID,
		Namespace:       va.Namespace,
		VariantName:     va.Name,
		VA:              va,
		Deployment:      deployment,
		AcceleratorCost: acceleratorCost,
		LastFetch:       time.Time{}, // Never fetched
	}
	pc.trackedVAs.Store(key, tracked)
	logger.Log.Debugf("Tracking VA for background fetching: key=%s", key)
}

// UntrackVA removes a VA from background fetching
func (pc *PrometheusCollector) UntrackVA(modelID, namespace, variantName string) {
	key := fmt.Sprintf("%s/%s/%s", modelID, namespace, variantName)
	pc.trackedVAs.Delete(key)
	logger.Log.Debugf("Untracked VA from background fetching: key=%s", key)
}

// TrackModel registers a model for background replica metrics fetching
func (pc *PrometheusCollector) TrackModel(modelID, namespace string) {
	key := fmt.Sprintf("%s/%s", modelID, namespace)
	tracked := &TrackedModel{
		ModelID:   modelID,
		Namespace: namespace,
		LastFetch: time.Time{}, // Never fetched
	}
	pc.trackedModels.Store(key, tracked)
	logger.Log.Debugf("Tracking model for background replica metrics fetching: key=%s", key)
}

// UntrackModel removes a model from background replica metrics fetching
func (pc *PrometheusCollector) UntrackModel(modelID, namespace string) {
	key := fmt.Sprintf("%s/%s", modelID, namespace)
	pc.trackedModels.Delete(key)
	logger.Log.Debugf("Untracked model from background replica metrics fetching: key=%s", key)
}

// fetchTrackedVAs fetches metrics for all tracked VAs with exponential backoff retry
// This is called by the PollingExecutor at configured intervals
func (pc *PrometheusCollector) fetchTrackedVAs(ctx context.Context) {
	now := time.Now()
	var wg sync.WaitGroup

	pc.trackedVAs.Range(func(key, value interface{}) bool {
		tracked, ok := value.(*TrackedVA)
		if !ok {
			// Invalid type stored in map, skip it
			logger.Log.Warnf("Invalid type in trackedVAs map for key %v, skipping", key)
			return true // continue
		}

		// Skip if fetched recently (within fetch interval)
		if !tracked.LastFetch.IsZero() && now.Sub(tracked.LastFetch) < pc.fetchInterval {
			return true // continue
		}

		wg.Add(1)
		go func(t *TrackedVA) {
			defer wg.Done()
			pc.fetchVAMetricsWithRetry(ctx, t)
		}(tracked)

		return true // continue
	})

	wg.Wait()
}

// fetchTrackedModels fetches replica metrics for all tracked models with exponential backoff retry
// This is called by the PollingExecutor at configured intervals
func (pc *PrometheusCollector) fetchTrackedModels(ctx context.Context) {
	if pc.k8sClient == nil {
		logger.Log.Debug("Skipping replica metrics background fetch: K8s client not set")
		return
	}

	now := time.Now()
	var wg sync.WaitGroup

	pc.trackedModels.Range(func(key, value interface{}) bool {
		tracked, ok := value.(*TrackedModel)
		if !ok {
			// Invalid type stored in map, skip it
			logger.Log.Warnf("Invalid type in trackedModels map for key %v, skipping", key)
			return true // continue
		}

		// Skip if fetched recently (within fetch interval)
		if !tracked.LastFetch.IsZero() && now.Sub(tracked.LastFetch) < pc.fetchInterval {
			return true // continue
		}

		wg.Add(1)
		go func(t *TrackedModel) {
			defer wg.Done()
			pc.fetchModelReplicaMetricsWithRetry(ctx, t)
		}(tracked)

		return true // continue
	})

	wg.Wait()
}

// fetchModelReplicaMetricsWithRetry fetches replica metrics for a single model with exponential backoff retry
func (pc *PrometheusCollector) fetchModelReplicaMetricsWithRetry(parentCtx context.Context, tracked *TrackedModel) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	// Use standard Prometheus query backoff for consistency with existing patterns
	err := wait.ExponentialBackoffWithContext(ctx, utils.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		// Build deployments, variantAutoscalings, and variantCosts maps fresh from K8s
		deployments, variantAutoscalings, variantCosts, err := pc.buildModelMaps(ctx, tracked.ModelID, tracked.Namespace)
		if err != nil {
			logger.Log.Debugf("Background fetch failed to build maps for model %s: %v (will retry)", tracked.ModelID, err)
			return false, nil // Retry
		}

		if len(variantAutoscalings) == 0 {
			// No VAs found for this model, skip fetching (but don't retry)
			logger.Log.Debugf("No VAs found for model %s in namespace %s, skipping background fetch", tracked.ModelID, tracked.Namespace)
			return true, nil // Success (nothing to fetch)
		}

		// Fetch replica metrics
		_, err = pc.fetchReplicaMetrics(ctx, tracked.ModelID, tracked.Namespace, deployments, variantAutoscalings, variantCosts)
		if err != nil {
			logger.Log.Debugf("Background fetch failed for model %s: %v (will retry)", tracked.ModelID, err)
			return false, nil // Retry
		}

		// Success - update last fetch time
		tracked.LastFetch = time.Now()
		logger.Log.Debugf("Background replica metrics fetch succeeded for model: model=%s, namespace=%s",
			tracked.ModelID, tracked.Namespace)

		return true, nil // Success, stop retrying
	})

	if err != nil {
		// Context cancelled or backoff exhausted
		if ctx.Err() == context.DeadlineExceeded {
			logger.Log.Debugf("Background replica metrics fetch timeout for model %s after 30s", tracked.ModelID)
		} else if ctx.Err() == context.Canceled {
			logger.Log.Debugf("Background replica metrics fetch cancelled for model %s", tracked.ModelID)
		} else {
			logger.Log.Debugf("Background replica metrics fetch failed for model %s after all retries: %v", tracked.ModelID, err)
		}
	}
}

// buildModelMaps builds deployments, variantAutoscalings, and variantCosts maps for a model
// by querying Kubernetes API for VAs in the namespace and filtering by modelID
func (pc *PrometheusCollector) buildModelMaps(ctx context.Context, modelID, namespace string) (
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
	err error,
) {
	deployments = make(map[string]*appsv1.Deployment)
	variantAutoscalings = make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
	variantCosts = make(map[string]float64)

	// List all VAs in the namespace
	var vaList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
	}
	if err := pc.k8sClient.List(ctx, &vaList, listOpts...); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list VariantAutoscaling resources: %w", err)
	}

	// Filter by modelID and build maps
	for i := range vaList.Items {
		va := &vaList.Items[i]
		if va.Spec.ModelID != modelID {
			continue // Skip VAs that don't match the modelID
		}

		// Parse variant cost
		cost := saturation.DefaultVariantCost // default
		if va.Spec.VariantCost != "" {
			if parsedCost, parseErr := strconv.ParseFloat(va.Spec.VariantCost, 64); parseErr == nil {
				cost = parsedCost
			}
		}
		variantCosts[va.Name] = cost

		// Get the deployment for this VA
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, pc.k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy); err != nil {
			logger.Log.Debugf("Could not get deployment for VA in background fetch: variant=%s, deployment=%s, error=%v",
				va.Name, va.GetScaleTargetName(), err)
			continue // Skip this VA if we can't get its deployment
		}

		deployments[va.Name] = &deploy
		variantAutoscalings[va.Name] = va
	}

	return deployments, variantAutoscalings, variantCosts, nil
}

// fetchReplicaMetrics is an internal helper for background worker that fetches replica metrics
// and stores them in cache (similar to fetchAllocationMetrics for allocation metrics)
func (pc *PrometheusCollector) fetchReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	// Use the existing SaturationMetricsCollector implementation
	saturationCollector := &SaturationMetricsCollector{
		promAPI:   pc.promAPI,
		k8sClient: pc.k8sClient,
	}
	replicaMetrics, err := saturationCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, err
	}

	// Store in cache without metadata (metadata added on retrieval)
	cacheMetrics := make([]interfaces.ReplicaMetrics, len(replicaMetrics))
	for i := range replicaMetrics {
		cacheMetrics[i] = replicaMetrics[i]
		cacheMetrics[i].Metadata = nil // Don't store metadata in cache
	}
	cacheKey := cache.NewCacheKey(modelID, namespace, "all", "replica-metrics")
	pc.cache.Set(cacheKey, cacheMetrics, 0) // 0 means use cache's default TTL

	return replicaMetrics, nil
}

// fetchVAMetricsWithRetry fetches metrics for a single VA with exponential backoff retry
func (pc *PrometheusCollector) fetchVAMetricsWithRetry(parentCtx context.Context, tracked *TrackedVA) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	// Use standard Prometheus query backoff for consistency with existing patterns
	// This uses exponential backoff with jitter: 500ms, 1s, 2s, 4s, 8s (with jitter)
	err := wait.ExponentialBackoffWithContext(ctx, utils.PrometheusQueryBackoff, func(ctx context.Context) (bool, error) {
		// Try to fetch allocation metrics
		alloc, err := pc.fetchAllocationMetrics(ctx, tracked.VA, tracked.Deployment, tracked.AcceleratorCost)
		if err != nil {
			logger.Log.Debugf("Background fetch failed for VA %s: %v (will retry)", tracked.VariantName, err)
			return false, nil // Retry
		}

		// Success - update last fetch time
		tracked.LastFetch = time.Now()
		logger.Log.Debugf("Background fetch succeeded for VA: model=%s, variant=%s",
			tracked.ModelID, tracked.VariantName)

		// Store in cache without metadata (metadata added on retrieval)
		cacheAlloc := alloc
		cacheAlloc.Metadata = nil // Don't store metadata in cache
		cacheKey := cache.NewCacheKey(tracked.ModelID, tracked.Namespace, tracked.VariantName, "allocation")
		pc.cache.Set(cacheKey, cacheAlloc, 0) // 0 means use cache's default TTL

		return true, nil // Success, stop retrying
	})

	if err != nil {
		// Context cancelled or backoff exhausted (though with context timeout, it's likely context cancellation)
		if ctx.Err() == context.DeadlineExceeded {
			logger.Log.Debugf("Background fetch timeout for VA %s after 30s", tracked.VariantName)
		} else if ctx.Err() == context.Canceled {
			logger.Log.Debugf("Background fetch cancelled for VA %s", tracked.VariantName)
		} else {
			logger.Log.Debugf("Background fetch failed for VA %s after all retries: %v", tracked.VariantName, err)
		}
	}
}

// fetchAllocationMetrics fetches allocation metrics (internal method used by background worker)
// This calls the core fetching logic without adding metadata or tracking
func (pc *PrometheusCollector) fetchAllocationMetrics(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
) (llmdVariantAutoscalingV1alpha1.Allocation, error) {
	// Call core fetching logic (skip cache check, skip metadata, skip tracking)
	return pc.fetchAllocationMetricsCore(ctx, va, deployment, acceleratorCostVal)
}

// fetchAllocationMetricsCore contains the core logic for fetching allocation metrics from Prometheus
// This is extracted to be reusable by both on-demand and background fetching
func (pc *PrometheusCollector) fetchAllocationMetricsCore(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
) (llmdVariantAutoscalingV1alpha1.Allocation, error) {
	deployNamespace := deployment.Namespace
	modelName := va.Spec.ModelID

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

	// Metric 4: Average TTFT (Time To First Token) - Prometheus returns seconds, converted to msec
	ttftQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 5: Average ITL (Inter-Token Latency) - Prometheus returns seconds, converted to msec
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
	// Convert from seconds (Prometheus format) to milliseconds (optimizer format)
	ttftAverageTime *= 1000

	itlAverage, err := pc.queryAndExtractMetric(ctx, itlQuery, "ITLAverage")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	// Convert from seconds (Prometheus format) to milliseconds (optimizer format)
	itlAverage *= 1000

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

	// --- 4. Build Allocation ---

	// Format metric values, ensuring they meet CRD validation regex '^\\d+(\\.\\d+)?$'
	variantCostStr := strconv.FormatFloat(discoveredCost, 'f', 2, 64)
	ttftAverageStr := strconv.FormatFloat(ttftAverageTime, 'f', 2, 64)
	itlAverageStr := strconv.FormatFloat(itlAverage, 'f', 2, 64)
	arrivalRateStr := strconv.FormatFloat(arrivalVal, 'f', 2, 64)
	avgInputTokensStr := strconv.FormatFloat(avgInputTokens, 'f', 2, 64)
	avgOutputTokensStr := strconv.FormatFloat(avgOutputTokens, 'f', 2, 64)

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

	return currentAlloc, nil
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

	vec, ok := val.(model.Vector)
	if !ok {
		// Type mismatch - should not happen but handle gracefully
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("prometheus query returned unexpected type (expected Vector, got %s)", val.Type().String()),
		}
	}
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
			if v, ok := val.(model.Vector); ok {
				vec = v
			} else {
				// Type mismatch - should not happen but handle gracefully
				return interfaces.MetricsValidationResult{
					Available: false,
					Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
					Message:   "Unexpected type conversion error for Prometheus query result",
				}
			}
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

	// Check cache first (always non-blocking)
	cacheKey := cache.NewCacheKey(modelName, deployNamespace, variantName, "allocation")
	if cached, found := pc.cache.Get(cacheKey); found {
		logger.Log.Debugf("Cache hit for allocation metrics: model=%s, variant=%s, age=%v",
			modelName, variantName, cached.Age())
		// Type assert to Allocation
		if alloc, ok := cached.Data.(llmdVariantAutoscalingV1alpha1.Allocation); ok {
			// Add freshness metadata
			alloc.Metadata = NewMetricsMetadata(cached.CollectedAt, pc.freshnessThresholds)
			// Track VA for background fetching
			pc.TrackVA(va, deployment, acceleratorCostVal)
			return alloc, nil
		}
		// If type assertion fails, continue to query
		logger.Log.Warnf("Cache entry has wrong type, querying Prometheus: model=%s, variant=%s",
			modelName, variantName)
	}

	logger.Log.Debugf("Cache miss for allocation metrics, querying Prometheus: model=%s, variant=%s",
		modelName, variantName)

	// Use core fetching logic (extracted for reuse by background worker)
	currentAlloc, err := pc.fetchAllocationMetricsCore(ctx, va, deployment, acceleratorCostVal)
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}

	// Add freshness metadata (fresh, just collected)
	collectedAt := time.Now()
	currentAlloc.Metadata = NewMetricsMetadata(collectedAt, pc.freshnessThresholds)

	// Store in cache (use default TTL from cache config)
	// Store without metadata in cache (metadata added when retrieved)
	cacheAlloc := currentAlloc
	cacheAlloc.Metadata = nil             // Don't store metadata in cache, recalculate on retrieval
	pc.cache.Set(cacheKey, cacheAlloc, 0) // 0 means use cache's default TTL
	logger.Log.Debugf("Cached allocation metrics: model=%s, variant=%s, freshness=%s",
		modelName, variantName, currentAlloc.Metadata.FreshnessStatus)

	// Track VA for background fetching
	pc.TrackVA(va, deployment, acceleratorCostVal)

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
	cacheKey := cache.NewCacheKey(modelID, namespace, "all", "replica-metrics")
	if cached, found := pc.cache.Get(cacheKey); found {
		logger.Log.Debugf("Cache hit for replica metrics: model=%s, namespace=%s, age=%v",
			modelID, namespace, cached.Age())
		// Type assert to []ReplicaMetrics
		if replicaMetrics, ok := cached.Data.([]interfaces.ReplicaMetrics); ok {
			// Add freshness metadata to each replica metric
			age := cached.Age()
			freshnessStatus := DetermineFreshnessStatus(age, pc.freshnessThresholds)
			for i := range replicaMetrics {
				replicaMetrics[i].Metadata = &interfaces.ReplicaMetricsMetadata{
					CollectedAt:     cached.CollectedAt,
					Age:             age,
					FreshnessStatus: freshnessStatus,
				}
			}
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

	// Add freshness metadata (fresh, just collected)
	collectedAt := time.Now()
	age := time.Duration(0) // Fresh
	freshnessStatus := "fresh"
	for i := range replicaMetrics {
		replicaMetrics[i].Metadata = &interfaces.ReplicaMetricsMetadata{
			CollectedAt:     collectedAt,
			Age:             age,
			FreshnessStatus: freshnessStatus,
		}
	}

	// Store in cache without metadata (metadata added on retrieval)
	cacheMetrics := make([]interfaces.ReplicaMetrics, len(replicaMetrics))
	for i := range replicaMetrics {
		cacheMetrics[i] = replicaMetrics[i]
		cacheMetrics[i].Metadata = nil // Don't store metadata in cache
	}
	pc.cache.Set(cacheKey, cacheMetrics, 0) // 0 means use cache's default TTL
	logger.Log.Debugf("Cached replica metrics: model=%s, namespace=%s, count=%d",
		modelID, namespace, len(replicaMetrics))

	// Track model for background fetching
	pc.TrackModel(modelID, namespace)

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

	vec, ok := val.(model.Vector)
	if !ok {
		// Type mismatch - should not happen but handle gracefully
		return 0.0, fmt.Errorf("prometheus query returned unexpected type (expected Vector, got %s)", val.Type().String())
	}
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
