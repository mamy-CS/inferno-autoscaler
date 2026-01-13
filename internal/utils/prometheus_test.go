package utils

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FormatPrometheusDuration", func() {
	DescribeTable("formats durations correctly",
		func(duration time.Duration, expected string) {
			result := FormatPrometheusDuration(duration)
			Expect(result).To(Equal(expected))
		},
		// Days
		Entry("1 day", 24*time.Hour, "1d"),
		Entry("2 days", 48*time.Hour, "2d"),
		Entry("7 days", 7*24*time.Hour, "7d"),

		// Hours
		Entry("1 hour", 1*time.Hour, "1h"),
		Entry("12 hours", 12*time.Hour, "12h"),

		// Minutes
		Entry("1 minute", 1*time.Minute, "1m"),
		Entry("10 minutes", 10*time.Minute, "10m"),
		Entry("30 minutes", 30*time.Minute, "30m"),

		// Seconds
		Entry("1 second", 1*time.Second, "1s"),
		Entry("30 seconds", 30*time.Second, "30s"),

		// Mixed durations (not cleanly divisible)
		Entry("90 minutes", 90*time.Minute, "90m"),

		// Very short durations
		Entry("100 milliseconds", 100*time.Millisecond, "1s"),
		Entry("zero", time.Duration(0), "1s"),
	)
})
