/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

func TestPrometheusCircuitBreaker_StateTransitions(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	config := CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          100 * time.Millisecond,
		HalfOpenTimeout:  50 * time.Millisecond,
	}

	cb := NewPrometheusCircuitBreaker(mockProm, config)

	// Initially closed
	assert.Equal(t, StateClosed, cb.GetState())
	assert.Equal(t, "closed", cb.GetStateString())

	// Test query with success
	query := "test_query"
	mockProm.QueryResults[query] = model.Vector{
		&model.Sample{Value: model.SampleValue(42)},
	}

	ctx := context.Background()
	result, warnings, err := cb.QueryPrometheus(ctx, query)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Nil(t, warnings)
	assert.Equal(t, StateClosed, cb.GetState())
}

func TestPrometheusCircuitBreaker_OpensOnFailures(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	config := CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Timeout:          100 * time.Millisecond,
		HalfOpenTimeout:  50 * time.Millisecond,
	}

	cb := NewPrometheusCircuitBreaker(mockProm, config)

	query := "test_query"
	mockProm.QueryErrors[query] = errors.New("prometheus error")

	ctx := context.Background()

	// Fail 3 times to open the circuit
	for i := 0; i < 3; i++ {
		_, _, err := cb.QueryPrometheus(ctx, query)
		assert.Error(t, err)
	}

	// Circuit should be open now
	assert.Equal(t, StateOpen, cb.GetState())
	assert.Equal(t, "open", cb.GetStateString())

	// Next query should fail immediately with circuit breaker error
	_, _, err := cb.QueryPrometheus(ctx, query)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrCircuitBreakerOpen))
}

func TestPrometheusCircuitBreaker_HalfOpenRecovery(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	config := CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 2,
		Timeout:          50 * time.Millisecond, // Short timeout for testing
		HalfOpenTimeout:  10 * time.Millisecond,
	}

	cb := NewPrometheusCircuitBreaker(mockProm, config)

	query := "test_query"
	mockProm.QueryErrors[query] = errors.New("prometheus error")

	ctx := context.Background()

	// Open the circuit
	for i := 0; i < 2; i++ {
		_, _, err := cb.QueryPrometheus(ctx, query)
		assert.Error(t, err)
	}
	assert.Equal(t, StateOpen, cb.GetState())

	// Wait for timeout to transition to half-open
	time.Sleep(60 * time.Millisecond)

	// Now set up success responses, delete the error entry and set result
	delete(mockProm.QueryErrors, query)
	mockProm.QueryResults[query] = model.Vector{
		&model.Sample{Value: model.SampleValue(42)},
	}

	// First successful query in half-open state
	result, warnings, err := cb.QueryPrometheus(ctx, query)
	require.NoError(t, err, "First query in half-open should succeed")
	assert.NotNil(t, result, "Result should not be nil")
	assert.Nil(t, warnings)
	assert.Equal(t, StateHalfOpen, cb.GetState(), "Should still be in half-open after first success")

	// Second successful query should close the circuit
	result, warnings, err = cb.QueryPrometheus(ctx, query)
	require.NoError(t, err, "Second query in half-open should succeed")
	assert.NotNil(t, result, "Result should not be nil")
	assert.Nil(t, warnings)
	assert.Equal(t, StateClosed, cb.GetState(), "Circuit should be closed after success threshold")
}

func TestPrometheusCircuitBreaker_HalfOpenFails(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	config := CircuitBreakerConfig{
		FailureThreshold: 2,
		SuccessThreshold: 2,
		Timeout:          50 * time.Millisecond,
		HalfOpenTimeout:  10 * time.Millisecond,
	}

	cb := NewPrometheusCircuitBreaker(mockProm, config)

	query := "test_query"
	mockProm.QueryErrors[query] = errors.New("prometheus error")

	ctx := context.Background()

	// Open the circuit
	for i := 0; i < 2; i++ {
		_, _, err := cb.QueryPrometheus(ctx, query)
		assert.Error(t, err)
	}
	assert.Equal(t, StateOpen, cb.GetState())

	// Wait for timeout
	time.Sleep(60 * time.Millisecond)

	// Failure in half-open should immediately reopen
	_, _, err := cb.QueryPrometheus(ctx, query)
	assert.Error(t, err)
	assert.Equal(t, StateOpen, cb.GetState())
}

func TestPrometheusCircuitBreaker_DefaultConfig(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	config := DefaultCircuitBreakerConfig()
	cb := NewPrometheusCircuitBreaker(mockProm, config)

	assert.Equal(t, 5, cb.failureThreshold)
	assert.Equal(t, 2, cb.successThreshold)
	assert.Equal(t, 30*time.Second, cb.timeout)
	assert.Equal(t, 5*time.Second, cb.halfOpenTimeout)
}

func TestPrometheusCircuitBreaker_InvalidConfigDefaults(t *testing.T) {
	t.Parallel()

	mockProm := &testutils.MockPromAPI{
		QueryResults: make(map[string]model.Value),
		QueryErrors:  make(map[string]error),
	}

	// Test with invalid config values
	config := CircuitBreakerConfig{
		FailureThreshold: 0, // Invalid, should default
		SuccessThreshold: 0, // Invalid, should default
		Timeout:          0, // Invalid, should default
		HalfOpenTimeout:  0, // Invalid, should default
	}

	cb := NewPrometheusCircuitBreaker(mockProm, config)

	// Should use defaults
	assert.Equal(t, 5, cb.failureThreshold)
	assert.Equal(t, 2, cb.successThreshold)
	assert.Equal(t, 30*time.Second, cb.timeout)
	assert.Equal(t, 5*time.Second, cb.halfOpenTimeout)
}
