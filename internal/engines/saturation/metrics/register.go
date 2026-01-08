package metrics

import (
	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/v2"
)

// Query name constants for type-safe query references.
const (
	// Saturation queries (per-pod metrics)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"
)

// RegisterSaturationQueries registers queries used by the saturation analyzer.
func RegisterSaturationQueries(sourceRegistry *collector.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// KV cache usage per pod (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes
	registry.MustRegister(collector.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        collector.QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{collector.ParamNamespace, collector.ParamModelID},
		Description: "Peak KV cache utilization per pod (0.0-1.0) over last minute",
	})

	// Queue length per pod (peak over last minute)
	// Uses max_over_time to catch burst traffic
	registry.MustRegister(collector.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        collector.QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{collector.ParamNamespace, collector.ParamModelID},
		Description: "Peak queue length per pod over last minute",
	})

}
