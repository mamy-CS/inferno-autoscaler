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
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

var (
	// ErrCircuitBreakerOpen is returned when the circuit breaker is open
	ErrCircuitBreakerOpen = errors.New("circuit breaker is open: Prometheus is unavailable")
)

// CircuitBreakerState represents the state of the circuit breaker
type CircuitBreakerState int

const (
	// StateClosed means the circuit is closed and requests are allowed
	StateClosed CircuitBreakerState = iota
	// StateOpen means the circuit is open and requests are blocked
	StateOpen
	// StateHalfOpen means the circuit is half-open and testing if the service recovered
	StateHalfOpen
)

// PrometheusCircuitBreaker implements a circuit breaker pattern for Prometheus queries.
// It prevents cascade failures when Prometheus is unavailable by blocking requests
// after a threshold of failures, and allowing periodic attempts to test recovery.
type PrometheusCircuitBreaker struct {
	// Configuration
	failureThreshold int           // Number of consecutive failures before opening
	successThreshold int           // Number of consecutive successes in half-open to close
	timeout          time.Duration // Duration to keep circuit open before attempting half-open
	halfOpenTimeout  time.Duration // Timeout for half-open test requests

	// State
	mu                  sync.RWMutex
	state               CircuitBreakerState
	failureCount        int
	successCount        int
	lastFailureTime     time.Time
	lastStateChangeTime time.Time

	// Prometheus API wrapper
	promAPI promv1.API
}

// CircuitBreakerConfig holds configuration for the circuit breaker
type CircuitBreakerConfig struct {
	FailureThreshold int           // Number of consecutive failures before opening (default: 5)
	SuccessThreshold int           // Number of consecutive successes in half-open to close (default: 2)
	Timeout          time.Duration // Duration to keep circuit open before attempting half-open (default: 30s)
	HalfOpenTimeout  time.Duration // Timeout for half-open test requests (default: 5s)
}

// DefaultCircuitBreakerConfig returns a default circuit breaker configuration
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		Timeout:          30 * time.Second,
		HalfOpenTimeout:  5 * time.Second,
	}
}

// NewPrometheusCircuitBreaker creates a new circuit breaker for Prometheus
func NewPrometheusCircuitBreaker(promAPI promv1.API, config CircuitBreakerConfig) *PrometheusCircuitBreaker {
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = 5
	}
	if config.SuccessThreshold <= 0 {
		config.SuccessThreshold = 2
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.HalfOpenTimeout <= 0 {
		config.HalfOpenTimeout = 5 * time.Second
	}

	return &PrometheusCircuitBreaker{
		failureThreshold:    config.FailureThreshold,
		successThreshold:    config.SuccessThreshold,
		timeout:             config.Timeout,
		halfOpenTimeout:     config.HalfOpenTimeout,
		state:               StateClosed,
		promAPI:             promAPI,
		lastStateChangeTime: time.Now(),
	}
}

// QueryPrometheus executes a Prometheus query with circuit breaker protection.
// Returns the query result or an error if the circuit is open or the query fails.
// This method wraps QueryPrometheusWithBackoff with circuit breaker logic.
func (cb *PrometheusCircuitBreaker) QueryPrometheus(ctx context.Context, query string) (interface{}, promv1.Warnings, error) {
	// Check circuit state and handle transitions atomically
	cb.mu.Lock()
	state := cb.state
	if state == StateOpen {
		// Check if timeout has elapsed to transition to half-open
		timeSinceOpen := time.Since(cb.lastStateChangeTime)
		if timeSinceOpen >= cb.timeout {
			// Transition to half-open state atomically
			logger.Log.Infof("Circuit breaker transitioning to half-open: timeout=%v elapsed", cb.timeout)
			cb.state = StateHalfOpen
			cb.lastStateChangeTime = time.Now()
			cb.failureCount = 0
			cb.successCount = 0
			state = StateHalfOpen
		} else {
			cb.mu.Unlock()
			return nil, nil, ErrCircuitBreakerOpen
		}
	}
	cb.mu.Unlock()

	// Execute query with timeout in half-open state
	if state == StateHalfOpen {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cb.halfOpenTimeout)
		defer cancel()
	}

	// Execute the actual Prometheus query
	result, warnings, err := QueryPrometheusWithBackoff(ctx, cb.promAPI, query)
	if err != nil {
		cb.recordFailure()
		return nil, warnings, err
	}

	// Check for warnings (non-fatal but worth logging)
	if len(warnings) > 0 {
		logger.Log.Warnf("Prometheus query returned warnings: query=%s, warnings=%v", query, warnings)
	}

	cb.recordSuccess()
	return result, warnings, nil
}

// getState returns the current circuit breaker state (thread-safe read)
func (cb *PrometheusCircuitBreaker) getState() CircuitBreakerState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// recordFailure records a failure and updates circuit state if needed
func (cb *PrometheusCircuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCount++
	cb.successCount = 0
	cb.lastFailureTime = time.Now()

	switch cb.state {
	case StateClosed:
		// Check if we've exceeded the failure threshold
		if cb.failureCount >= cb.failureThreshold {
			logger.Log.Warnf("Circuit breaker opening: failureCount=%d, threshold=%d",
				cb.failureCount, cb.failureThreshold)
			cb.state = StateOpen
			cb.lastStateChangeTime = time.Now()
		}
	case StateHalfOpen:
		// Any failure in half-open state immediately opens the circuit
		logger.Log.Warnf("Circuit breaker reopening: failure in half-open state")
		cb.state = StateOpen
		cb.lastStateChangeTime = time.Now()
		cb.failureCount = 0 // Reset for next cycle
	}
}

// recordSuccess records a success and updates circuit state if needed
func (cb *PrometheusCircuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.successCount++
	cb.failureCount = 0

	switch cb.state {
	case StateHalfOpen:
		// Check if we've achieved enough successes to close the circuit
		if cb.successCount >= cb.successThreshold {
			logger.Log.Infof("Circuit breaker closing: successCount=%d, threshold=%d",
				cb.successCount, cb.successThreshold)
			cb.state = StateClosed
			cb.lastStateChangeTime = time.Now()
			cb.successCount = 0
		}
	case StateClosed:
		// Reset failure count on success in closed state
		if cb.failureCount > 0 {
			cb.failureCount = 0
		}
	}
}

// GetState returns the current circuit breaker state (for observability)
func (cb *PrometheusCircuitBreaker) GetState() CircuitBreakerState {
	return cb.getState()
}

// GetStateString returns a human-readable state string
func (cb *PrometheusCircuitBreaker) GetStateString() string {
	switch cb.getState() {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}
