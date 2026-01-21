package pipeline

import (
	"context"
	"testing"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/discovery"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDiscovery implements discovery.CapacityDiscovery for testing.
type mockDiscovery struct {
	inventory map[string]map[string]discovery.AcceleratorModelInfo
	err       error
}

func (m *mockDiscovery) Discover(ctx context.Context) (map[string]map[string]discovery.AcceleratorModelInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.inventory, nil
}

// mockFullDiscovery implements discovery.FullDiscovery for testing.
type mockFullDiscovery struct {
	inventory map[string]map[string]discovery.AcceleratorModelInfo
	usage     map[string]int
	discErr   error
	usageErr  error
}

func (m *mockFullDiscovery) Discover(ctx context.Context) (map[string]map[string]discovery.AcceleratorModelInfo, error) {
	if m.discErr != nil {
		return nil, m.discErr
	}
	return m.inventory, nil
}

func (m *mockFullDiscovery) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	if m.usageErr != nil {
		return nil, m.usageErr
	}
	return m.usage, nil
}

func TestTypeInventory_Refresh(t *testing.T) {
	tests := []struct {
		name             string
		nodeInventory    map[string]map[string]discovery.AcceleratorModelInfo
		expectedLimits   map[string]int
		expectedTotal    int
		expectedAccTypes []string
	}{
		{
			name: "single node single type",
			nodeInventory: map[string]map[string]discovery.AcceleratorModelInfo{
				"node-1": {
					"H100": {Count: 8, Memory: "80GB"},
				},
			},
			expectedLimits:   map[string]int{"H100": 8},
			expectedTotal:    8,
			expectedAccTypes: []string{"H100"},
		},
		{
			name: "single node multiple types",
			nodeInventory: map[string]map[string]discovery.AcceleratorModelInfo{
				"node-1": {
					"H100": {Count: 4, Memory: "80GB"},
					"A100": {Count: 4, Memory: "40GB"},
				},
			},
			expectedLimits:   map[string]int{"H100": 4, "A100": 4},
			expectedTotal:    8,
			expectedAccTypes: []string{"H100", "A100"},
		},
		{
			name: "multiple nodes same type",
			nodeInventory: map[string]map[string]discovery.AcceleratorModelInfo{
				"node-1": {
					"H100": {Count: 8, Memory: "80GB"},
				},
				"node-2": {
					"H100": {Count: 8, Memory: "80GB"},
				},
			},
			expectedLimits:   map[string]int{"H100": 16},
			expectedTotal:    16,
			expectedAccTypes: []string{"H100"},
		},
		{
			name: "heterogeneous cluster",
			nodeInventory: map[string]map[string]discovery.AcceleratorModelInfo{
				"node-1": {
					"H100": {Count: 8, Memory: "80GB"},
				},
				"node-2": {
					"A100": {Count: 8, Memory: "40GB"},
				},
				"node-3": {
					"L40S": {Count: 4, Memory: "48GB"},
				},
			},
			expectedLimits:   map[string]int{"H100": 8, "A100": 8, "L40S": 4},
			expectedTotal:    20,
			expectedAccTypes: []string{"H100", "A100", "L40S"},
		},
		{
			name:             "empty cluster",
			nodeInventory:    map[string]map[string]discovery.AcceleratorModelInfo{},
			expectedLimits:   map[string]int{},
			expectedTotal:    0,
			expectedAccTypes: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disc := &mockDiscovery{inventory: tt.nodeInventory}
			inv := NewTypeInventory("test", disc)

			err := inv.Refresh(context.Background())
			require.NoError(t, err)

			// Check limits (capacity)
			assert.Equal(t, tt.expectedTotal, inv.TotalLimit(), "wrong total limit")

			for accType, expected := range tt.expectedLimits {
				assert.Equal(t, expected, inv.LimitByType(accType),
					"wrong limit for accelerator type %s", accType)
			}

			// With no usage set, available should equal limits
			assert.Equal(t, tt.expectedTotal, inv.TotalAvailable(), "available should equal limit when no usage")
			for accType, expected := range tt.expectedLimits {
				assert.Equal(t, expected, inv.AvailableByType(accType),
					"available should equal limit for type %s when no usage", accType)
			}

			accTypes := inv.AcceleratorTypes()
			assert.ElementsMatch(t, tt.expectedAccTypes, accTypes)
		})
	}
}

func TestTypeInventory_SetUsed(t *testing.T) {
	disc := &mockDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {"H100": {Count: 16, Memory: "80GB"}},
			"node-2": {"A100": {Count: 8, Memory: "40GB"}},
		},
	}

	inv := NewTypeInventory("test", disc)
	err := inv.Refresh(context.Background())
	require.NoError(t, err)

	// Initially: limit=24, used=0, available=24
	assert.Equal(t, 24, inv.TotalLimit())
	assert.Equal(t, 0, inv.TotalUsed())
	assert.Equal(t, 24, inv.TotalAvailable())

	// Set some usage
	inv.SetUsed(map[string]int{"H100": 4, "A100": 2})

	// Now: limit=24, used=6, available=18
	assert.Equal(t, 24, inv.TotalLimit())
	assert.Equal(t, 6, inv.TotalUsed())
	assert.Equal(t, 18, inv.TotalAvailable())

	// Per-type checks
	assert.Equal(t, 16, inv.LimitByType("H100"))
	assert.Equal(t, 4, inv.UsedByType("H100"))
	assert.Equal(t, 12, inv.AvailableByType("H100"))

	assert.Equal(t, 8, inv.LimitByType("A100"))
	assert.Equal(t, 2, inv.UsedByType("A100"))
	assert.Equal(t, 6, inv.AvailableByType("A100"))
}

func TestTypeInventory_OverAllocation(t *testing.T) {
	disc := &mockDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {"H100": {Count: 8, Memory: "80GB"}},
		},
	}

	inv := NewTypeInventory("test", disc)
	err := inv.Refresh(context.Background())
	require.NoError(t, err)

	// Set usage greater than limit (shouldn't happen but handle gracefully)
	inv.SetUsed(map[string]int{"H100": 12})

	// Available should be 0, not negative
	assert.Equal(t, 8, inv.TotalLimit())
	assert.Equal(t, 12, inv.TotalUsed())
	assert.Equal(t, 0, inv.TotalAvailable())
	assert.Equal(t, 0, inv.AvailableByType("H100"))
}

func TestTypeInventory_RefreshAll(t *testing.T) {
	disc := &mockFullDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {"H100": {Count: 16, Memory: "80GB"}},
			"node-2": {"A100": {Count: 8, Memory: "40GB"}},
		},
		usage: map[string]int{"H100": 4, "A100": 2},
	}

	inv := NewTypeInventoryWithUsage("test", disc)
	err := inv.RefreshAll(context.Background())
	require.NoError(t, err)

	// Check limits
	assert.Equal(t, 24, inv.TotalLimit())
	assert.Equal(t, 16, inv.LimitByType("H100"))
	assert.Equal(t, 8, inv.LimitByType("A100"))

	// Check usage (auto-discovered)
	assert.Equal(t, 6, inv.TotalUsed())
	assert.Equal(t, 4, inv.UsedByType("H100"))
	assert.Equal(t, 2, inv.UsedByType("A100"))

	// Check available
	assert.Equal(t, 18, inv.TotalAvailable())
	assert.Equal(t, 12, inv.AvailableByType("H100"))
	assert.Equal(t, 6, inv.AvailableByType("A100"))
}

func TestTypeInventory_RefreshAll_NoUsageDiscovery(t *testing.T) {
	disc := &mockDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {"H100": {Count: 8, Memory: "80GB"}},
		},
	}

	// Create without usage discovery
	inv := NewTypeInventory("test", disc)

	// RefreshAll should fail without usage discovery configured
	err := inv.RefreshAll(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "usage discovery not configured")
}

func TestTypeAllocator_TryAllocate(t *testing.T) {
	tests := []struct {
		name              string
		initialByType     map[string]int
		decision          *interfaces.VariantDecision
		gpusRequested     int
		expectedAllocated int
		expectedRemaining map[string]int
		expectError       bool
	}{
		{
			name:          "allocate from available pool",
			initialByType: map[string]int{"H100": 16, "A100": 8},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			},
			gpusRequested:     4,
			expectedAllocated: 4,
			expectedRemaining: map[string]int{"H100": 12, "A100": 8},
		},
		{
			name:          "allocate entire pool",
			initialByType: map[string]int{"H100": 8},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			},
			gpusRequested:     8,
			expectedAllocated: 8,
			expectedRemaining: map[string]int{"H100": 0},
		},
		{
			name:          "partial allocation when pool exhausted",
			initialByType: map[string]int{"H100": 4},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			},
			gpusRequested:     8,
			expectedAllocated: 4,
			expectedRemaining: map[string]int{"H100": 0},
		},
		{
			name:          "no allocation when type not available",
			initialByType: map[string]int{"A100": 8},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			},
			gpusRequested:     4,
			expectedAllocated: 0,
			expectedRemaining: map[string]int{"A100": 8},
		},
		{
			name:          "error when accelerator name not specified",
			initialByType: map[string]int{"H100": 8},
			decision: &interfaces.VariantDecision{
				VariantName: "model-a",
				Namespace:   "default",
				// AcceleratorName is empty
			},
			gpusRequested: 4,
			expectError:   true,
		},
		{
			name:          "zero request returns zero",
			initialByType: map[string]int{"H100": 8},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "H100",
			},
			gpusRequested:     0,
			expectedAllocated: 0,
			expectedRemaining: map[string]int{"H100": 8},
		},
		{
			name:          "types are isolated",
			initialByType: map[string]int{"H100": 4, "A100": 8},
			decision: &interfaces.VariantDecision{
				VariantName:     "model-a",
				Namespace:       "default",
				AcceleratorName: "A100",
			},
			gpusRequested:     6,
			expectedAllocated: 6,
			expectedRemaining: map[string]int{"H100": 4, "A100": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate initial total
			total := 0
			for _, count := range tt.initialByType {
				total += count
			}

			allocator := &typeAllocator{
				remainingByType: copyMap(tt.initialByType),
				totalRemaining:  total,
			}

			allocated, err := allocator.TryAllocate(tt.decision, tt.gpusRequested)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectedAllocated, allocated)

			for accType, expected := range tt.expectedRemaining {
				assert.Equal(t, expected, allocator.RemainingForType(accType),
					"wrong remaining for type %s", accType)
			}
		})
	}
}

func TestTypeAllocator_MultipleAllocations(t *testing.T) {
	allocator := &typeAllocator{
		remainingByType: map[string]int{"H100": 16, "A100": 8},
		totalRemaining:  24,
	}

	// First allocation: 4 H100 GPUs
	allocated, err := allocator.TryAllocate(&interfaces.VariantDecision{
		VariantName:     "model-a",
		Namespace:       "default",
		AcceleratorName: "H100",
	}, 4)
	require.NoError(t, err)
	assert.Equal(t, 4, allocated)
	assert.Equal(t, 12, allocator.RemainingForType("H100"))
	assert.Equal(t, 8, allocator.RemainingForType("A100"))
	assert.Equal(t, 20, allocator.Remaining())

	// Second allocation: 6 A100 GPUs
	allocated, err = allocator.TryAllocate(&interfaces.VariantDecision{
		VariantName:     "model-b",
		Namespace:       "default",
		AcceleratorName: "A100",
	}, 6)
	require.NoError(t, err)
	assert.Equal(t, 6, allocated)
	assert.Equal(t, 12, allocator.RemainingForType("H100"))
	assert.Equal(t, 2, allocator.RemainingForType("A100"))
	assert.Equal(t, 14, allocator.Remaining())

	// Third allocation: more H100 than available
	allocated, err = allocator.TryAllocate(&interfaces.VariantDecision{
		VariantName:     "model-c",
		Namespace:       "default",
		AcceleratorName: "H100",
	}, 20)
	require.NoError(t, err)
	assert.Equal(t, 12, allocated) // Only 12 remaining
	assert.Equal(t, 0, allocator.RemainingForType("H100"))
	assert.Equal(t, 2, allocator.RemainingForType("A100"))
	assert.Equal(t, 2, allocator.Remaining())
}

func TestTypeInventory_CreateAllocator(t *testing.T) {
	disc := &mockDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {
				"H100": {Count: 16, Memory: "80GB"},
			},
			"node-2": {
				"A100": {Count: 8, Memory: "40GB"},
			},
		},
	}

	inv := NewTypeInventory("test", disc)
	err := inv.Refresh(context.Background())
	require.NoError(t, err)

	// Set current usage
	inv.SetUsed(map[string]int{"H100": 4, "A100": 4})

	// Verify inventory state: limit=24, used=8, available=16
	assert.Equal(t, 24, inv.TotalLimit())
	assert.Equal(t, 8, inv.TotalUsed())
	assert.Equal(t, 16, inv.TotalAvailable())

	// Create allocator - should get available (limit - used)
	allocator := inv.CreateAllocator(context.Background())

	// Allocator should have available GPUs (not limits)
	assert.Equal(t, 16, allocator.Remaining()) // H100: 16-4=12, A100: 8-4=4

	// Allocate from H100 pool
	allocated, err := allocator.TryAllocate(&interfaces.VariantDecision{
		VariantName:     "model-a",
		Namespace:       "default",
		AcceleratorName: "H100",
	}, 4)
	require.NoError(t, err)
	assert.Equal(t, 4, allocated)
	assert.Equal(t, 12, allocator.Remaining()) // 16 - 4 = 12

	// Original inventory should be unchanged
	assert.Equal(t, 16, inv.TotalAvailable())
	assert.Equal(t, 12, inv.AvailableByType("H100"))
}

func TestTypeInventory_CreateAllocatorWithUsage(t *testing.T) {
	disc := &mockDiscovery{
		inventory: map[string]map[string]discovery.AcceleratorModelInfo{
			"node-1": {"H100": {Count: 8, Memory: "80GB"}},
		},
	}

	inv := NewTypeInventory("test", disc)
	err := inv.Refresh(context.Background())
	require.NoError(t, err)

	// Set high usage - only 2 GPUs available
	inv.SetUsed(map[string]int{"H100": 6})

	allocator := inv.CreateAllocator(context.Background())
	assert.Equal(t, 2, allocator.Remaining())

	// Request more than available - should get partial allocation
	allocated, err := allocator.TryAllocate(&interfaces.VariantDecision{
		VariantName:     "model-a",
		Namespace:       "default",
		AcceleratorName: "H100",
	}, 4)
	require.NoError(t, err)
	assert.Equal(t, 2, allocated) // Only 2 available
	assert.Equal(t, 0, allocator.Remaining())
}

// copyMap creates a copy of a map[string]int
func copyMap(m map[string]int) map[string]int {
	result := make(map[string]int, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}
