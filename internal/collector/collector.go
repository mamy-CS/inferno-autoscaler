// Package collector provides metrics collection functionality.
//
// The collector package provides a pluggable metrics collection system with support for
// multiple backends (Prometheus, EPP). Use factory.NewMetricsCollector() to create collector
// instances, and the MetricsCollector interface from internal/interfaces to interact with them.
//
// Note: Some legacy functions in this package (ValidateMetricsAvailability, AddMetricsToOptStatus)
// are deprecated. See individual function documentation for details.
package collector

// This file contains deprecated compatibility functions that delegate to the new
// MetricsCollector interface. These functions are kept for backward compatibility
// but should not be used in new code.

import (
	"context"
	"math"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
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

// MetricsValidationResult contains the result of metrics availability check
// Deprecated: Use interfaces.MetricsValidationResult instead
type MetricsValidationResult = interfaces.MetricsValidationResult

// ValidateMetricsAvailability checks if vLLM metrics are available for the given model and namespace
// Returns a validation result with details about metric availability
//
// Deprecated: This function is maintained for backward compatibility.
// New code should use MetricsCollector interface via interfaces.MetricsCollector.
// This function delegates to PrometheusCollector for backward compatibility.
func ValidateMetricsAvailability(ctx context.Context, promAPI promv1.API, modelName, namespace string) MetricsValidationResult {
	// Use factory to create collector to avoid import cycle
	pc, err := NewMetricsCollector(Config{
		Type:    CollectorTypePrometheus,
		PromAPI: promAPI,
	})
	if err != nil {
		return MetricsValidationResult{
			Available: false,
			Reason:    "Error",
			Message:   err.Error(),
		}
	}
	return pc.ValidateMetricsAvailability(ctx, modelName, namespace)
}

// AddMetricsToOptStatus collects metrics for optimization and populates the Allocation status.
//
// Deprecated: This function is maintained for backward compatibility.
// New code should use MetricsCollector interface via interfaces.MetricsCollector.
// This function delegates to PrometheusCollector for backward compatibility.
func AddMetricsToOptStatus(ctx context.Context,
	opt *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
	promAPI promv1.API) (llmdVariantAutoscalingV1alpha1.Allocation, error) {
	// Use factory to create collector to avoid import cycle
	pc, err := NewMetricsCollector(Config{
		Type:    CollectorTypePrometheus,
		PromAPI: promAPI,
	})
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	// Get raw metrics from collector
	metrics, err := pc.AddMetricsToOptStatus(ctx, opt, deployment, acceleratorCostVal)
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	// Build Allocation from metrics (using utils to avoid import cycle)
	return utils.BuildAllocationFromMetrics(metrics, opt, deployment, acceleratorCostVal)
}

// Helper to handle if a value is NaN or infinite
func FixValue(x *float64) {
	if math.IsNaN(*x) || math.IsInf(*x, 0) {
		*x = 0
	}
}
