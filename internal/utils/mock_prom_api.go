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
	"fmt"
	"sync"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// MockPromAPI is a mock implementation of promv1.API for testing
type MockPromAPI struct {
	QueryResults    map[string]model.Value
	QueryErrors     map[string]error
	QueryCallCounts map[string]int // Track number of calls per query for retry testing
	QueryFailCounts map[string]int // Number of times to fail before succeeding (for retry testing)
	mu              sync.RWMutex
}

func (m *MockPromAPI) Query(ctx context.Context, query string, ts time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	m.mu.Lock()
	// Initialize call count if not exists
	if m.QueryCallCounts == nil {
		m.QueryCallCounts = make(map[string]int)
	}

	m.QueryCallCounts[query]++
	callCount := m.QueryCallCounts[query]

	// Check if this query should fail a certain number of times before succeeding
	var shouldFail bool
	var failCount int
	if m.QueryFailCounts != nil {
		if fc, exists := m.QueryFailCounts[query]; exists {
			failCount = fc
			if callCount <= failCount {
				shouldFail = true
			}
		}
	}

	// Check for permanent errors
	var permErr error
	if err, exists := m.QueryErrors[query]; exists {
		permErr = err
	}

	// Get successful result
	var result model.Value
	var hasResult bool
	if val, exists := m.QueryResults[query]; exists {
		result = val
		hasResult = true
	}

	m.mu.Unlock()

	if shouldFail {
		return nil, nil, fmt.Errorf("transient error (attempt %d/%d)", callCount, failCount+1)
	}

	if permErr != nil {
		return nil, nil, permErr
	}

	if hasResult {
		return result, nil, nil
	}

	// Default return vector with one sample (to pass metrics validation)
	// This simulates Prometheus having scraped at least one metric
	return model.Vector{
		&model.Sample{
			Metric:    model.Metric{},
			Value:     0,
			Timestamp: model.TimeFromUnix(ts.Unix()),
		},
	}, nil, nil
}

func (m *MockPromAPI) QueryRange(ctx context.Context, query string, r promv1.Range, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) QueryExemplars(ctx context.Context, query string, startTime, endTime time.Time) ([]promv1.ExemplarQueryResult, error) {
	return nil, nil
}

func (m *MockPromAPI) Buildinfo(ctx context.Context) (promv1.BuildinfoResult, error) {
	return promv1.BuildinfoResult{}, nil
}

func (m *MockPromAPI) Config(ctx context.Context) (promv1.ConfigResult, error) {
	return promv1.ConfigResult{}, nil
}

func (m *MockPromAPI) Flags(ctx context.Context) (promv1.FlagsResult, error) {
	return promv1.FlagsResult{}, nil
}

func (m *MockPromAPI) LabelNames(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...promv1.Option) ([]string, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) LabelValues(ctx context.Context, label string, matches []string, startTime, endTime time.Time, opts ...promv1.Option) (model.LabelValues, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) Series(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...promv1.Option) ([]model.LabelSet, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) GetValue(ctx context.Context, timestamp time.Time, opts ...promv1.Option) (model.Value, promv1.Warnings, error) {
	return nil, nil, nil
}

func (m *MockPromAPI) Metadata(ctx context.Context, metric, limit string) (map[string][]promv1.Metadata, error) {
	return nil, nil
}

func (m *MockPromAPI) TSDB(ctx context.Context, opts ...promv1.Option) (promv1.TSDBResult, error) {
	return promv1.TSDBResult{}, nil
}

func (m *MockPromAPI) WalReplay(ctx context.Context) (promv1.WalReplayStatus, error) {
	return promv1.WalReplayStatus{}, nil
}

func (m *MockPromAPI) Targets(ctx context.Context) (promv1.TargetsResult, error) {
	return promv1.TargetsResult{}, nil
}

func (m *MockPromAPI) TargetsMetadata(ctx context.Context, matchTarget, metric, limit string) ([]promv1.MetricMetadata, error) {
	return nil, nil
}

func (m *MockPromAPI) AlertManagers(ctx context.Context) (promv1.AlertManagersResult, error) {
	return promv1.AlertManagersResult{}, nil
}

func (m *MockPromAPI) CleanTombstones(ctx context.Context) error {
	return nil
}

func (m *MockPromAPI) DeleteSeries(ctx context.Context, matches []string, startTime, endTime time.Time) error {
	return nil
}

func (m *MockPromAPI) Snapshot(ctx context.Context, skipHead bool) (promv1.SnapshotResult, error) {
	return promv1.SnapshotResult{}, nil
}

func (m *MockPromAPI) Rules(ctx context.Context) (promv1.RulesResult, error) {
	return promv1.RulesResult{}, nil
}

func (m *MockPromAPI) Alerts(ctx context.Context) (promv1.AlertsResult, error) {
	return promv1.AlertsResult{}, nil
}

func (m *MockPromAPI) Runtimeinfo(ctx context.Context) (promv1.RuntimeinfoResult, error) {
	return promv1.RuntimeinfoResult{}, nil
}
