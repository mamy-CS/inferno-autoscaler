package metrics

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d-incubation/inferno-autoscaler/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	replicaScalingTotal *prometheus.CounterVec
	desiredReplicas     *prometheus.GaugeVec
	currentReplicas     *prometheus.GaugeVec
)

// InitMetrics registers all custom metrics with the provided registry
func InitMetrics(registry prometheus.Registerer) error {
	replicaScalingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "inferno_replica_scaling_total",
			Help: "Total number of replica scaling operations",
		},
		[]string{"variant_name", "namespace", "direction", "reason"},
	)
	desiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "inferno_desired_replicas",
			Help: "Desired number of replicas for each variant",
		},
		[]string{"variant_name", "namespace", "accelerator_type"},
	)
	currentReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "inferno_current_replicas",
			Help: "Current number of replicas for each variant",
		},
		[]string{"variant_name", "namespace", "accelerator_type"},
	)

	// Register metrics with the registry
	if err := registry.Register(replicaScalingTotal); err != nil {
		return fmt.Errorf("failed to register replicaScalingTotal metric: %w", err)
	}
	if err := registry.Register(desiredReplicas); err != nil {
		return fmt.Errorf("failed to register desiredReplicas metric: %w", err)
	}
	if err := registry.Register(currentReplicas); err != nil {
		return fmt.Errorf("failed to register currentReplicas metric: %w", err)
	}
	return nil
}

// InitMetricsAndEmitter registers metrics with Prometheus and creates a metrics emitter
// This is a convenience function that handles both registration and emitter creation
func InitMetricsAndEmitter(registry prometheus.Registerer) (*MetricsEmitter, error) {
	if err := InitMetrics(registry); err != nil {
		return nil, err
	}
	return NewMetricsEmitter(), nil
}

// MetricsEmitter handles emission of custom metrics
type MetricsEmitter struct{}

// NewMetricsEmitter creates a new metrics emitter
func NewMetricsEmitter() *MetricsEmitter {
	return &MetricsEmitter{}
}

// EmitReplicaScalingMetrics emits metrics related to replica scaling
func (m *MetricsEmitter) EmitReplicaScalingMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, direction, reason string) error {
	labels := prometheus.Labels{
		"variant_name": va.Name,
		"namespace":    va.Namespace,
		"direction":    direction,
		"reason":       reason,
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if replicaScalingTotal == nil {
		return fmt.Errorf("replicaScalingTotal metric not initialized")
	}

	replicaScalingTotal.With(labels).Inc()
	return nil
}

// EmitReplicaMetrics emits current and desired replica metrics
func (m *MetricsEmitter) EmitReplicaMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, current, desired int32, acceleratorType string) error {
	baseLabels := prometheus.Labels{
		"variant_name":     va.Name,
		"namespace":        va.Namespace,
		"accelerator_type": acceleratorType,
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if currentReplicas == nil || desiredReplicas == nil {
		return fmt.Errorf("replica metrics not initialized")
	}

	currentReplicas.With(baseLabels).Set(float64(current))
	desiredReplicas.With(baseLabels).Set(float64(desired))
	return nil
}
