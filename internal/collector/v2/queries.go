// Package collector provides metrics collection functionality.
//
// This file defines standard Prometheus queries used by WVA.
// Queries are registered per-source at creation time.
package collector

// Query name constants for type-safe query references.
const (
	// Optimizer queries (aggregated metrics across all pods)
	QueryArrivalRate     = "arrival_rate"
	QueryAvgPromptTokens = "avg_prompt_tokens"
	QueryAvgDecodeTokens = "avg_decode_tokens"
	QueryTTFT            = "ttft"
	QueryITL             = "itl"

	// Validation queries
	QueryMetricsValidation         = "metrics_validation"
	QueryMetricsValidationFallback = "metrics_validation_fallback"

	// Saturation queries (per-pod metrics)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"
	QueryPodExistence = "pod_existence"
)

// Common parameter names used across queries.
const (
	ParamNamespace = "namespace"
	ParamModelID   = "modelID"
	ParamPodFilter = "podFilter" // Optional regex filter for pod names
)

// RegisterStandardQueries registers all standard WVA queries to a PrometheusSource.
// Call this after creating a PrometheusSource to enable standard metrics collection.
func RegisterStandardQueries(source *PrometheusSource) {
	registry := source.QueryRegistry()
	registerOptimizerQueries(registry)
	registerSaturationQueries(registry)
}

// registerOptimizerQueries registers queries used by the model-based optimizer.
// These queries return aggregated metrics across all pods of a model.
func registerOptimizerQueries(registry *QueryRegistry) {
	// Arrival rate: requests per second
	registry.MustRegister(QueryTemplate{
		Name:        QueryArrivalRate,
		Type:        QueryTypePromQL,
		Template:    `sum(rate(vllm:request_success_total{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{ParamModelID, ParamNamespace},
		Description: "Request arrival rate (requests/second) for a model",
	})

	// Average prompt (input) tokens per request
	registry.MustRegister(QueryTemplate{
		Name:        QueryAvgPromptTokens,
		Type:        QueryTypePromQL,
		Template:    `sum(rate(vllm:request_prompt_tokens_sum{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m])) / sum(rate(vllm:request_prompt_tokens_count{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{ParamModelID, ParamNamespace},
		Description: "Average input tokens per request",
	})

	// Average decode (output) tokens per request
	registry.MustRegister(QueryTemplate{
		Name:        QueryAvgDecodeTokens,
		Type:        QueryTypePromQL,
		Template:    `sum(rate(vllm:request_generation_tokens_sum{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m])) / sum(rate(vllm:request_generation_tokens_count{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{ParamModelID, ParamNamespace},
		Description: "Average output tokens per request",
	})

	// Time to first token (TTFT) in seconds
	registry.MustRegister(QueryTemplate{
		Name:        QueryTTFT,
		Type:        QueryTypePromQL,
		Template:    `sum(rate(vllm:time_to_first_token_seconds_sum{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m])) / sum(rate(vllm:time_to_first_token_seconds_count{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{ParamModelID, ParamNamespace},
		Description: "Average time to first token (seconds)",
	})

	// Inter-token latency (ITL) in seconds
	registry.MustRegister(QueryTemplate{
		Name:        QueryITL,
		Type:        QueryTypePromQL,
		Template:    `sum(rate(vllm:time_per_output_token_seconds_sum{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m])) / sum(rate(vllm:time_per_output_token_seconds_count{model_name="{{.modelID}}",namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{ParamModelID, ParamNamespace},
		Description: "Average inter-token latency (seconds)",
	})
}

// registerSaturationQueries registers queries used by the saturation analyzer.
// These queries return per-pod metrics using max_over_time for conservative analysis.
func registerSaturationQueries(registry *QueryRegistry) {
	// KV cache usage per pod (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes
	registry.MustRegister(QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{ParamNamespace, ParamModelID},
		Description: "Peak KV cache utilization per pod (0.0-1.0) over last minute",
	})

	// Queue length per pod (peak over last minute)
	// Uses max_over_time to catch burst traffic
	registry.MustRegister(QueryTemplate{
		Name:        QueryQueueLength,
		Type:        QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{ParamNamespace, ParamModelID},
		Description: "Peak queue length per pod over last minute",
	})

	// Pod existence check using kube-state-metrics
	// Used to filter out stale metrics from terminated pods
	// podFilter is optional - pass empty string to query all pods in namespace
	registry.MustRegister(QueryTemplate{
		Name:        QueryPodExistence,
		Type:        QueryTypePromQL,
		Template:    `kube_pod_info{namespace="{{.namespace}}"{{.podFilter}}}`,
		Params:      []string{ParamNamespace, ParamPodFilter},
		Description: "Current pods in namespace (from kube-state-metrics)",
	})
}
