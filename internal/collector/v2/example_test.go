// Package collector provides metrics collection functionality.
//
// This file contains examples demonstrating how to use the query registry
// and PrometheusSource for metrics collection.
package collector

import (
	"context"
	"fmt"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// Example_registerQuery demonstrates how to register queries with a source.
func Example_registerQuery() {
	// Create a Prometheus API client
	client, _ := promapi.NewClient(promapi.Config{
		Address: "http://prometheus:9090",
	})
	api := promv1.NewAPI(client)

	// Create a PrometheusSource
	source := NewPrometheusSource(api, PrometheusSourceConfig{
		DefaultTTL:   30 * time.Second,
		QueryTimeout: 10 * time.Second,
	})

	// Register queries with this source's registry
	registry := source.QueryRegistry()
	registry.MustRegister(QueryTemplate{
		Name:        "my_kv_cache_usage",
		Type:        QueryTypePromQL,
		Template:    `max by (pod) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}", model_name="{{.modelID}}"})`,
		Params:      []string{"namespace", "modelID"},
		Description: "KV cache utilization per pod",
	})

	registry.MustRegister(QueryTemplate{
		Name:        "custom_metric",
		Type:        QueryTypePromQL,
		Template:    `rate(requests_total{service="{{.service}}"}[5m])`,
		Params:      []string{"service"},
		Description: "Request rate per service",
	})

	// Build a query string with parameters
	query, err := registry.Build("my_kv_cache_usage", map[string]string{
		"namespace": "llm-serving",
		"modelID":   "llama-7b",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(query)
	// Output: max by (pod) (vllm:kv_cache_usage_perc{namespace="llm-serving", model_name="llama-7b"})
}

// Example_prometheusSource demonstrates how to use PrometheusSource to collect metrics.
func Example_prometheusSource() {
	// Create Prometheus API client
	client, err := promapi.NewClient(promapi.Config{
		Address: "http://prometheus:9090",
	})
	if err != nil {
		panic(err)
	}
	api := promv1.NewAPI(client)

	// Create the source with configuration
	source := NewPrometheusSource(api, PrometheusSourceConfig{
		DefaultTTL:   30 * time.Second, // Cache results for 30 seconds
		QueryTimeout: 10 * time.Second, // Timeout per query
	})

	// Register standard WVA queries
	RegisterStandardQueries(source)

	ctx := context.Background()

	// Refresh all registered queries with parameters
	results, err := source.Refresh(ctx, RefreshSpec{
		Params: map[string]string{
			"namespace": "llm-serving",
			"modelID":   "llama-7b",
		},
	})
	if err != nil {
		panic(err)
	}

	// Process results
	for queryName, result := range results {
		if result.HasError() {
			fmt.Printf("Query %s failed: %v\n", queryName, result.Error)
			continue
		}

		fmt.Printf("Query %s returned %d values:\n", queryName, len(result.Values))
		for _, value := range result.Values {
			fmt.Printf("  - pod=%s: %.2f (sampled at %s)\n",
				value.Labels["pod"],
				value.Value,
				value.Timestamp.Format(time.RFC3339))
		}
	}
}

// Example_cachedAccess demonstrates how to access cached values.
func Example_cachedAccess() {
	// Assuming source is already created and Refresh() was called...
	var source *PrometheusSource // = NewPrometheusSource(...)

	// The cache key is constructed from query name + params
	params := map[string]string{
		"namespace": "llm-serving",
		"modelID":   "llama-7b",
	}

	// Get a specific cached value (must provide same params used during Refresh)
	cached := source.Get("kv_cache_usage", params)
	if cached == nil {
		fmt.Println("Not in cache or expired")
		return
	}

	// Check if cache entry is expired (based on TTL)
	if cached.IsExpired() {
		fmt.Println("Cache entry expired, need to refresh")
		return
	}

	// Check staleness based on metric timestamp (not cache TTL)
	stalenessThreshold := 60 * time.Second
	if cached.Result.IsStale(stalenessThreshold) {
		fmt.Println("Metric data is stale (sample timestamp too old)")
	}

	// Access the first value (useful for scalar queries)
	first := cached.Result.FirstValue()
	fmt.Printf("KV Cache Usage: %.2f%% (age: %s)\n",
		first.Value*100,
		first.Age())

	// Iterate over all values (useful for vector queries)
	for _, v := range cached.Result.Values {
		fmt.Printf("Pod %s: %.2f%% (stale: %v)\n",
			v.Labels["pod"],
			v.Value*100,
			v.IsStale(stalenessThreshold))
	}
}

// Example_selectiveRefresh demonstrates how to refresh only specific queries.
func Example_selectiveRefresh() {
	var source *PrometheusSource // = NewPrometheusSource(...)
	ctx := context.Background()

	// Refresh only specific queries (not all registered ones)
	results, _ := source.Refresh(ctx, RefreshSpec{
		Queries: []string{"kv_cache_usage", "queue_length"}, // Only these two
		Params: map[string]string{
			"namespace": "llm-serving",
			"modelID":   "llama-7b",
		},
	})

	// Check results
	for name, result := range results {
		fmt.Printf("%s: %d values, error=%v\n", name, len(result.Values), result.Error)
	}
}

// Example_invalidateCache demonstrates how to invalidate cached values.
func Example_invalidateCache() {
	var source *PrometheusSource // = NewPrometheusSource(...)

	// Params used during Refresh (cache key includes params)
	params := map[string]string{
		"namespace": "llm-serving",
		"modelID":   "llama-7b",
	}

	// Invalidate a specific query with specific params
	source.Invalidate("kv_cache_usage", params)

	// Invalidate a query with no params (or nil)
	source.Invalidate("simple_metric", nil)

	// Invalidate all cached values (all queries, all params)
	source.InvalidateAll()
}

// Example_initTimeRegistration shows the recommended pattern for registering
// queries at application startup.
func Example_initTimeRegistration() {
	// Register standard queries when creating a source:
	//
	// source := NewPrometheusSource(api, config)
	// RegisterStandardQueries(source)
	//
	// Or register custom queries:
	//
	// registry := source.QueryRegistry()
	// registry.MustRegister(QueryTemplate{
	//     Name:     "arrival_rate",
	//     Type:     QueryTypePromQL,
	//     Template: `sum(rate(vllm:request_success_total{model_name="{{.modelID}}"}[1m]))`,
	//     Params:   []string{"modelID"},
	// })
	//
	// This ensures queries are registered before any code tries to use them.
	// MustRegister panics on error, which is appropriate for init-time registration
	// since registration errors are programming mistakes, not runtime errors.
}
