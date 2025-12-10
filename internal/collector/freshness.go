package collector

import (
	"fmt"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FreshnessThresholds defines when metrics are considered fresh, stale, or unavailable
type FreshnessThresholds struct {
	FreshThreshold       time.Duration // Metrics are fresh if age < this (default: 1 minute)
	StaleThreshold       time.Duration // Metrics are stale if age >= this but < unavailable (default: 2 minutes)
	UnavailableThreshold time.Duration // Metrics are unavailable if age >= this (default: 5 minutes)
}

// DefaultFreshnessThresholds returns default freshness thresholds
func DefaultFreshnessThresholds() FreshnessThresholds {
	return FreshnessThresholds{
		FreshThreshold:       1 * time.Minute,
		StaleThreshold:       2 * time.Minute,
		UnavailableThreshold: 5 * time.Minute,
	}
}

// DetermineFreshnessStatus determines the freshness status based on age
func DetermineFreshnessStatus(age time.Duration, thresholds FreshnessThresholds) string {
	if age < thresholds.FreshThreshold {
		return "fresh"
	} else if age < thresholds.UnavailableThreshold {
		return "stale"
	}
	return "unavailable"
}

// NewMetricsMetadata creates a new MetricsMetadata with current timestamp and calculated age
func NewMetricsMetadata(collectedAt time.Time, thresholds FreshnessThresholds) *llmdVariantAutoscalingV1alpha1.MetricsMetadata {
	age := time.Since(collectedAt)
	return &llmdVariantAutoscalingV1alpha1.MetricsMetadata{
		CollectedAt:     metav1.NewTime(collectedAt),
		AgeSeconds:      fmt.Sprintf("%.2f", age.Seconds()), // Serialize as string to avoid CRD float issues
		FreshnessStatus: DetermineFreshnessStatus(age, thresholds),
	}
}
