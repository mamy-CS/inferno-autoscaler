package config

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
)

// TestConfig_ThreadSafeUpdates tests that concurrent reads and writes to DynamicConfig
// are thread-safe and don't cause race conditions or data corruption.
func TestConfig_ThreadSafeUpdates(t *testing.T) {
	cfg := NewTestConfig()
	cfg.Dynamic.OptimizationInterval = 30 * time.Second

	const (
		numReaders = 10
		numWriters = 5
		iterations = 100
	)

	var (
		readErrors  int64
		writeErrors int64
		wg          sync.WaitGroup
	)

	// Spawn reader goroutines that continuously read from DynamicConfig
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Read various dynamic config values
				interval := cfg.GetOptimizationInterval()
				if interval <= 0 {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Invalid interval at iteration %d: %v", readerID, j, interval)
					continue
				}

				satConfig := cfg.GetSaturationConfig()
				if satConfig == nil {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Nil saturation config at iteration %d", readerID, j)
					continue
				}

				scaleToZeroConfig := cfg.GetScaleToZeroConfig()
				if scaleToZeroConfig == nil {
					atomic.AddInt64(&readErrors, 1)
					t.Logf("Reader %d: Nil scale-to-zero config at iteration %d", readerID, j)
					continue
				}

				// Small sleep to increase chance of concurrent access
				time.Sleep(time.Microsecond)
			}
		}(i)
	}

	// Spawn writer goroutines that continuously update DynamicConfig
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Update optimization interval
				newInterval := time.Duration(30+j) * time.Second
				cfg.UpdateOptimizationInterval(newInterval)

				// Update saturation config
				newSatConfig := make(map[string]interfaces.SaturationScalingConfig)
				newSatConfig["test-accelerator"] = interfaces.SaturationScalingConfig{
					KvCacheThreshold:     0.8,
					QueueLengthThreshold: 5,
					KvSpareTrigger:       0.1,
					QueueSpareTrigger:    3,
				}
				cfg.UpdateSaturationConfig(newSatConfig)

				// Update scale-to-zero config
				newScaleToZeroConfig := make(ScaleToZeroConfigData)
				enabled := true
				newScaleToZeroConfig["model1"] = ModelScaleToZeroConfig{
					ModelID:           "model1",
					EnableScaleToZero: &enabled,
					RetentionPeriod:   "5m",
				}
				cfg.UpdateScaleToZeroConfig(newScaleToZeroConfig)

				// Small sleep to increase chance of concurrent access
				time.Sleep(time.Microsecond)
			}
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Verify no errors occurred
	assert.Equal(t, int64(0), readErrors, "No read errors should occur in thread-safe access")
	assert.Equal(t, int64(0), writeErrors, "No write errors should occur in thread-safe updates")

	// Verify final state is consistent
	finalInterval := cfg.GetOptimizationInterval()
	assert.Greater(t, finalInterval, time.Duration(0), "Final interval should be positive")

	finalSatConfig := cfg.GetSaturationConfig()
	assert.NotNil(t, finalSatConfig, "Final saturation config should not be nil")

	finalScaleToZeroConfig := cfg.GetScaleToZeroConfig()
	assert.NotNil(t, finalScaleToZeroConfig, "Final scale-to-zero config should not be nil")
}

// TestConfig_ThreadSafeConcurrentReads tests that multiple concurrent reads don't block each other.
func TestConfig_ThreadSafeConcurrentReads(t *testing.T) {
	cfg := NewTestConfig()
	cfg.Dynamic.OptimizationInterval = 60 * time.Second

	const numReaders = 50
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Spawn many reader goroutines
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // Wait for signal to start
			// All readers should be able to read concurrently (RWMutex allows multiple readers)
			interval := cfg.GetOptimizationInterval()
			satConfig := cfg.GetSaturationConfig()
			scaleToZeroConfig := cfg.GetScaleToZeroConfig()
			_ = interval
			_ = satConfig
			_ = scaleToZeroConfig
		}()
	}

	// Start all readers at once
	close(start)

	// Measure time - should be fast since RWMutex allows concurrent reads
	startTime := time.Now()
	wg.Wait()
	duration := time.Since(startTime)

	// Concurrent reads should complete quickly (much faster than sequential)
	// If reads were blocking each other, this would take much longer
	assert.Less(t, duration, 100*time.Millisecond, "Concurrent reads should complete quickly")
}

// TestConfig_ThreadSafeReadDuringWrite tests that reads can still happen during writes,
// but writes are serialized.
func TestConfig_ThreadSafeReadDuringWrite(t *testing.T) {
	cfg := NewTestConfig()
	cfg.Dynamic.OptimizationInterval = 30 * time.Second

	var (
		readCount  int64
		writeCount int64
		wg         sync.WaitGroup
		done       = make(chan struct{})
	)

	// Writer goroutine that continuously updates
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			cfg.UpdateOptimizationInterval(time.Duration(30+i) * time.Second)
			atomic.AddInt64(&writeCount, 1)
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()

	// Reader goroutines that read while writes are happening
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					interval := cfg.GetOptimizationInterval()
					if interval > 0 {
						atomic.AddInt64(&readCount, 1)
					}
					time.Sleep(time.Microsecond)
				}
			}
		}()
	}

	wg.Wait()

	// Verify both reads and writes happened
	assert.Greater(t, atomic.LoadInt64(&readCount), int64(0), "Should have performed reads")
	assert.Equal(t, int64(100), atomic.LoadInt64(&writeCount), "Should have performed all writes")
}

// TestDetectImmutableParameterChanges tests that attempts to change immutable parameters
// are correctly detected.
func TestDetectImmutableParameterChanges(t *testing.T) {
	// Create initial config with Prometheus URL
	initialConfig := NewTestConfig()
	initialConfig.Static.Prometheus = &interfaces.PrometheusConfig{
		BaseURL: "https://prometheus-initial:9090",
	}

	tests := []struct {
		name        string
		configMap   map[string]string
		expectError bool
		errorMsg    string
	}{
		{
			name: "No immutable parameter change",
			configMap: map[string]string{
				"GLOBAL_OPT_INTERVAL": "120s",
			},
			expectError: false,
		},
		{
			name: "Attempt to change PROMETHEUS_BASE_URL",
			configMap: map[string]string{
				"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
		{
			name: "Attempt to change METRICS_ADDR",
			configMap: map[string]string{
				"METRICS_ADDR": ":9443",
			},
			expectError: true,
			errorMsg:    "METRICS_ADDR",
		},
		{
			name: "Attempt to change PROBE_ADDR",
			configMap: map[string]string{
				"PROBE_ADDR": ":8082",
			},
			expectError: true,
			errorMsg:    "PROBE_ADDR",
		},
		{
			name: "Attempt to change LEADER_ELECTION_ID",
			configMap: map[string]string{
				"LEADER_ELECTION_ID": "new-election-id",
			},
			expectError: true,
			errorMsg:    "LEADER_ELECTION_ID",
		},
		{
			name: "Multiple immutable parameter changes",
			configMap: map[string]string{
				"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
				"METRICS_ADDR":         ":9443",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
		{
			name: "Mixed mutable and immutable changes",
			configMap: map[string]string{
				"GLOBAL_OPT_INTERVAL":  "120s",
				"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
			},
			expectError: true,
			errorMsg:    "PROMETHEUS_BASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes, err := DetectImmutableParameterChanges(initialConfig, tt.configMap)
			if tt.expectError {
				require.Error(t, err, "Should detect immutable parameter change")
				require.NotEmpty(t, changes, "Should return list of changed immutable parameters")
				// Verify the expected key is in the changes list
				found := false
				for _, change := range changes {
					if change.Key == tt.errorMsg {
						found = true
						break
					}
				}
				assert.True(t, found, "Expected key %q should be in the changes list", tt.errorMsg)
				// Verify all detected changes are in the ConfigMap
				for _, change := range changes {
					assert.Contains(t, tt.configMap, change.Key, "Detected change should be in ConfigMap")
				}
			} else {
				require.NoError(t, err, "Should not detect immutable parameter change")
				assert.Empty(t, changes, "Should return empty list when no immutable changes")
			}
		})
	}
}

// TestDetectImmutableParameterChanges_NoInitialConfig tests detection when initial config
// doesn't have certain fields set.
func TestDetectImmutableParameterChanges_NoInitialConfig(t *testing.T) {
	initialConfig := NewTestConfig()
	// Don't set Prometheus config initially

	// Attempting to set PROMETHEUS_BASE_URL when it wasn't set initially
	// This is actually allowed during initial load, but not during runtime updates
	// For runtime updates, we should detect it as a change attempt
	configMap := map[string]string{
		"PROMETHEUS_BASE_URL": "https://prometheus-new:9090",
	}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	// If Prometheus wasn't set initially, setting it via ConfigMap is a change attempt
	// (though this would typically be caught at startup, not runtime)
	require.Error(t, err, "Should detect change attempt even if initial value was not set")
	assert.NotEmpty(t, changes, "Should return list of changed immutable parameters")
}

// TestDetectImmutableParameterChanges_EmptyConfigMap tests that empty ConfigMap doesn't trigger errors.
func TestDetectImmutableParameterChanges_EmptyConfigMap(t *testing.T) {
	initialConfig := NewTestConfig()
	initialConfig.Static.Prometheus = &interfaces.PrometheusConfig{
		BaseURL: "https://prometheus:9090",
	}

	configMap := map[string]string{}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	require.NoError(t, err, "Empty ConfigMap should not trigger errors")
	assert.Empty(t, changes, "Should return empty list for empty ConfigMap")
}

// TestDetectImmutableParameterChanges_OnlyMutable tests that only mutable parameters don't trigger errors.
func TestDetectImmutableParameterChanges_OnlyMutable(t *testing.T) {
	initialConfig := NewTestConfig()
	initialConfig.Static.Prometheus = &interfaces.PrometheusConfig{
		BaseURL: "https://prometheus:9090",
	}

	// Only mutable parameters
	configMap := map[string]string{
		"GLOBAL_OPT_INTERVAL": "120s",
	}

	changes, err := DetectImmutableParameterChanges(initialConfig, configMap)
	require.NoError(t, err, "Mutable parameters should not trigger errors")
	assert.Empty(t, changes, "Should return empty list for mutable parameters only")
}
