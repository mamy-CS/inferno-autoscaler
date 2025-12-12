package prometheus

import (
	"context"
	"fmt"
	"math"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	"github.com/prometheus/common/model"
)

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
