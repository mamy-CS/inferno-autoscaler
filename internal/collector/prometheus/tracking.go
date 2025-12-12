package prometheus

import (
	"fmt"
	"sync"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	appsv1 "k8s.io/api/apps/v1"
)

// TrackedVA holds information about a VA that should be fetched in background
type TrackedVA struct {
	mu              sync.Mutex // Protects LastFetch and future mutable fields
	ModelID         string
	Namespace       string
	VariantName     string
	VA              *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	Deployment      appsv1.Deployment
	AcceleratorCost float64
	LastFetch       time.Time
}

// needsFetch checks if the VA needs fetching based on the fetch interval (thread-safe)
func (t *TrackedVA) needsFetch(fetchInterval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.LastFetch.IsZero() {
		return true
	}
	return time.Since(t.LastFetch) >= fetchInterval
}

// setLastFetch updates the last fetch time (thread-safe)
func (t *TrackedVA) setLastFetch(tm time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.LastFetch = tm
}

// TrackedModel holds information about a model that should have replica metrics fetched in background
type TrackedModel struct {
	mu        sync.Mutex // Protects LastFetch and future mutable fields
	ModelID   string
	Namespace string
	LastFetch time.Time
}

// needsFetch checks if the model needs fetching based on the fetch interval (thread-safe)
func (t *TrackedModel) needsFetch(fetchInterval time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.LastFetch.IsZero() {
		return true
	}
	return time.Since(t.LastFetch) >= fetchInterval
}

// setLastFetch updates the last fetch time (thread-safe)
func (t *TrackedModel) setLastFetch(tm time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.LastFetch = tm
}

// TrackVA registers a VA for background fetching
func (pc *PrometheusCollector) TrackVA(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, deployment appsv1.Deployment, acceleratorCost float64) {
	key := fmt.Sprintf("%s/%s/%s", va.Spec.ModelID, va.Namespace, va.Name)
	tracked := &TrackedVA{
		ModelID:         va.Spec.ModelID,
		Namespace:       va.Namespace,
		VariantName:     va.Name,
		VA:              va,
		Deployment:      deployment,
		AcceleratorCost: acceleratorCost,
		// LastFetch initialized to zero value (never fetched)
	}
	pc.trackedVAs.Store(key, tracked)
	logger.Log.Debugw("Tracking VA for background fetching", "key", key)
}

// UntrackVA removes a VA from background fetching
func (pc *PrometheusCollector) UntrackVA(modelID, namespace, variantName string) {
	key := fmt.Sprintf("%s/%s/%s", modelID, namespace, variantName)
	pc.trackedVAs.Delete(key)
	logger.Log.Debugw("Untracked VA from background fetching", "key", key)
}

// TrackModel registers a model for background replica metrics fetching
func (pc *PrometheusCollector) TrackModel(modelID, namespace string) {
	key := fmt.Sprintf("%s/%s", modelID, namespace)
	tracked := &TrackedModel{
		ModelID:   modelID,
		Namespace: namespace,
		// LastFetch initialized to zero value (never fetched)
	}
	pc.trackedModels.Store(key, tracked)
	logger.Log.Debugw("Tracking model for background replica metrics fetching", "key", key)
}

// UntrackModel removes a model from background replica metrics fetching
func (pc *PrometheusCollector) UntrackModel(modelID, namespace string) {
	key := fmt.Sprintf("%s/%s", modelID, namespace)
	pc.trackedModels.Delete(key)
	logger.Log.Debugw("Untracked model from background replica metrics fetching", "key", key)
}
