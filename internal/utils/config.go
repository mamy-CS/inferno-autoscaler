package utils

import (
	"strconv"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
)

// GetConfigValue retrieves a value from a ConfigMap with a default fallback
func GetConfigValue(data map[string]string, key, def string) string {
	if v, ok := data[key]; ok {
		return v
	}
	return def
}

// ParseDurationFromConfig parses a duration string from ConfigMap with default fallback
// Returns the parsed duration or the default value if parsing fails or key is missing
func ParseDurationFromConfig(data map[string]string, key string, defaultValue time.Duration) time.Duration {
	if valStr := GetConfigValue(data, key, ""); valStr != "" {
		if val, err := time.ParseDuration(valStr); err == nil {
			return val
		}
		logger.Log.Warnf("Invalid duration value '%s' for key '%s' in ConfigMap, using default: %v", valStr, key, defaultValue)
	}
	return defaultValue
}

// ParseIntFromConfig parses an integer from ConfigMap with default fallback and minimum value validation
// Returns the parsed integer or the default value if parsing fails, is less than minValue, or key is missing
func ParseIntFromConfig(data map[string]string, key string, defaultValue int, minValue int) int {
	if valStr := GetConfigValue(data, key, ""); valStr != "" {
		if val, err := strconv.Atoi(valStr); err == nil && val >= minValue {
			return val
		}
		logger.Log.Warnf("Invalid integer value '%s' for key '%s' in ConfigMap (must be >= %d), using default: %d", valStr, key, minValue, defaultValue)
	}
	return defaultValue
}

// ParseBoolFromConfig parses a boolean from ConfigMap with default fallback
// Accepts "true", "1", or "yes" as true values (case-sensitive)
// Returns the parsed boolean or the default value if key is missing or value is not recognized
func ParseBoolFromConfig(data map[string]string, key string, defaultValue bool) bool {
	if valStr := GetConfigValue(data, key, ""); valStr != "" {
		// Accept "true", "1", "yes" as true
		return valStr == "true" || valStr == "1" || valStr == "yes"
	}
	return defaultValue
}
