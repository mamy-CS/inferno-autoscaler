package registration

import (
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source"
)

// Query name constants for type-safe query references.
const (
	// Saturation queries (per-pod peak metrics over time windows)
	QueryKvCacheUsage = "kv_cache_usage"
	QueryQueueLength  = "queue_length"

	// V2 queries (token-based capacity analysis)
	QueryCacheConfigInfo = "cache_config_info"
	QueryAvgOutputTokens = "avg_output_tokens"
	QueryAvgInputTokens  = "avg_input_tokens"
)

// RegisterSaturationQueries registers queries used by the saturation analyzer.
func RegisterSaturationQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// KV cache usage per pod (peak over last minute)
	// Uses max_over_time to catch saturation events between scrapes
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryKvCacheUsage,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Peak KV cache utilization per pod (0.0-1.0) over last minute",
	})

	// Queue length per pod (peak over last minute)
	// Uses max_over_time to catch burst traffic
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryQueueLength,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod) (max_over_time(vllm:num_requests_waiting{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Peak queue length per pod over last minute",
	})

	// --- V2 queries for token-based capacity analysis ---

	// Cache config info per pod (static labels with block size and GPU blocks count)
	// Uses max to deduplicate when multiple series exist per pod with different label combinations
	// Used by Saturation Analyzer V2 for token capacity computation
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryCacheConfigInfo,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod, num_gpu_blocks, block_size) (vllm:cache_config_info{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "KV cache configuration info per pod (num_gpu_blocks and block_size as labels)",
	})

	// Average output (generation) tokens per completed request
	// Used for output-length-dependent k2 estimation
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgOutputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod) (rate(vllm:request_generation_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]) / rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average output tokens per completed request (5m rate)",
	})

	// Average input (prompt) tokens per completed request
	// Used in k2 derivation formula: k2 = N_max × (I + O/2)
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryAvgInputTokens,
		Type:        source.QueryTypePromQL,
		Template:    `max by (pod) (rate(vllm:request_prompt_tokens_sum{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]) / rate(vllm:request_prompt_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[5m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Average input tokens per completed request (5m rate)",
	})

}
