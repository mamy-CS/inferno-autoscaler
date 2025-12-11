package prometheus

import (
	"fmt"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DetermineFreshnessStatus determines the freshness status based on age
func DetermineFreshnessStatus(age time.Duration, thresholds config.FreshnessThresholds) string {
	if age < thresholds.FreshThreshold {
		return "fresh"
	} else if age < thresholds.UnavailableThreshold {
		return "stale"
	}
	return "unavailable"
}

// NewMetricsMetadata creates a new MetricsMetadata with current timestamp and calculated age
func NewMetricsMetadata(collectedAt time.Time, thresholds config.FreshnessThresholds) *llmdVariantAutoscalingV1alpha1.MetricsMetadata {
	age := time.Since(collectedAt)
	return &llmdVariantAutoscalingV1alpha1.MetricsMetadata{
		CollectedAt:     metav1.NewTime(collectedAt),
		AgeSeconds:      fmt.Sprintf("%.2f", age.Seconds()), // Serialize as string to avoid CRD float issues
		FreshnessStatus: DetermineFreshnessStatus(age, thresholds),
	}
}
