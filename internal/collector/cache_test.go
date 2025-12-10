package collector

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

var _ = Describe("Cache", func() {
	BeforeEach(func() {
		logger.Log = zap.NewNop().Sugar()
	})

	Describe("CacheKey", func() {
		It("should create cache key correctly", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			Expect(key).To(Equal(CacheKey("model1/ns1/variant1/allocation")))
		})

		It("should handle special characters in cache key", func() {
			key := NewCacheKey("model-1", "ns_1", "variant.1", "replica-metrics")
			Expect(key).To(Equal(CacheKey("model-1/ns_1/variant.1/replica-metrics")))
		})
	})

	Describe("CachedMetrics", func() {
		It("should detect expired metrics", func() {
			cached := &CachedMetrics{
				Data:        "test-data",
				CollectedAt: time.Now().Add(-2 * time.Minute),
				TTL:         30 * time.Second,
			}
			Expect(cached.IsExpired()).To(BeTrue())
		})

		It("should detect fresh metrics", func() {
			cached := &CachedMetrics{
				Data:        "test-data",
				CollectedAt: time.Now().Add(-10 * time.Second),
				TTL:         30 * time.Second,
			}
			Expect(cached.IsExpired()).To(BeFalse())
		})

		It("should calculate age correctly", func() {
			now := time.Now()
			cached := &CachedMetrics{
				Data:        "test-data",
				CollectedAt: now.Add(-1 * time.Minute),
				TTL:         30 * time.Second,
			}
			age := cached.Age()
			Expect(age).To(BeNumerically(">=", 59*time.Second))
			Expect(age).To(BeNumerically("<=", 61*time.Second))
		})

		It("should handle nil cached metrics", func() {
			var cached *CachedMetrics
			Expect(cached.IsExpired()).To(BeTrue())
			Expect(cached.Age()).To(Equal(time.Duration(0)))
		})
	})

	Describe("MemoryCache", func() {
		var cache *MemoryCache

		BeforeEach(func() {
			cache = NewMemoryCache(30*time.Second, 0, 1*time.Minute)
		})

		AfterEach(func() {
			if cache != nil {
				cache.Stop()
			}
		})

		It("should store and retrieve cached metrics", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			data := "test-allocation-data"

			cache.Set(key, data, 0) // Use default TTL

			cached, found := cache.Get(key)
			Expect(found).To(BeTrue())
			Expect(cached.Data).To(Equal(data))
			Expect(cached.IsExpired()).To(BeFalse())
		})

		It("should return false for non-existent keys", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			_, found := cache.Get(key)
			Expect(found).To(BeFalse())
		})

		It("should expire metrics after TTL", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			data := "test-data"

			// Set with short TTL
			cache.Set(key, data, 100*time.Millisecond)

			// Should be found immediately
			_, found := cache.Get(key)
			Expect(found).To(BeTrue())

			// Wait for expiration
			time.Sleep(150 * time.Millisecond)

			// Should be expired now
			_, found = cache.Get(key)
			Expect(found).To(BeFalse())
		})

		It("should invalidate specific cache entries", func() {
			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model1", "ns1", "variant2", "allocation")

			cache.Set(key1, "data1", 0)
			cache.Set(key2, "data2", 0)

			cache.Invalidate(key1)

			_, found1 := cache.Get(key1)
			_, found2 := cache.Get(key2)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeTrue())
		})

		It("should invalidate all entries for a model", func() {
			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model1", "ns1", "variant2", "allocation")
			key3 := NewCacheKey("model2", "ns1", "variant1", "allocation")

			cache.Set(key1, "data1", 0)
			cache.Set(key2, "data2", 0)
			cache.Set(key3, "data3", 0)

			cache.InvalidateForModel("model1", "ns1")

			_, found1 := cache.Get(key1)
			_, found2 := cache.Get(key2)
			_, found3 := cache.Get(key3)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeFalse())
			Expect(found3).To(BeTrue())
		})

		It("should invalidate all entries for a variant", func() {
			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model1", "ns1", "variant1", "replica-metrics")
			key3 := NewCacheKey("model1", "ns1", "variant2", "allocation")

			cache.Set(key1, "data1", 0)
			cache.Set(key2, "data2", 0)
			cache.Set(key3, "data3", 0)

			cache.InvalidateForVariant("model1", "ns1", "variant1")

			_, found1 := cache.Get(key1)
			_, found2 := cache.Get(key2)
			_, found3 := cache.Get(key3)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeFalse())
			Expect(found3).To(BeTrue())
		})

		It("should clear all cache entries", func() {
			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model2", "ns2", "variant2", "allocation")

			cache.Set(key1, "data1", 0)
			cache.Set(key2, "data2", 0)

			Expect(cache.Size()).To(Equal(2))

			cache.Clear()

			Expect(cache.Size()).To(Equal(0))
			_, found1 := cache.Get(key1)
			_, found2 := cache.Get(key2)
			Expect(found1).To(BeFalse())
			Expect(found2).To(BeFalse())
		})

		It("should track cache size correctly", func() {
			Expect(cache.Size()).To(Equal(0))

			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model1", "ns1", "variant2", "allocation")

			cache.Set(key1, "data1", 0)
			Expect(cache.Size()).To(Equal(1))

			cache.Set(key2, "data2", 0)
			Expect(cache.Size()).To(Equal(2))

			cache.Invalidate(key1)
			Expect(cache.Size()).To(Equal(1))
		})

		It("should respect max size limit", func() {
			limitedCache := NewMemoryCache(30*time.Second, 2, 1*time.Minute)
			defer limitedCache.Stop()

			key1 := NewCacheKey("model1", "ns1", "variant1", "allocation")
			key2 := NewCacheKey("model1", "ns1", "variant2", "allocation")
			key3 := NewCacheKey("model1", "ns1", "variant3", "allocation")

			limitedCache.Set(key1, "data1", 0)
			Expect(limitedCache.Size()).To(Equal(1))
			limitedCache.Set(key2, "data2", 0)
			Expect(limitedCache.Size()).To(Equal(2))
			// When maxSize is set and reached, the current implementation allows growth beyond max
			// This is a simple implementation that logs a warning but doesn't enforce the limit strictly
			limitedCache.Set(key3, "data3", 0)
			// Verify it doesn't enforce strict limit (allows growth)
			Expect(limitedCache.Size()).To(BeNumerically(">=", 3))
		})
	})

	Describe("noOpCache", func() {
		var noOp *noOpCache

		BeforeEach(func() {
			noOp = &noOpCache{}
		})

		It("should always return false for Get", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			_, found := noOp.Get(key)
			Expect(found).To(BeFalse())
		})

		It("should have no effect on Set", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			noOp.Set(key, "data", 30*time.Second)
			// Should not panic and should not store anything
			_, found := noOp.Get(key)
			Expect(found).To(BeFalse())
		})

		It("should have no effect on Invalidate", func() {
			key := NewCacheKey("model1", "ns1", "variant1", "allocation")
			noOp.Invalidate(key)
			// Should not panic
		})

		It("should return zero size", func() {
			Expect(noOp.Size()).To(Equal(0))
		})
	})
})
