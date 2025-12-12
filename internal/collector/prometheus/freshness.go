package prometheus

import (
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
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
