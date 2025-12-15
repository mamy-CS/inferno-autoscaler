package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePrometheusConfigFromEnv(t *testing.T) {
	// Test with HTTPS URL (default)
	if err := os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}

	config := ParsePrometheusConfigFromEnv()
	assert.Equal(t, "https://prometheus:9090", config.BaseURL)

	// Test with explicit TLS configuration
	if err := os.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "true"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}

	config = ParsePrometheusConfigFromEnv()
	assert.Equal(t, "https://prometheus:9090", config.BaseURL)
	assert.True(t, config.InsecureSkipVerify)

	// Test OpenShift configuration
	if err := os.Setenv("PROMETHEUS_BASE_URL", "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "false"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_CA_CERT_PATH", "/etc/openshift-ca/ca.crt"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_CLIENT_CERT_PATH", ""); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_CLIENT_KEY_PATH", ""); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_SERVER_NAME", "thanos-querier.openshift-monitoring.svc"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}
	if err := os.Setenv("PROMETHEUS_TOKEN_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"); err != nil {
		t.Fatalf("Failed to set environment variable: %v", err)
	}

	config = ParsePrometheusConfigFromEnv()
	assert.Equal(t, "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091", config.BaseURL)
	assert.False(t, config.InsecureSkipVerify)
	assert.Equal(t, "/etc/openshift-ca/ca.crt", config.CACertPath)
	assert.Equal(t, "", config.ClientCertPath)
	assert.Equal(t, "", config.ClientKeyPath)
	assert.Equal(t, "thanos-querier.openshift-monitoring.svc", config.ServerName)
	assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", config.TokenPath)

	// Clean up
	if err := os.Unsetenv("PROMETHEUS_BASE_URL"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_CA_CERT_PATH"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_CLIENT_CERT_PATH"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_CLIENT_KEY_PATH"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_SERVER_NAME"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
	if err := os.Unsetenv("PROMETHEUS_TOKEN_PATH"); err != nil {
		t.Fatalf("Failed to unset environment variable: %v", err)
	}
}
