package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
)

type AcceleratorModelInfo struct {
	Count  int
	Memory string
}

// TODO: Resource accounting and capacity tracking for limited mode.
// The WVA currently operates in unlimited mode only, where each variant receives
// optimal allocation independently without cluster capacity constraints.
// Limited mode support requires integration with the llmd stack and additional
// design work to handle degraded mode operations without violating SLOs.
// Future work: Implement CollectInventoryK8S and capacity-aware allocation for limited mode.

// vendors list for GPU vendors - kept for future limited mode support
var vendors = []string{
	"nvidia.com",
	"amd.com",
	"intel.com",
}

// CollectInventoryK8S is a stub for future limited mode support.
// Currently returns empty inventory as WVA operates in unlimited mode.
func CollectInventoryK8S(ctx context.Context, r interface{}) (map[string]map[string]AcceleratorModelInfo, error) {
	// Stub implementation - will be properly implemented for limited mode
	return make(map[string]map[string]AcceleratorModelInfo), nil
}

type MetricKV struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// queryAndExtractMetricWithCircuitBreaker performs a Prometheus query with optional circuit breaker protection.
// If circuitBreaker is nil, queries are executed without circuit breaker protection.
func queryAndExtractMetricWithCircuitBreaker(ctx context.Context, promAPI promv1.API, query string, metricName string, circuitBreaker *utils.PrometheusCircuitBreaker) (float64, error) {
	var val model.Value
	var warn promv1.Warnings
	var err error

	if circuitBreaker != nil {
		// Use circuit breaker for query
		result, warnings, queryErr := circuitBreaker.QueryPrometheus(ctx, query)
		if queryErr != nil {
			if errors.Is(queryErr, utils.ErrCircuitBreakerOpen) {
				return 0.0, fmt.Errorf("circuit breaker is open: Prometheus unavailable for %s", metricName)
			}
			return 0.0, fmt.Errorf("failed to query Prometheus for %s: %w", metricName, queryErr)
		}
		// Safe type assertion with error handling
		var ok bool
		val, ok = result.(model.Value)
		if !ok {
			return 0.0, fmt.Errorf("unexpected result type from Prometheus query for %s: got %T, expected model.Value", metricName, result)
		}
		warn = warnings
	} else {
		// Fallback to direct query without circuit breaker
		val, warn, err = utils.QueryPrometheusWithBackoff(ctx, promAPI, query)
		if err != nil {
			return 0.0, fmt.Errorf("failed to query Prometheus for %s: %w", metricName, err)
		}
	}

	if warn != nil {
		logger.Log.Warn("Prometheus warnings", "metric", metricName, "warnings", warn)
	}

	// Check if the result type is a Vector
	if val.Type() != model.ValVector {
		logger.Log.Debug("Prometheus query returned non-vector type", "metric", metricName, "type", val.Type().String())
		return 0.0, nil
	}

	// Safe type assertion, already verified it's a Vector type above
	vec, ok := val.(model.Vector)
	if !ok {
		logger.Log.Warnf("Type assertion failed for Vector despite Type() check: metric=%s, type=%T", metricName, val)
		return 0.0, nil
	}
	resultVal := 0.0
	if len(vec) > 0 {
		resultVal = float64(vec[0].Value)
		// Handle NaN or Inf values
		FixValue(&resultVal)
	}

	return resultVal, nil
}

// MetricsValidationResult contains the result of metrics availability check
type MetricsValidationResult struct {
	Available bool
	Reason    string
	Message   string
}

// ValidateMetricsAvailability checks if vLLM metrics are available for the given model and namespace
// Returns a validation result with details about metric availability
func ValidateMetricsAvailability(ctx context.Context, promAPI promv1.API, modelName, namespace string) MetricsValidationResult {
	// Query for basic vLLM metric to validate scraping is working
	// Try with namespace label first (real vLLM), fall back to just model_name (vllme emulator)
	testQuery := fmt.Sprintf(`%s{model_name="%s",namespace="%s"}`, constants.VLLMNumRequestRunning, modelName, namespace)

	val, _, err := utils.QueryPrometheusWithBackoff(ctx, promAPI, testQuery)
	if err != nil {
		logger.Log.Error(err, "Error querying Prometheus for metrics validation",
			"model", modelName, "namespace", namespace)
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
		}
	}

	// Check if we got any results
	if val.Type() != model.ValVector {
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
			Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check ServiceMonitor configuration and ensure vLLM pods are exposing /metrics endpoint", modelName, namespace),
		}
	}

	// Safe type assertion, already verified it's a vector type above
	vec, ok := val.(model.Vector)
	if !ok {
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
			Message:   fmt.Sprintf("Unexpected result type for model '%s': expected Vector, got %T", modelName, val),
		}
	}
	// If no results with namespace label, try without it (for vllme emulator compatibility)
	if len(vec) == 0 {
		testQueryFallback := fmt.Sprintf(`%s{model_name="%s"}`, constants.VLLMNumRequestRunning, modelName)
		val, _, err = utils.QueryPrometheusWithBackoff(ctx, promAPI, testQueryFallback)
		if err != nil {
			return MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
			}
		}

		if val.Type() == model.ValVector {
			vec, ok = val.(model.Vector)
			if !ok {
				return MetricsValidationResult{
					Available: false,
					Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
					Message:   fmt.Sprintf("Unexpected result type for fallback query for model '%s': expected Vector, got %T", modelName, val),
				}
			}
		}

		// If still no results, metrics are truly missing
		if len(vec) == 0 {
			return MetricsValidationResult{
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
			return MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsStale,
				Message:   fmt.Sprintf("vLLM metrics for model '%s' are stale (last update: %v ago). ServiceMonitor may not be scraping correctly.", modelName, age),
			}
		}
	}

	return MetricsValidationResult{
		Available: true,
		Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
		Message:   "vLLM metrics are available and up-to-date",
	}
}

func AddMetricsToOptStatus(ctx context.Context,
	opt *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
	promAPI promv1.API) (llmdVariantAutoscalingV1alpha1.Allocation, error) {
	return AddMetricsToOptStatusWithCircuitBreaker(ctx, opt, deployment, acceleratorCostVal, promAPI, nil)
}

// AddMetricsToOptStatusWithCircuitBreaker collects metrics with optional circuit breaker protection.
// If circuitBreaker is nil, queries are executed without circuit breaker protection.
func AddMetricsToOptStatusWithCircuitBreaker(ctx context.Context,
	opt *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
	promAPI promv1.API,
	circuitBreaker *utils.PrometheusCircuitBreaker) (llmdVariantAutoscalingV1alpha1.Allocation, error) {

	deployNamespace := deployment.Namespace
	modelName := opt.Spec.ModelID

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

	// --- 2. Execute Queries in Parallel ---
	// Parallelize all 5 Prometheus queries to reduce latency

	type queryResults struct {
		arrivalVal      float64
		avgInputTokens  float64
		avgOutputTokens float64
		ttftAverageTime float64
		itlAverage      float64
		arrivalErr      error
		avgInputErr     error
		avgOutputErr    error
		ttftErr         error
		itlErr          error
	}

	results := &queryResults{}
	var wg sync.WaitGroup
	var mu sync.Mutex

	// Query 1: Arrival rate
	wg.Add(1)
	go func() {
		defer wg.Done()
		var val float64
		var err error
		// Check context cancellation before starting query
		if ctx.Err() != nil {
			err = fmt.Errorf("context cancelled: %w", ctx.Err())
		} else {
			val, err = queryAndExtractMetricWithCircuitBreaker(ctx, promAPI, arrivalQuery, "ArrivalRate", circuitBreaker)
			val = val * 60 // convert from req/sec to req/min
		}
		mu.Lock()
		results.arrivalVal = val
		results.arrivalErr = err
		mu.Unlock()
	}()

	// Query 2: Average input tokens
	wg.Add(1)
	go func() {
		defer wg.Done()
		var val float64
		var err error
		// Check context cancellation before starting query
		if ctx.Err() != nil {
			err = fmt.Errorf("context cancelled: %w", ctx.Err())
		} else {
			val, err = queryAndExtractMetricWithCircuitBreaker(ctx, promAPI, avgPromptToksQuery, "AvgInputTokens", circuitBreaker)
		}
		mu.Lock()
		results.avgInputTokens = val
		results.avgInputErr = err
		mu.Unlock()
	}()

	// Query 3: Average output tokens
	wg.Add(1)
	go func() {
		defer wg.Done()
		var val float64
		var err error
		// Check context cancellation before starting query
		if ctx.Err() != nil {
			err = fmt.Errorf("context cancelled: %w", ctx.Err())
		} else {
			val, err = queryAndExtractMetricWithCircuitBreaker(ctx, promAPI, avgDecToksQuery, "AvgOutputTokens", circuitBreaker)
		}
		mu.Lock()
		results.avgOutputTokens = val
		results.avgOutputErr = err
		mu.Unlock()
	}()

	// Query 4: Average TTFT
	wg.Add(1)
	go func() {
		defer wg.Done()
		var val float64
		var err error
		// Check context cancellation before starting query
		if ctx.Err() != nil {
			err = fmt.Errorf("context cancelled: %w", ctx.Err())
		} else {
			val, err = queryAndExtractMetricWithCircuitBreaker(ctx, promAPI, ttftQuery, "TTFTAverageTime", circuitBreaker)
			val = val * 1000 // convert to msec
		}
		mu.Lock()
		results.ttftAverageTime = val
		results.ttftErr = err
		mu.Unlock()
	}()

	// Query 5: Average ITL
	wg.Add(1)
	go func() {
		defer wg.Done()
		var val float64
		var err error
		// Check context cancellation before starting query
		if ctx.Err() != nil {
			err = fmt.Errorf("context cancelled: %w", ctx.Err())
		} else {
			val, err = queryAndExtractMetricWithCircuitBreaker(ctx, promAPI, itlQuery, "ITLAverage", circuitBreaker)
			val = val * 1000 // convert to msec
		}
		mu.Lock()
		results.itlAverage = val
		results.itlErr = err
		mu.Unlock()
	}()

	// Wait for all queries to complete
	wg.Wait()

	// Check for errors after all queries complete
	if results.arrivalErr != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, fmt.Errorf("failed to query arrival rate: %w", results.arrivalErr)
	}
	if results.avgInputErr != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, fmt.Errorf("failed to query avg input tokens: %w", results.avgInputErr)
	}
	if results.avgOutputErr != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, fmt.Errorf("failed to query avg output tokens: %w", results.avgOutputErr)
	}
	if results.ttftErr != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, fmt.Errorf("failed to query TTFT: %w", results.ttftErr)
	}
	if results.itlErr != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, fmt.Errorf("failed to query ITL: %w", results.itlErr)
	}

	// Extract results
	arrivalVal := results.arrivalVal
	avgInputTokens := results.avgInputTokens
	avgOutputTokens := results.avgOutputTokens
	ttftAverageTime := results.ttftAverageTime
	itlAverage := results.itlAverage

	// --- 3. Collect K8s and Static Info ---

	// number of replicas
	numReplicas := int(*deployment.Spec.Replicas)

	// accelerator type - strict validation required
	acc := ""
	if val, ok := opt.Labels["inference.optimization/acceleratorName"]; ok && val != "" {
		acc = val
	} else {
		return llmdVariantAutoscalingV1alpha1.Allocation{},
			fmt.Errorf("missing or empty acceleratorName label on VariantAutoscaling object: %s", opt.Name)
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
	return currentAlloc, nil
}

// Helper to handle if a value is NaN or infinite
func FixValue(x *float64) {
	if math.IsNaN(*x) || math.IsInf(*x, 0) {
		*x = 0
	}
}
