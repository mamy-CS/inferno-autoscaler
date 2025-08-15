package actuator

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d-incubation/inferno-autoscaler/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d-incubation/inferno-autoscaler/internal/logger"
	"github.com/llm-d-incubation/inferno-autoscaler/internal/metrics"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Actuator struct {
	Client         client.Client
	MetricsEmitter *metrics.MetricsEmitter
}

func NewActuator(k8sClient client.Client) *Actuator {
	return &Actuator{
		Client:         k8sClient,
		MetricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// getCurrentDeploymentReplicas gets the real current replica count from the actual Deployment
func (a *Actuator) getCurrentDeploymentReplicas(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling) (int32, error) {
	var deploy appsv1.Deployment
	err := a.Client.Get(ctx, types.NamespacedName{
		Name:      va.Name,
		Namespace: va.Namespace,
	}, &deploy)
	if err != nil {
		return 0, fmt.Errorf("failed to get Deployment %s/%s: %w", va.Namespace, va.Name, err)
	}

	// Prefer status replicas (actual current state)
	if deploy.Status.Replicas > 0 {
		return deploy.Status.Replicas, nil
	}

	// Fallback to spec if status not ready
	if deploy.Spec.Replicas != nil {
		return *deploy.Spec.Replicas, nil
	}

	// Final fallback
	return 1, nil
}

func (a *Actuator) EmitMetrics(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	// Emit replica metrics with real-time data for external autoscalers
	if VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas > 0 {

		// Get real current replicas from Deployment (not stale VariantAutoscaling status)
		currentReplicas, err := a.getCurrentDeploymentReplicas(ctx, VariantAutoscaling)
		if err != nil {
			logger.Log.Warn("Could not get current deployment replicas, using VariantAutoscaling status",
				"error", err, "variant", VariantAutoscaling.Name)
			currentReplicas = int32(VariantAutoscaling.Status.CurrentAlloc.NumReplicas) // fallback
		}

		if err := a.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			VariantAutoscaling,
			currentReplicas, // Real current from Deployment
			int32(VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas), // Inferno's optimization target
			VariantAutoscaling.Status.DesiredOptimizedAlloc.Accelerator,
		); err != nil {
			logger.Log.Error(err, "Failed to emit optimization signals for external autoscalers",
				"variant", VariantAutoscaling.Name)
			// Don't fail the reconciliation for metric emission errors
			// Metrics are critical for external autoscalers, but emission failures shouldn't break core functionality
		} else {
			logger.Log.Debug("EmitReplicaMetrics completed successfully", "variant", VariantAutoscaling.Name)
		}
	} else {
		logger.Log.Debug("Skipping EmitReplicaMetrics - NumReplicas is 0", "variant", VariantAutoscaling.Name)
	}

	logger.Log.Info("Emitted optimization signals for external autoscaler consumption",
		"variant", VariantAutoscaling.Name, "namespace", VariantAutoscaling.Namespace)
	return nil
}
