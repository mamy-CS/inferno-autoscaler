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
	simulatorImage string
	runtimeImage   string
	containerPort  int32
	hfTokenSecret  string
	hfTokenKey     string
	modelLabel     string
	guideLabel     string
	testLabelValue string
}

// ModelServiceOption overrides fixture conventions used by model-service resources.
type ModelServiceOption func(*modelServiceFixtureConfig)

// WithModelServiceImages overrides simulator/runtime container images.
func WithModelServiceImages(simulatorImage, runtimeImage string) ModelServiceOption {
	return func(cfg *modelServiceFixtureConfig) {
		if simulatorImage != "" {
			cfg.simulatorImage = simulatorImage
		}
		if runtimeImage != "" {
			cfg.runtimeImage = runtimeImage
		}
	}
}

// WithModelServiceContainerPort overrides the model server container port.
func WithModelServiceContainerPort(port int32) ModelServiceOption {
	return func(cfg *modelServiceFixtureConfig) {
		if port > 0 {
			cfg.containerPort = port
		}
	}
}

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

// WithModelServiceLabelValues overrides convention label values used by fixtures.
func WithModelServiceLabelValues(modelLabel, guideLabel, testLabelValue string) ModelServiceOption {
	return func(cfg *modelServiceFixtureConfig) {
		if modelLabel != "" {
			cfg.modelLabel = modelLabel
		}
		if guideLabel != "" {
			cfg.guideLabel = guideLabel
		}
		if testLabelValue != "" {
			cfg.testLabelValue = testLabelValue
		}
	}
}

func defaultModelServiceFixtureConfig() modelServiceFixtureConfig {
	return modelServiceFixtureConfig{
		simulatorImage: defaultModelServiceSimulatorImage,
		runtimeImage:   defaultModelServiceRuntimeImage,
		containerPort:  defaultModelServiceContainerPort,
		hfTokenSecret:  defaultHFTokenSecretName,
		hfTokenKey:     defaultHFTokenSecretKey,
		modelLabel:     defaultModelServiceLabelValue,
		guideLabel:     defaultGuideLabelValue,
		testLabelValue: defaultTestResourceLabelValue,
	}
}

func resolveModelServiceFixtureConfig(opts ...ModelServiceOption) modelServiceFixtureConfig {
	cfg := defaultModelServiceFixtureConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
