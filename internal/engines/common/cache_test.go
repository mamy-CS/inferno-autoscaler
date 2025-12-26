package common

import (
	"sync"
	"testing"

	wvav1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestInternalDecisionCache(t *testing.T) {
	cache := &InternalDecisionCache{
		items: make(map[string]interfaces.VariantDecision),
	}

	// Test Set and Get
	decision := interfaces.VariantDecision{
		VariantName:     "test-variant",
		Namespace:       "test-ns",
		TargetReplicas:  5,
		AcceleratorName: "A100",
		Action:          interfaces.ActionScaleUp,
	}

	cache.Set("test-variant", "test-ns", decision)

	retrieved, ok := cache.Get("test-variant", "test-ns")
	if !ok {
		t.Error("Expected decision to be found in cache")
	}
	if retrieved.TargetReplicas != 5 {
		t.Errorf("Expected TargetReplicas 5, got %d", retrieved.TargetReplicas)
	}

	_, ok = cache.Get("non-existent", "test-ns")
	if ok {
		t.Error("Expected non-existent item to not be found")
	}

	// Test Concurrency
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Set("test-variant", "test-ns", decision)
			cache.Get("test-variant", "test-ns")
		}()
	}
	wg.Wait()
}

func TestGlobalConfig(t *testing.T) {
	config := &GlobalConfig{}

	// Test Optimization Config
	config.UpdateOptimizationConfig("60s")
	if config.GetOptimizationInterval() != "60s" {
		t.Errorf("Expected interval '60s', got '%s'", config.GetOptimizationInterval())
	}

	// Test Saturation Config
	satConfig := map[string]interfaces.SaturationScalingConfig{
		"default": {KvCacheThreshold: 0.8},
	}
	config.UpdateSaturationConfig(satConfig)
	retrievedConfig := config.GetSaturationConfig()
	if retrievedConfig["default"].KvCacheThreshold != 0.8 {
		t.Errorf("Expected threshold 0.8, got %f", retrievedConfig["default"].KvCacheThreshold)
	}

	// Test Concurrency
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			config.UpdateOptimizationConfig("30s")
			config.GetOptimizationInterval()
		}()
	}
	wg.Wait()
}

func TestVACache(t *testing.T) {
	// Reset global cache for testing (since it's a global variable)
	// Note: This might affect other parallel tests if any, but unit tests usually run sequentially pkg-wise
	vaCacheLocks := &vaCacheLock
	vaCacheLocks.Lock()
	originalCache := vaCache
	vaCache = make(map[client.ObjectKey]*wvav1alpha1.VariantAutoscaling)
	vaCacheLocks.Unlock()

	defer func() {
		vaCacheLocks.Lock()
		vaCache = originalCache
		vaCacheLocks.Unlock()
	}()

	va := &wvav1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-va",
			Namespace: "test-ns",
		},
	}

	// Test Update
	UpdateVACache(va)
	key := client.ObjectKey{Name: "test-va", Namespace: "test-ns"}

	// Verify via GetReadyVAs (implicitly tests Get)
	readyVAs := GetReadyVAs()
	if len(readyVAs) != 1 {
		t.Errorf("Expected 1 ready VA, got %d", len(readyVAs))
	} else if readyVAs[0].Name != "test-va" {
		t.Errorf("Expected VA name 'test-va', got '%s'", readyVAs[0].Name)
	}

	// Test Remove
	RemoveVACache(key)
	readyVAs = GetReadyVAs()
	if len(readyVAs) != 0 {
		t.Errorf("Expected 0 ready VAs, got %d", len(readyVAs))
	}

	// Test Deleted VAs are skipped in GetReadyVAs
	deletedVA := va.DeepCopy()
	now := metav1.Now()
	deletedVA.DeletionTimestamp = &now
	UpdateVACache(deletedVA)
	readyVAs = GetReadyVAs()
	if len(readyVAs) != 0 {
		t.Errorf("Expected 0 ready VAs (filtered deleted), got %d", len(readyVAs))
	}
}

func TestDecisionToOptimizedAlloc(t *testing.T) {
	d := interfaces.VariantDecision{
		TargetReplicas:  3,
		AcceleratorName: "H100",
	}

	replicas, acc, _ := DecisionToOptimizedAlloc(d)

	if replicas != 3 {
		t.Errorf("Expected 3 replicas, got %d", replicas)
	}
	if acc != "H100" {
		t.Errorf("Expected H100 accelerator, got %s", acc)
	}
}
