package fixtures

const (
	// decodeNameSuffix follows the llm-d fixture convention where model-serving
	// workloads are named with "<base>-decode".
	decodeNameSuffix = "-decode"
	// serviceNameSuffix follows the fixture convention where Service names are
	// derived from "<base>-service".
	serviceNameSuffix = "-service"
	// serviceMonitorNameSuffix follows the fixture convention "<base>-monitor".
	serviceMonitorNameSuffix = "-monitor"

	// kubePrometheusStackReleaseLabelValue matches the Helm release label used by
	// kube-prometheus-stack ServiceMonitor discovery.
	kubePrometheusStackReleaseLabelValue = "kube-prometheus-stack"
	defaultServicePortName               = "http"
	defaultServiceMonitorMetricsPath     = "/metrics"

	defaultModelServiceSimulatorImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.7.1"
	defaultModelServiceRuntimeImage   = "ghcr.io/llm-d/llm-d-cuda-dev:latest"
	defaultModelServiceContainerPort  = int32(8000)
	defaultHFTokenSecretName          = "llm-d-hf-token"
	defaultHFTokenSecretKey           = "HF_TOKEN"
	defaultModelServiceLabelValue     = "ms-sim-llm-d-modelservice"
	defaultInferenceServingLabelValue = "true"
	defaultGuideLabelValue            = "workload-autoscaling"
	defaultTestResourceLabelValue     = "true"
)

type modelServiceFixtureConfig struct {
	hfTokenSecret string
	hfTokenKey    string
}

// ModelServiceOption overrides fixture conventions used by model-service resources.
type ModelServiceOption func(*modelServiceFixtureConfig)

// WithModelServiceHFTokenSecret overrides the Hugging Face token secret reference.
func WithModelServiceHFTokenSecret(secretName, secretKey string) ModelServiceOption {
	return func(cfg *modelServiceFixtureConfig) {
		if secretName != "" {
			cfg.hfTokenSecret = secretName
		}
		if secretKey != "" {
			cfg.hfTokenKey = secretKey
		}
	}
}

func defaultModelServiceFixtureConfig() modelServiceFixtureConfig {
	return modelServiceFixtureConfig{
		hfTokenSecret: defaultHFTokenSecretName,
		hfTokenKey:    defaultHFTokenSecretKey,
	}
}

func resolveModelServiceFixtureConfig(opts ...ModelServiceOption) modelServiceFixtureConfig {
	cfg := defaultModelServiceFixtureConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
