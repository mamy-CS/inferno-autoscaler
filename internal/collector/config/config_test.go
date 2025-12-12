package config

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config", func() {
	Describe("DefaultFreshnessThresholds", func() {
		It("should return default freshness thresholds", func() {
			thresholds := DefaultFreshnessThresholds()

			Expect(thresholds.FreshThreshold).To(Equal(1 * time.Minute))
			Expect(thresholds.StaleThreshold).To(Equal(2 * time.Minute))
			Expect(thresholds.UnavailableThreshold).To(Equal(5 * time.Minute))
		})

		It("should have valid threshold ordering", func() {
			thresholds := DefaultFreshnessThresholds()

			// FreshThreshold should be less than StaleThreshold
			Expect(thresholds.FreshThreshold).To(BeNumerically("<", thresholds.StaleThreshold))
			// StaleThreshold should be less than UnavailableThreshold
			Expect(thresholds.StaleThreshold).To(BeNumerically("<", thresholds.UnavailableThreshold))
			// FreshThreshold should be less than UnavailableThreshold
			Expect(thresholds.FreshThreshold).To(BeNumerically("<", thresholds.UnavailableThreshold))
		})

		It("should return non-zero thresholds", func() {
			thresholds := DefaultFreshnessThresholds()

			Expect(thresholds.FreshThreshold).To(BeNumerically(">", 0))
			Expect(thresholds.StaleThreshold).To(BeNumerically(">", 0))
			Expect(thresholds.UnavailableThreshold).To(BeNumerically(">", 0))
		})

		It("should return consistent values on multiple calls", func() {
			thresholds1 := DefaultFreshnessThresholds()
			thresholds2 := DefaultFreshnessThresholds()

			Expect(thresholds1).To(Equal(thresholds2))
		})
	})

	Describe("CacheConfig", func() {
		It("should have all required fields", func() {
			cfg := CacheConfig{
				Enabled:             true,
				TTL:                 30 * time.Second,
				CleanupInterval:     1 * time.Minute,
				FetchInterval:       30 * time.Second,
				FreshnessThresholds: DefaultFreshnessThresholds(),
			}

			Expect(cfg.Enabled).To(BeTrue())
			Expect(cfg.TTL).To(Equal(30 * time.Second))
			Expect(cfg.CleanupInterval).To(Equal(1 * time.Minute))
			Expect(cfg.FetchInterval).To(Equal(30 * time.Second))
			Expect(cfg.FreshnessThresholds).NotTo(BeNil())
		})

		It("should support disabled cache", func() {
			cfg := CacheConfig{
				Enabled: false,
			}

			Expect(cfg.Enabled).To(BeFalse())
		})

		It("should support zero fetch interval (disabled background fetching)", func() {
			cfg := CacheConfig{
				FetchInterval: 0,
			}

			Expect(cfg.FetchInterval).To(Equal(time.Duration(0)))
		})
	})

	Describe("FreshnessThresholds", func() {
		It("should support custom thresholds", func() {
			thresholds := FreshnessThresholds{
				FreshThreshold:       10 * time.Second,
				StaleThreshold:       20 * time.Second,
				UnavailableThreshold: 30 * time.Second,
			}

			Expect(thresholds.FreshThreshold).To(Equal(10 * time.Second))
			Expect(thresholds.StaleThreshold).To(Equal(20 * time.Second))
			Expect(thresholds.UnavailableThreshold).To(Equal(30 * time.Second))
		})

		It("should handle zero thresholds", func() {
			thresholds := FreshnessThresholds{
				FreshThreshold:       0,
				StaleThreshold:       0,
				UnavailableThreshold: 0,
			}

			Expect(thresholds.FreshThreshold).To(Equal(time.Duration(0)))
			Expect(thresholds.StaleThreshold).To(Equal(time.Duration(0)))
			Expect(thresholds.UnavailableThreshold).To(Equal(time.Duration(0)))
		})

		It("should handle very large thresholds", func() {
			thresholds := FreshnessThresholds{
				FreshThreshold:       1 * time.Hour,
				StaleThreshold:       2 * time.Hour,
				UnavailableThreshold: 5 * time.Hour,
			}

			Expect(thresholds.FreshThreshold).To(Equal(1 * time.Hour))
			Expect(thresholds.StaleThreshold).To(Equal(2 * time.Hour))
			Expect(thresholds.UnavailableThreshold).To(Equal(5 * time.Hour))
		})

		Describe("DetermineStatus", func() {
			var thresholds FreshnessThresholds

			BeforeEach(func() {
				thresholds = FreshnessThresholds{
					FreshThreshold:       1 * time.Minute,
					StaleThreshold:       2 * time.Minute,
					UnavailableThreshold: 5 * time.Minute,
				}
			})

			It("should return 'fresh' when age is less than FreshThreshold", func() {
				age := 30 * time.Second
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("fresh"))
			})

			It("should return 'fresh' when age is zero", func() {
				age := time.Duration(0)
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("fresh"))
			})

			It("should return 'fresh' when age is just below FreshThreshold", func() {
				age := 59 * time.Second
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("fresh"))
			})

			It("should return 'stale' when age equals FreshThreshold", func() {
				age := 1 * time.Minute
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("stale"))
			})

			It("should return 'stale' when age is between FreshThreshold and UnavailableThreshold", func() {
				age := 3 * time.Minute
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("stale"))
			})

			It("should return 'stale' when age is just below UnavailableThreshold", func() {
				age := 4*time.Minute + 59*time.Second
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("stale"))
			})

			It("should return 'unavailable' when age equals UnavailableThreshold", func() {
				age := 5 * time.Minute
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("unavailable"))
			})

			It("should return 'unavailable' when age exceeds UnavailableThreshold", func() {
				age := 10 * time.Minute
				status := thresholds.DetermineStatus(age)
				Expect(status).To(Equal("unavailable"))
			})

			It("should handle custom thresholds correctly", func() {
				customThresholds := FreshnessThresholds{
					FreshThreshold:       10 * time.Second,
					StaleThreshold:       20 * time.Second,
					UnavailableThreshold: 30 * time.Second,
				}

				// Fresh
				status := customThresholds.DetermineStatus(5 * time.Second)
				Expect(status).To(Equal("fresh"))

				// Stale
				status = customThresholds.DetermineStatus(15 * time.Second)
				Expect(status).To(Equal("stale"))

				// Unavailable
				status = customThresholds.DetermineStatus(35 * time.Second)
				Expect(status).To(Equal("unavailable"))
			})

			It("should handle very small thresholds", func() {
				smallThresholds := FreshnessThresholds{
					FreshThreshold:       1 * time.Millisecond,
					StaleThreshold:       2 * time.Millisecond,
					UnavailableThreshold: 5 * time.Millisecond,
				}

				status := smallThresholds.DetermineStatus(500 * time.Microsecond)
				Expect(status).To(Equal("fresh"))

				status = smallThresholds.DetermineStatus(3 * time.Millisecond)
				Expect(status).To(Equal("stale"))

				status = smallThresholds.DetermineStatus(10 * time.Millisecond)
				Expect(status).To(Equal("unavailable"))
			})

			It("should handle very large thresholds", func() {
				largeThresholds := FreshnessThresholds{
					FreshThreshold:       1 * time.Hour,
					StaleThreshold:       2 * time.Hour,
					UnavailableThreshold: 5 * time.Hour,
				}

				status := largeThresholds.DetermineStatus(30 * time.Minute)
				Expect(status).To(Equal("fresh"))

				status = largeThresholds.DetermineStatus(90 * time.Minute)
				Expect(status).To(Equal("stale"))

				status = largeThresholds.DetermineStatus(6 * time.Hour)
				Expect(status).To(Equal("unavailable"))
			})

			It("should use default thresholds correctly", func() {
				defaultThresholds := DefaultFreshnessThresholds()

				// Verify defaults
				Expect(defaultThresholds.FreshThreshold).To(Equal(1 * time.Minute))
				Expect(defaultThresholds.StaleThreshold).To(Equal(2 * time.Minute))
				Expect(defaultThresholds.UnavailableThreshold).To(Equal(5 * time.Minute))

				// Test with defaults
				status := defaultThresholds.DetermineStatus(30 * time.Second)
				Expect(status).To(Equal("fresh"))

				status = defaultThresholds.DetermineStatus(90 * time.Second)
				Expect(status).To(Equal("stale"))

				status = defaultThresholds.DetermineStatus(6 * time.Minute)
				Expect(status).To(Equal("unavailable"))
			})
		})
	})
})
