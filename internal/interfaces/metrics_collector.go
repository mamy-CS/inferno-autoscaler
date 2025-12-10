package interfaces

import (
	"context"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
)

// MetricsValidationResult contains the result of metrics availability check
type MetricsValidationResult struct {
	Available bool
	Reason    string
	Message   string
}

// MetricsCollector defines the interface for collecting metrics from various backends.
// Implementations can collect metrics from Prometheus, EPP, or other backends.
type MetricsCollector interface {
	// ValidateMetricsAvailability checks if metrics are available for the given model and namespace.
	// Returns a validation result with details about metric availability.
	ValidateMetricsAvailability(
		ctx context.Context,
		modelName string,
		namespace string,
	) MetricsValidationResult

	// AddMetricsToOptStatus collects metrics for optimization and populates the Allocation status.
	// This is used by the model-based optimizer to gather current metrics for a variant.
	// Returns the current allocation with metrics populated.
	AddMetricsToOptStatus(
		ctx context.Context,
		va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
		deployment appsv1.Deployment,
		acceleratorCostVal float64,
	) (llmdVariantAutoscalingV1alpha1.Allocation, error)

	// CollectReplicaMetrics collects capacity-related metrics for all replicas of a model.
	// This is used by the saturation analyzer to gather KV cache usage and queue length metrics.
	// Returns a list of replica metrics, one per pod.
	CollectReplicaMetrics(
		ctx context.Context,
		modelID string,
		namespace string,
		deployments map[string]*appsv1.Deployment,
		variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
		variantCosts map[string]float64,
	) ([]ReplicaMetrics, error)
}
