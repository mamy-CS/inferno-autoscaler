// Package pod provides the Pod scraping metrics source implementation.
//
// This package implements the PodScrapingSource that scrapes metrics directly
// from EPP pods via HTTP requests to their /metrics endpoints.
package pod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/common/expfmt"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
)

// PodScrapingSourceConfig contains configuration for pod scraping.
type PodScrapingSourceConfig struct {
	// InferencePool identification
	InferencePoolName      string
	InferencePoolNamespace string

	// Service discovery
	// The scale-from-zero engine should provide ServiceName explicitly.
	// If empty, falls back to pattern-based discovery: {poolName}-epp
	ServiceName string

	// Metrics endpoint (provided by client/engine)
	MetricsPort   int32  // provided by client
	MetricsPath   string // provided by client, default: "/metrics"
	MetricsScheme string // provided by client, default: "http"

	// Authentication
	MetricsReaderSecretName string // default: "inference-gateway-sa-metrics-reader-secret"
	MetricsReaderSecretKey  string // default: "token"
	BearerToken             string // optional: explicit token override

	// Scraping behavior
	ScrapeTimeout        time.Duration // default: 5s per pod
	MaxConcurrentScrapes int           // default: 10

	// Cache configuration
	DefaultTTL time.Duration // default: 30s
}

// DefaultPodScrapingSourceConfig returns sensible defaults.
func DefaultPodScrapingSourceConfig() PodScrapingSourceConfig {
	return PodScrapingSourceConfig{
		MetricsPath:             "/metrics",
		MetricsScheme:           "http",
		MetricsReaderSecretName: "inference-gateway-sa-metrics-reader-secret",
		MetricsReaderSecretKey:  "token",
		ScrapeTimeout:           5 * time.Second,
		MaxConcurrentScrapes:    10,
		DefaultTTL:              30 * time.Second,
	}
}

// PodScrapingSource implements MetricsSource for direct EPP pod scraping.
type PodScrapingSource struct {
	config     PodScrapingSourceConfig
	k8sClient  client.Client
	httpClient *http.Client
	registry   *source.QueryList

	mu    sync.RWMutex // protects the cache and refresh operations
	cache *source.Cache
}

// NewPodScrapingSource creates a new PodScrapingSource for an InferencePool.
func NewPodScrapingSource(
	ctx context.Context,
	k8sClient client.Client,
	config PodScrapingSourceConfig,
) (*PodScrapingSource, error) {
	// Auto-discover service name if not provided
	if config.ServiceName == "" {
		config.ServiceName = discoverServiceName(config.InferencePoolName)
	}

	// Set defaults
	if config.MetricsPath == "" {
		config.MetricsPath = "/metrics"
	}
	if config.MetricsScheme == "" {
		config.MetricsScheme = "http"
	}
	if config.MetricsReaderSecretName == "" {
		config.MetricsReaderSecretName = "inference-gateway-sa-metrics-reader-secret"
	}
	if config.MetricsReaderSecretKey == "" {
		config.MetricsReaderSecretKey = "token"
	}
	if config.ScrapeTimeout == 0 {
		config.ScrapeTimeout = 5 * time.Second
	}
	if config.MaxConcurrentScrapes == 0 {
		config.MaxConcurrentScrapes = 10
	}
	if config.DefaultTTL == 0 {
		config.DefaultTTL = 30 * time.Second
	}

	// Build HTTP client
	httpClient := &http.Client{
		Timeout: config.ScrapeTimeout,
	}

	podSource := &PodScrapingSource{
		config:     config,
		k8sClient:  k8sClient,
		httpClient: httpClient,
		registry:   source.NewQueryList(),
		cache:      source.NewCache(ctx, config.DefaultTTL, 1*time.Second),
	}

	// Register default query
	podSource.registry.MustRegister(source.QueryTemplate{
		Name:        "all_metrics",
		Type:        source.QueryTypeMetricName,
		Template:    "all_metrics",
		Params:      []string{},
		Description: "All metrics from EPP pods",
	})

	return podSource, nil
}

// discoverServiceName derives service name from InferencePool name.
// NOTE: This is a fallback mechanism. The scale-from-zero engine should provide
// the ServiceName directly in PodScrapingSourceConfig. This pattern-based
// discovery is only used if ServiceName is not explicitly set by the engine.
func discoverServiceName(inferencePoolName string) string {
	return fmt.Sprintf("%s-epp", inferencePoolName)
}

// QueryList returns the query registry for this source.
func (p *PodScrapingSource) QueryList() *source.QueryList {
	return p.registry
}

// Refresh executes queries and updates the cache.
// Called by engine/reconciler on-demand.
func (p *PodScrapingSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	logger := ctrl.LoggerFrom(ctx)

	// Discover pods
	pods, err := p.discoverPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover pods: %w", err)
	}

	if len(pods) == 0 {
		logger.V(logging.DEBUG).Info("No ready pods found for scraping")
		return map[string]*source.MetricResult{
			"all_metrics": {
				QueryName:   "all_metrics",
				Values:      []source.MetricValue{},
				CollectedAt: time.Now(),
			},
		}, nil
	}

	// Scrape all pods concurrently
	results := p.scrapeAllPods(ctx, pods)

	// Aggregate results
	aggregated := p.aggregateResults(results)

	// Cache the result
	cacheKey := source.BuildCacheKey("all_metrics", nil)
	p.cache.Set(cacheKey, *aggregated, p.config.DefaultTTL)

	logger.V(logging.DEBUG).Info("Scraped metrics from pods",
		"podCount", len(pods),
		"successCount", len(results),
		"metricCount", len(aggregated.Values))

	return map[string]*source.MetricResult{
		"all_metrics": aggregated,
	}, nil
}

// Get retrieves cached metrics.
func (p *PodScrapingSource) Get(queryName string, params map[string]string) *source.CachedValue {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cacheKey := source.BuildCacheKey(queryName, params)
	cached, ok := p.cache.Get(cacheKey)
	if !ok || cached.IsExpired() {
		return nil
	}

	return cached
}

// discoverPods finds all Ready pods for the EPP service.
func (p *PodScrapingSource) discoverPods(ctx context.Context) ([]corev1.Pod, error) {
	// Get Service
	svc := &corev1.Service{}
	svcKey := types.NamespacedName{
		Name:      p.config.ServiceName,
		Namespace: p.config.InferencePoolNamespace,
	}
	if err := p.k8sClient.Get(ctx, svcKey, svc); err != nil {
		return nil, fmt.Errorf("failed to get service %s: %w", svcKey, err)
	}

	// List pods using Service selector
	podList := &corev1.PodList{}
	selector := labels.SelectorFromSet(svc.Spec.Selector)
	if err := p.k8sClient.List(ctx, podList, &client.ListOptions{
		Namespace:     p.config.InferencePoolNamespace,
		LabelSelector: selector,
	}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Filter to Ready pods only
	readyPods := []corev1.Pod{}
	for _, pod := range podList.Items {
		if isPodReady(&pod) {
			readyPods = append(readyPods, pod)
		}
	}

	return readyPods, nil
}

// isPodReady checks if pod is in Ready condition.
func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// scrapeAllPods scrapes metrics from all pods concurrently.
func (p *PodScrapingSource) scrapeAllPods(ctx context.Context, pods []corev1.Pod) map[string]*source.MetricResult {
	logger := ctrl.LoggerFrom(ctx)
	results := make(map[string]*source.MetricResult)
	var resultsMu sync.Mutex

	// Semaphore for concurrency control
	sem := make(chan struct{}, p.config.MaxConcurrentScrapes)
	var wg sync.WaitGroup

	for _, pod := range pods {
		wg.Add(1)
		go func(pod corev1.Pod) {
			defer wg.Done()

			sem <- struct{}{}        // Acquire
			defer func() { <-sem }() // Release

			result, err := p.scrapePodMetrics(ctx, &pod)
			if err != nil {
				logger.V(logging.VERBOSE).Error(err, "Failed to scrape pod",
					"pod", pod.Name)
				return
			}

			resultsMu.Lock()
			results[pod.Name] = result
			resultsMu.Unlock()
		}(pod)
	}

	wg.Wait()
	return results
}

// scrapePodMetrics scrapes metrics from a single pod.
func (p *PodScrapingSource) scrapePodMetrics(ctx context.Context, pod *corev1.Pod) (*source.MetricResult, error) {
	// Build URL: {scheme}://{podIP}:{port}{path}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s has no IP address", pod.Name)
	}

	url := fmt.Sprintf("%s://%s:%d%s",
		p.config.MetricsScheme,
		pod.Status.PodIP,
		p.config.MetricsPort,
		p.config.MetricsPath,
	)

	// Create request with timeout
	reqCtx, cancel := context.WithTimeout(ctx, p.config.ScrapeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add authentication header
	token, err := p.getAuthToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get auth token: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Execute request
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to scrape pod %s: %w", pod.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pod %s returned status %d", pod.Name, resp.StatusCode)
	}

	// Parse Prometheus text format
	return p.parsePrometheusMetrics(resp.Body, pod.Name)
}

// getAuthToken retrieves the authentication token.
func (p *PodScrapingSource) getAuthToken(ctx context.Context) (string, error) {
	// If explicit token provided, use it
	if p.config.BearerToken != "" {
		return p.config.BearerToken, nil
	}

	// Read from secret
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      p.config.MetricsReaderSecretName,
		Namespace: p.config.InferencePoolNamespace,
	}

	if err := p.k8sClient.Get(ctx, secretKey, secret); err != nil {
		return "", fmt.Errorf("failed to get metrics reader secret: %w", err)
	}

	tokenBytes, ok := secret.Data[p.config.MetricsReaderSecretKey]
	if !ok {
		return "", fmt.Errorf("token key %q not found in secret", p.config.MetricsReaderSecretKey)
	}

	return string(tokenBytes), nil
}

// parsePrometheusMetrics parses Prometheus text format into MetricResult.
func (p *PodScrapingSource) parsePrometheusMetrics(reader io.Reader, podName string) (*source.MetricResult, error) {
	var parser expfmt.TextParser
	metricFamilies, err := parser.TextToMetricFamilies(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metrics: %w", err)
	}

	// Convert to source.MetricValue array
	values := []source.MetricValue{}
	now := time.Now()

	for name, family := range metricFamilies {
		for _, metric := range family.Metric {
			labels := make(map[string]string)
			labels["pod"] = podName // Add pod label

			// Extract metric labels
			for _, labelPair := range metric.Label {
				if labelPair.Name != nil && labelPair.Value != nil {
					labels[*labelPair.Name] = *labelPair.Value
				}
			}

			// Extract metric value
			var value float64
			switch {
			case metric.Gauge != nil:
				value = metric.Gauge.GetValue()
			case metric.Counter != nil:
				value = metric.Counter.GetValue()
			case metric.Histogram != nil:
				value = float64(metric.Histogram.GetSampleCount())
			case metric.Summary != nil:
				value = float64(metric.Summary.GetSampleCount())
			case metric.Untyped != nil:
				value = metric.Untyped.GetValue()
			}

			// Add metric name as label for identification
			labels["__name__"] = name

			values = append(values, source.MetricValue{
				Value:     value,
				Timestamp: now, // Use current time as scrape timestamp
				Labels:    labels,
			})
		}
	}

	return &source.MetricResult{
		QueryName:   "all_metrics",
		Values:      values,
		CollectedAt: now,
	}, nil
}

// aggregateResults combines metrics from all pods.
func (p *PodScrapingSource) aggregateResults(results map[string]*source.MetricResult) *source.MetricResult {
	allValues := []source.MetricValue{}
	var latestCollectedAt time.Time

	for _, result := range results {
		if result == nil {
			continue
		}

		// Add all values (already have pod label)
		allValues = append(allValues, result.Values...)

		if result.CollectedAt.After(latestCollectedAt) {
			latestCollectedAt = result.CollectedAt
		}
	}

	if latestCollectedAt.IsZero() {
		latestCollectedAt = time.Now()
	}

	return &source.MetricResult{
		QueryName:   "all_metrics",
		Values:      allValues,
		CollectedAt: latestCollectedAt,
	}
}
