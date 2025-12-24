package saturation

import (
	"sync"
	"time"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	wvav1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
)

// InternalDecisionCache holds the latest saturation decisions for VAs.
// This is used to pass decisions from the Engine to the Controller without API server interaction.
type InternalDecisionCache struct {
	sync.RWMutex
	items map[string]interfaces.VariantDecision
}

// Key format: namespace/name
func cacheKey(name, namespace string) string {
	return namespace + "/" + name
}

func (c *InternalDecisionCache) Set(name, namespace string, d interfaces.VariantDecision) {
	c.Lock()
	defer c.Unlock()
	key := cacheKey(name, namespace)
	c.items[key] = d
}

func (c *InternalDecisionCache) Get(name, namespace string) (interfaces.VariantDecision, bool) {
	c.RLock()
	defer c.RUnlock()
	key := cacheKey(name, namespace)
	val, ok := c.items[key]
	return val, ok
}

// Global cache instance
var DecisionCache = &InternalDecisionCache{
	items: make(map[string]interfaces.VariantDecision),
}

// DecisionTrigger is a channel to trigger reconciliation for VAs.
// Buffered to prevent blocking the engine loop.
var DecisionTrigger = make(chan event.GenericEvent, 1000)

// Helper to convert VariantDecision to OptimizedAlloc status
func DecisionToOptimizedAlloc(d interfaces.VariantDecision) (int, string, metav1.Time) {
	// If LastRunTime is adding to VariantDecision, use it, else Now
	// For now we assume the consumer sets LastRunTime or uses Now
	return d.TargetReplicas, d.AcceleratorName, metav1.NewTime(time.Now())
}

// GlobalConfig holds the shared configuration for the autoscaler components.
type GlobalConfig struct {
	sync.RWMutex
	OptimizationInterval string
	SaturationConfig     map[string]interfaces.SaturationScalingConfig
}

// UpdateOptimizationConfig updates the optimization interval.
func (c *GlobalConfig) UpdateOptimizationConfig(interval string) {
	c.Lock()
	defer c.Unlock()
	c.OptimizationInterval = interval
}

// UpdateSaturationConfig updates the saturation scaling configuration.
func (c *GlobalConfig) UpdateSaturationConfig(config map[string]interfaces.SaturationScalingConfig) {
	c.Lock()
	defer c.Unlock()
	c.SaturationConfig = config
}

// GetOptimizationInterval returns the current optimization interval.
func (c *GlobalConfig) GetOptimizationInterval() string {
	c.RLock()
	defer c.RUnlock()
	return c.OptimizationInterval
}

// GetSaturationConfig returns the current saturation scaling configuration.
func (c *GlobalConfig) GetSaturationConfig() map[string]interfaces.SaturationScalingConfig {
	c.RLock()
	defer c.RUnlock()
	// Return a copy or just the map (caller should not modify it)
	// For efficiency, expecting caller to treat as read-only or we copy if needed.
	// Returning map directly for now as readers are expected to be well-behaved.
	return c.SaturationConfig
}

// TransformationConfig is the global singleton for configuration.
// (Using name TransformationConfig as a placeholder/legacy name if suitable, or just Config)
var Config = &GlobalConfig{}

// Global cache for VariantAutoscalings to avoid API server queries in Engine
var (
	vaCache     = make(map[client.ObjectKey]*wvav1alpha1.VariantAutoscaling)
	vaCacheLock sync.RWMutex
)

// UpdateVACache updates the global cache with a VariantAutoscaling.
func UpdateVACache(va *wvav1alpha1.VariantAutoscaling) {
	vaCacheLock.Lock()
	defer vaCacheLock.Unlock()
	key := client.ObjectKey{Name: va.Name, Namespace: va.Namespace}
	vaCache[key] = va.DeepCopy()
}

// RemoveVACache removes a VariantAutoscaling from the global cache.
func RemoveVACache(key client.ObjectKey) {
	vaCacheLock.Lock()
	defer vaCacheLock.Unlock()
	delete(vaCache, key)
}

// GetReadyVAs retrieves all VariantAutoscalings from the cache that are ready for optimization.
// This replaces readyVariantAutoscalings in utils, but without logging context or client dependency.
func GetReadyVAs() []wvav1alpha1.VariantAutoscaling {
	vaCacheLock.RLock()
	defer vaCacheLock.RUnlock()

	readyVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(vaCache))
	for _, va := range vaCache {
		// Skip deleted VAs
		if !va.DeletionTimestamp.IsZero() {
			continue
		}
		readyVAs = append(readyVAs, *va)
	}
	return readyVAs
}
