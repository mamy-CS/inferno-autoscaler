/*
Copyright 2023, 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	llmdOptv1alpha1 "github.com/llm-d-incubation/inferno-autoscaler/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

type PrometheusActuator struct {
	desiredReplicasGauge    *prometheus.GaugeVec
	currentReplicasGauge    *prometheus.GaugeVec
	optimizationStatusGauge *prometheus.GaugeVec
}

var (
	pa = &PrometheusActuator{
		desiredReplicasGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inferno_desired_replicas",
				Help: "Desired number of replicas for the inference model",
			},
			[]string{"model", "namespace"},
		),
		currentReplicasGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inferno_current_replicas",
				Help: "Current number of replicas for the inference model",
			},
			[]string{"model", "namespace"},
		),
		optimizationStatusGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "inferno_optimization_status",
				Help: "Optimization status: 1 if applied successfully, 0 otherwise",
			},
			[]string{"model", "namespace"},
		),
	}
)

// Register metrics
func RegisterMetrics() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(pa.desiredReplicasGauge, pa.currentReplicasGauge, pa.optimizationStatusGauge)
}

func (a *PrometheusActuator) ApplyReplicaTargets(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	// This actuator does not modify replicas directly
	logger := log.FromContext(ctx)
	logger.Info("PrometheusActuator does not apply replica targets directly")
	return nil
}

func NewPrometheusActuator() *PrometheusActuator {
	return pa
}

func (a *PrometheusActuator) EmitMetrics(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	logger := log.FromContext(ctx)

	model := VariantAutoscaling.Spec.ModelID
	namespace := VariantAutoscaling.Namespace

	desired := float64(VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas)
	current := float64(VariantAutoscaling.Status.CurrentAlloc.NumReplicas)
	applied := 0.0
	if VariantAutoscaling.Status.Actuation.Applied {
		applied = 1.0
	}

	a.desiredReplicasGauge.WithLabelValues(model, namespace).Set(desired)
	a.currentReplicasGauge.WithLabelValues(model, namespace).Set(current)
	a.optimizationStatusGauge.WithLabelValues(model, namespace).Set(applied)

	logger.Info("Emitted Prometheus metrics", "model", model, "namespace", namespace,
		"desiredReplicas", desired, "currentReplicas", current, "applied", applied)

	return nil
}
