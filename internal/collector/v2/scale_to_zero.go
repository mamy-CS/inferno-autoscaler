// Package collector provides metrics collection functionality.
//
// This file provides scale-to-zero metrics collection using the v2 collector
// infrastructure with registered query templates.
package collector

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
)

// Query name constants for scale-to-zero metrics.
const (
	// QueryModelRequestCount is the query name for total model requests over a time window.
	QueryModelRequestCount = "model_request_count"

	// ParamRetentionPeriod is the parameter name for the retention period duration.
	ParamRetentionPeriod = "retentionPeriod"
)

// RegisterScaleToZeroQueries registers queries used for scale-to-zero decisions.
// This should be called during initialization to register query templates with the prometheus source.
func RegisterScaleToZeroQueries(sourceRegistry *SourceRegistry) {
	source := sourceRegistry.Get("prometheus")
	if source == nil {
		// Prometheus source not registered yet, skip registration
		return
	}

	registry := source.QueryList()

	// Model request count over a retention period
	// Uses sum(increase(...)) to get total requests over the time window
	// The retentionPeriod parameter should be in Prometheus duration format (e.g., "10m", "1h")
	registry.MustRegister(QueryTemplate{
		Name:        QueryModelRequestCount,
		Type:        QueryTypePromQL,
		Template:    `sum(increase(vllm:request_success_total{namespace="{{.namespace}}",model_name="{{.modelID}}"}[{{.retentionPeriod}}]))`,
		Params:      []string{ParamNamespace, ParamModelID, ParamRetentionPeriod},
		Description: "Total successful requests for a model over the retention period",
	})
}

// ScaleToZeroCollector collects metrics for scale-to-zero decisions.
type ScaleToZeroCollector struct {
	source MetricsSource
}

// NewScaleToZeroCollector creates a new scale-to-zero metrics collector.
func NewScaleToZeroCollector(source MetricsSource) *ScaleToZeroCollector {
	return &ScaleToZeroCollector{
		source: source,
	}
}

// CollectModelRequestCount collects the total number of successful requests for a model
// over the specified retention period. This is used for scale-to-zero decisions.
//
// Parameters:
//   - ctx: Context for the operation
//   - modelID: The model identifier
//   - namespace: The namespace where the model is deployed
//   - retentionPeriod: How far back to look for requests
//
// Returns:
//   - float64: Total request count over the retention period (0 if no requests or error)
//   - error: Any error that occurred during collection
func (c *ScaleToZeroCollector) CollectModelRequestCount(
	ctx context.Context,
	modelID string,
	namespace string,
	retentionPeriod time.Duration,
) (float64, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Convert Go duration to Prometheus duration format
	retentionPeriodStr := formatPrometheusDuration(retentionPeriod)

	params := map[string]string{
		ParamModelID:         modelID,
		ParamNamespace:       namespace,
		ParamRetentionPeriod: retentionPeriodStr,
	}

	// Execute the query
	results, err := c.source.Refresh(ctx, RefreshSpec{
		Queries: []string{QueryModelRequestCount},
		Params:  params,
	})
	if err != nil {
		logger.V(logging.DEBUG).Info("Failed to query model request count, returning 0",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr,
			"error", err)
		return 0, nil // Return 0 instead of error - no requests is valid
	}

	// Extract the result
	result := results[QueryModelRequestCount]
	if result == nil {
		logger.V(logging.DEBUG).Info("No result for model request count query, returning 0",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr)
		return 0, nil
	}

	if result.HasError() {
		logger.V(logging.DEBUG).Info("Model request count query failed, returning 0",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr,
			"error", result.Error)
		return 0, nil
	}

	// Get the first value (sum query returns a single scalar)
	if len(result.Values) == 0 {
		logger.V(logging.DEBUG).Info("No values in model request count result, returning 0",
			"model", modelID,
			"namespace", namespace,
			"retentionPeriod", retentionPeriodStr)
		return 0, nil
	}

	count := result.FirstValue().Value

	logger.V(logging.DEBUG).Info("Collected model request count",
		"model", modelID,
		"namespace", namespace,
		"retentionPeriod", retentionPeriodStr,
		"count", count)

	return count, nil
}

// formatPrometheusDuration converts a Go time.Duration to Prometheus duration format.
// Prometheus uses formats like "5m", "1h", "30s", "1d".
func formatPrometheusDuration(d time.Duration) string {
	// Handle days (Prometheus supports 'd' suffix)
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		days := d / (24 * time.Hour)
		return fmt.Sprintf("%dd", days)
	}

	// Handle hours
	if d >= time.Hour && d%time.Hour == 0 {
		hours := d / time.Hour
		return fmt.Sprintf("%dh", hours)
	}

	// Handle minutes
	if d >= time.Minute && d%time.Minute == 0 {
		minutes := d / time.Minute
		return fmt.Sprintf("%dm", minutes)
	}

	// Handle seconds
	if d >= time.Second && d%time.Second == 0 {
		seconds := d / time.Second
		return fmt.Sprintf("%ds", seconds)
	}

	// Default to seconds (round down)
	seconds := d / time.Second
	if seconds > 0 {
		return fmt.Sprintf("%ds", seconds)
	}

	// Very short durations, use milliseconds (Prometheus doesn't support ms, use minimum 1s)
	return "1s"
}
