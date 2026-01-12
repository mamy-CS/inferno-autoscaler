package utils

import (
	"testing"
	"time"
)

func TestFormatPrometheusDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		expected string
	}{
		// Days
		{"1 day", 24 * time.Hour, "1d"},
		{"2 days", 48 * time.Hour, "2d"},
		{"7 days", 7 * 24 * time.Hour, "7d"},

		// Hours
		{"1 hour", 1 * time.Hour, "1h"},
		{"12 hours", 12 * time.Hour, "12h"},

		// Minutes
		{"1 minute", 1 * time.Minute, "1m"},
		{"10 minutes", 10 * time.Minute, "10m"},
		{"30 minutes", 30 * time.Minute, "30m"},

		// Seconds
		{"1 second", 1 * time.Second, "1s"},
		{"30 seconds", 30 * time.Second, "30s"},

		// Mixed durations (not cleanly divisible)
		{"90 minutes", 90 * time.Minute, "90m"},

		// Very short durations
		{"100 milliseconds", 100 * time.Millisecond, "1s"},
		{"zero", 0, "1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatPrometheusDuration(tt.duration)
			if result != tt.expected {
				t.Errorf("FormatPrometheusDuration(%v) = %q, want %q", tt.duration, result, tt.expected)
			}
		})
	}
}
