package collector

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

var _ = Describe("ScaleToZero", func() {
	Describe("formatPrometheusDuration", func() {
		It("should format days correctly", func() {
			Expect(formatPrometheusDuration(24 * time.Hour)).To(Equal("1d"))
			Expect(formatPrometheusDuration(48 * time.Hour)).To(Equal("2d"))
			Expect(formatPrometheusDuration(7 * 24 * time.Hour)).To(Equal("7d"))
		})

		It("should format hours correctly", func() {
			Expect(formatPrometheusDuration(1 * time.Hour)).To(Equal("1h"))
			Expect(formatPrometheusDuration(12 * time.Hour)).To(Equal("12h"))
		})

		It("should format minutes correctly", func() {
			Expect(formatPrometheusDuration(1 * time.Minute)).To(Equal("1m"))
			Expect(formatPrometheusDuration(10 * time.Minute)).To(Equal("10m"))
			Expect(formatPrometheusDuration(30 * time.Minute)).To(Equal("30m"))
		})

		It("should format seconds correctly", func() {
			Expect(formatPrometheusDuration(1 * time.Second)).To(Equal("1s"))
			Expect(formatPrometheusDuration(30 * time.Second)).To(Equal("30s"))
		})

		It("should handle mixed durations", func() {
			// 1h30m is not cleanly divisible by hour, so it should use minutes
			Expect(formatPrometheusDuration(90 * time.Minute)).To(Equal("90m"))
		})

		It("should handle very short durations", func() {
			Expect(formatPrometheusDuration(100 * time.Millisecond)).To(Equal("1s"))
			Expect(formatPrometheusDuration(0)).To(Equal("1s"))
		})
	})

	Describe("RegisterScaleToZeroQueries", func() {
		It("should register the model request count query", func() {
			ctx := context.Background()
			registry := NewSourceRegistry()

			// Create a mock prometheus source
			mockAPI := &mockPrometheusAPI{}
			source := NewPrometheusSource(ctx, mockAPI, DefaultPrometheusSourceConfig())
			Expect(registry.Register("prometheus", source)).To(Succeed())

			// Register scale-to-zero queries
			RegisterScaleToZeroQueries(registry)

			// Verify the query is registered
			query := source.QueryList().Get(QueryModelRequestCount)
			Expect(query).NotTo(BeNil())
			Expect(query.Name).To(Equal(QueryModelRequestCount))
			Expect(query.Type).To(Equal(QueryTypePromQL))
			Expect(query.Params).To(ContainElements(ParamNamespace, ParamModelID, ParamRetentionPeriod))
		})

		It("should not panic when prometheus source is not registered", func() {
			registry := NewSourceRegistry()
			// This should not panic
			RegisterScaleToZeroQueries(registry)
		})
	})

	Describe("ScaleToZeroCollector", func() {
		var (
			ctx      context.Context
			registry *SourceRegistry
			mockAPI  *mockPrometheusAPI
		)

		BeforeEach(func() {
			ctx = context.Background()
			registry = NewSourceRegistry()
			mockAPI = &mockPrometheusAPI{}
			source := NewPrometheusSource(ctx, mockAPI, DefaultPrometheusSourceConfig())
			Expect(registry.Register("prometheus", source)).To(Succeed())
			RegisterScaleToZeroQueries(registry)
		})

		Describe("CollectModelRequestCount", func() {
			It("should return request count when metrics are available", func() {
				mockAPI.queryFunc = func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					// Return a scalar value representing total requests
					return &model.Scalar{
						Value:     model.SampleValue(100.5),
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					}, nil, nil
				}

				collector := NewScaleToZeroCollector(registry.Get("prometheus"))
				count, err := collector.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(100.5))
			})

			It("should return 0 when no metrics are available", func() {
				mockAPI.queryFunc = func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					// Return empty vector
					return model.Vector{}, nil, nil
				}

				collector := NewScaleToZeroCollector(registry.Get("prometheus"))
				count, err := collector.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(0.0))
			})

			It("should return 0 when query returns error", func() {
				mockAPI.queryFunc = func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					return nil, nil, context.DeadlineExceeded
				}

				collector := NewScaleToZeroCollector(registry.Get("prometheus"))
				count, err := collector.CollectModelRequestCount(ctx, "my-model", "default", 10*time.Minute)

				// Should return 0, not an error (no requests is valid)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(0.0))
			})

			It("should format retention period correctly in query", func() {
				var capturedQuery string
				mockAPI.queryFunc = func(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
					capturedQuery = query
					return &model.Scalar{
						Value:     model.SampleValue(50),
						Timestamp: model.TimeFromUnix(time.Now().Unix()),
					}, nil, nil
				}

				collector := NewScaleToZeroCollector(registry.Get("prometheus"))
				_, _ = collector.CollectModelRequestCount(ctx, "test-model", "test-ns", 15*time.Minute)

				Expect(capturedQuery).To(ContainSubstring("[15m]"))
				Expect(capturedQuery).To(ContainSubstring(`model_name="test-model"`))
				Expect(capturedQuery).To(ContainSubstring(`namespace="test-ns"`))
			})
		})
	})
})
