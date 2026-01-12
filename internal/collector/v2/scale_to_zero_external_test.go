package collector_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/v2"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/v2/registration"
)

// mockPrometheusAPI implements promv1.API for testing
type mockPrometheusAPI struct {
	queryFunc func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error)
}

func (m *mockPrometheusAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, ts, opts...)
	}
	return nil, nil, nil
}

// Implement remaining API methods as no-ops for interface compliance
func (m *mockPrometheusAPI) AlertManagers(ctx context.Context) (v1.AlertManagersResult, error) {
	return v1.AlertManagersResult{}, nil
}
func (m *mockPrometheusAPI) Alerts(ctx context.Context) (v1.AlertsResult, error) {
	return v1.AlertsResult{}, nil
}
func (m *mockPrometheusAPI) Buildinfo(ctx context.Context) (v1.BuildinfoResult, error) {
	return v1.BuildinfoResult{}, nil
}
func (m *mockPrometheusAPI) CleanTombstones(ctx context.Context) error { return nil }
func (m *mockPrometheusAPI) Config(ctx context.Context) (v1.ConfigResult, error) {
	return v1.ConfigResult{}, nil
}
func (m *mockPrometheusAPI) DeleteSeries(ctx context.Context, matches []string, startTime, endTime time.Time) error {
	return nil
}
func (m *mockPrometheusAPI) Flags(ctx context.Context) (v1.FlagsResult, error) {
	return v1.FlagsResult{}, nil
}
func (m *mockPrometheusAPI) LabelNames(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]string, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) LabelValues(ctx context.Context, label string, matches []string, startTime, endTime time.Time, opts ...v1.Option) (model.LabelValues, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Metadata(ctx context.Context, metric, limit string) (map[string][]v1.Metadata, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) QueryExemplars(ctx context.Context, query string, startTime, endTime time.Time) ([]v1.ExemplarQueryResult, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) QueryRange(ctx context.Context, query string, r v1.Range, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Rules(ctx context.Context) (v1.RulesResult, error) {
	return v1.RulesResult{}, nil
}
func (m *mockPrometheusAPI) Runtimeinfo(ctx context.Context) (v1.RuntimeinfoResult, error) {
	return v1.RuntimeinfoResult{}, nil
}
func (m *mockPrometheusAPI) Series(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]model.LabelSet, v1.Warnings, error) {
	return nil, nil, nil
}
func (m *mockPrometheusAPI) Snapshot(ctx context.Context, skipHead bool) (v1.SnapshotResult, error) {
	return v1.SnapshotResult{}, nil
}
func (m *mockPrometheusAPI) Targets(ctx context.Context) (v1.TargetsResult, error) {
	return v1.TargetsResult{}, nil
}
func (m *mockPrometheusAPI) TargetsMetadata(ctx context.Context, matchTarget, metric, limit string) ([]v1.MetricMetadata, error) {
	return nil, nil
}
func (m *mockPrometheusAPI) TSDB(ctx context.Context, opts ...v1.Option) (v1.TSDBResult, error) {
	return v1.TSDBResult{}, nil
}
func (m *mockPrometheusAPI) WalReplay(ctx context.Context) (v1.WalReplayStatus, error) {
	return v1.WalReplayStatus{}, nil
}

func TestRegisterScaleToZeroQueries(t *testing.T) {
	t.Run("should register the model request count query", func(t *testing.T) {
		ctx := context.Background()
		registry := collector.NewSourceRegistry()

		// Create a mock prometheus source
		mockAPI := &mockPrometheusAPI{}
		source := collector.NewPrometheusSource(ctx, mockAPI, collector.DefaultPrometheusSourceConfig())
		if err := registry.Register("prometheus", source); err != nil {
			t.Fatalf("failed to register prometheus source: %v", err)
		}

		// Register scale-to-zero queries
		registration.RegisterScaleToZeroQueries(registry)

		// Verify the query is registered
		query := source.QueryList().Get(registration.QueryModelRequestCount)
		if query == nil {
			t.Fatal("expected query to be registered, got nil")
		}
		if query.Name != registration.QueryModelRequestCount {
			t.Errorf("expected query name %q, got %q", registration.QueryModelRequestCount, query.Name)
		}
		if query.Type != collector.QueryTypePromQL {
			t.Errorf("expected query type %q, got %q", collector.QueryTypePromQL, query.Type)
		}
	})

	t.Run("should not panic when prometheus source is not registered", func(t *testing.T) {
		registry := collector.NewSourceRegistry()
		// This should not panic
		registration.RegisterScaleToZeroQueries(registry)
	})
}

func TestScaleToZeroCollector_CollectModelRequestCount(t *testing.T) {
	t.Run("should return request count when metrics are available", func(t *testing.T) {
		ctx := context.Background()
		registry := collector.NewSourceRegistry()
		mockAPI := &mockPrometheusAPI{
			queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
				return &model.Scalar{
					Value:     model.SampleValue(100.5),
					Timestamp: model.TimeFromUnix(time.Now().Unix()),
				}, nil, nil
			},
		}
		source := collector.NewPrometheusSource(ctx, mockAPI, collector.DefaultPrometheusSourceConfig())
		if err := registry.Register("prometheus", source); err != nil {
			t.Fatalf("failed to register prometheus source: %v", err)
		}
		registration.RegisterScaleToZeroQueries(registry)

		coll := registration.NewScaleToZeroCollector(registry.Get("prometheus"))
		count, err := coll.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 100.5 {
			t.Errorf("expected count 100.5, got %f", count)
		}
	})

	t.Run("should return 0 when no metrics are available", func(t *testing.T) {
		ctx := context.Background()
		registry := collector.NewSourceRegistry()
		mockAPI := &mockPrometheusAPI{
			queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
				return model.Vector{}, nil, nil
			},
		}
		source := collector.NewPrometheusSource(ctx, mockAPI, collector.DefaultPrometheusSourceConfig())
		if err := registry.Register("prometheus", source); err != nil {
			t.Fatalf("failed to register prometheus source: %v", err)
		}
		registration.RegisterScaleToZeroQueries(registry)

		coll := registration.NewScaleToZeroCollector(registry.Get("prometheus"))
		count, err := coll.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 0.0 {
			t.Errorf("expected count 0.0, got %f", count)
		}
	})

	t.Run("should return 0 when query returns error", func(t *testing.T) {
		ctx := context.Background()
		registry := collector.NewSourceRegistry()
		mockAPI := &mockPrometheusAPI{
			queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
				return nil, nil, context.DeadlineExceeded
			},
		}
		source := collector.NewPrometheusSource(ctx, mockAPI, collector.DefaultPrometheusSourceConfig())
		if err := registry.Register("prometheus", source); err != nil {
			t.Fatalf("failed to register prometheus source: %v", err)
		}
		registration.RegisterScaleToZeroQueries(registry)

		coll := registration.NewScaleToZeroCollector(registry.Get("prometheus"))
		count, err := coll.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

		// Should return 0, not an error (no requests is valid)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 0.0 {
			t.Errorf("expected count 0.0, got %f", count)
		}
	})

	t.Run("should format retention period correctly in query", func(t *testing.T) {
		ctx := context.Background()
		registry := collector.NewSourceRegistry()
		var capturedQuery string
		mockAPI := &mockPrometheusAPI{
			queryFunc: func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
				capturedQuery = query
				return &model.Scalar{
					Value:     model.SampleValue(50),
					Timestamp: model.TimeFromUnix(time.Now().Unix()),
				}, nil, nil
			},
		}
		source := collector.NewPrometheusSource(ctx, mockAPI, collector.DefaultPrometheusSourceConfig())
		if err := registry.Register("prometheus", source); err != nil {
			t.Fatalf("failed to register prometheus source: %v", err)
		}
		registration.RegisterScaleToZeroQueries(registry)

		coll := registration.NewScaleToZeroCollector(registry.Get("prometheus"))
		_, _ = coll.CollectModelRequestCount(ctx, "test-model", "test-ns", 15*time.Minute)

		if capturedQuery == "" {
			t.Fatal("expected query to be captured")
		}
		if !containsSubstring(capturedQuery, "[15m]") {
			t.Errorf("expected query to contain [15m], got: %s", capturedQuery)
		}
		if !containsSubstring(capturedQuery, `model_name="test-model"`) {
			t.Errorf("expected query to contain model_name=\"test-model\", got: %s", capturedQuery)
		}
		if !containsSubstring(capturedQuery, `namespace="test-ns"`) {
			t.Errorf("expected query to contain namespace=\"test-ns\", got: %s", capturedQuery)
		}
	})
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstringHelper(s, substr))
}

func containsSubstringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
