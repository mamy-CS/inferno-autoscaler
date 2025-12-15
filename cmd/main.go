/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/controller"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/saturation"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/scalefromzero"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	//+kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	utilruntime.Must(promoperator.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	// Server and certificate configuration
	var (
		metricsAddr                                      string
		probeAddr                                        string
		metricsCertPath, metricsCertName, metricsCertKey string
		webhookCertPath, webhookCertName, webhookCertKey string
	)
	// Leader election configuration
	var (
		enableLeaderElection bool
		leaseDuration        time.Duration
		renewDeadline        time.Duration
		retryPeriod          time.Duration
		restTimeout          time.Duration
	)
	// Feature flags
	var (
		secureMetrics bool
		enableHTTP2   bool
	)
	// Other
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// Leader election timeout configuration flags
	// These can be overridden in manager.yaml to tune for different environments
	// (e.g., higher values for environments with network latency or API server slowness)
	flag.DurationVar(&leaseDuration, "leader-election-lease-duration", 60*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership. "+
			"Increased from default 15s to 60s to prevent lease renewal failures in environments with network latency.")
	flag.DurationVar(&renewDeadline, "leader-election-renew-deadline", 50*time.Second,
		"The duration that the acting master will retry refreshing leadership before giving up. "+
			"Increased from default 10s to 50s to provide more tolerance for network latency and API server delays.")
	flag.DurationVar(&retryPeriod, "leader-election-retry-period", 10*time.Second,
		"The duration the clients should wait between tries of actions. "+
			"Increased from default 2s to 10s to reduce API server load and provide more time between renewal attempts.")
	flag.DurationVar(&restTimeout, "rest-client-timeout", 60*time.Second,
		"The timeout for REST API calls to the Kubernetes API server. "+
			"Increased from default ~30s to 60s for better resilience against network latency.")

	flag.Parse()

	setupLog, err := logger.InitLogger()
	if err != nil {
		panic("unable to initialize logger: " + err.Error())
	}
	defer func() {
		if err := setupLog.Sync(); err != nil {
			// Optionally log the error or handle it as needed
			// For now, just print to stderr
			_, _ = os.Stderr.WriteString("error syncing logger: " + err.Error() + "\n")
		}
	}()

	ctrllog.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(false), ctrlzap.WriteTo(os.Stdout)))

	setupLog.Info("Zap logger initialized")

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			zap.String("metrics-cert-path", metricsCertPath),
			zap.String("metrics-cert-name", metricsCertName),
			zap.String("metrics-cert-key", metricsCertKey))

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error("Failed to initialize webhook certificate watcher", zap.Error(err))
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			zap.String("metrics-cert-path", metricsCertPath),
			zap.String("metrics-cert-name", metricsCertName),
			zap.String("metrics-cert-key", metricsCertKey),
		)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error("Failed to initialize metrics certificate watcher", zap.Error(err))
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// Get REST config and configure timeouts to handle network latency
	// This addresses issues with leader election lease renewal failures in environments
	// with higher network latency or API server slowness.
	restConfig := ctrl.GetConfigOrDie()
	// Use configurable REST client timeout (default 60s, can be overridden via --rest-client-timeout flag)
	restConfig.Timeout = restTimeout

	// Configure leader election with configurable timeouts to prevent lease renewal failures
	// Default values are: LeaseDuration=60s, RenewDeadline=50s, RetryPeriod=10s
	// These can be overridden via command-line flags in manager.yaml
	// Increased from controller-runtime defaults (15s, 10s, 2s) to provide more tolerance
	// for network latency and API server delays

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "72dd1cf1.llm-d.ai",
		// Leader election timeout configuration (configurable via flags)
		LeaseDuration: &leaseDuration,
		RenewDeadline: &renewDeadline,
		RetryPeriod:   &retryPeriod,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// This is safe to enable because the program ends immediately after the manager stops
		// (see mgr.Start() call at the end of main()). This enables fast failover during
		// deployments and upgrades, reducing downtime from ~60s to ~1-2s.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error("unable to start manager", zap.Error(err))
		os.Exit(1)
	}

	// Register optimization engine loops with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		engine := saturation.NewEngine(mgr.GetClient())
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error("unable to add optimization engine loops to manager", zap.Error(err))
		os.Exit(1)
	}

	// Register scale from zero engine loops with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		engine := scalefromzero.NewEngine(mgr.GetClient())
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error("unable to add optimization engine loops to manager", zap.Error(err))
		os.Exit(1)
	}

	// Initialize metrics
	logger.Log.Infof("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	logger.Log.Infof("Metrics emitter created successfully")

	// Configure Prometheus client using flexible configuration with TLS support
	promConfig, err := utils.GetPrometheusConfig(context.Background(), mgr.GetClient())
	if err != nil {
		setupLog.Error("failed to get Prometheus configuration", zap.Error(err))
		os.Exit(1)
	}

	// ensure we have a valid configuration
	if promConfig == nil {
		setupLog.Error("no Prometheus configuration found - this should not happen")
		os.Exit(1)
	}

	// Always validate TLS configuration since HTTPS is required
	if err := utils.ValidateTLSConfig(promConfig); err != nil {
		logger.Log.Errorf("TLS configuration validation failed - HTTPS is required: error=%v", err)
		setupLog.Error("TLS configuration validation failed", zap.Error(err))
		os.Exit(1)
	}

	logger.Log.Infof("Initializing Prometheus client -> address: %s, tls_enabled: true", promConfig.BaseURL)

	// Create Prometheus client with TLS support
	promClientConfig, err := utils.CreatePrometheusClientConfig(promConfig)
	if err != nil {
		setupLog.Error("failed to create prometheus client config", zap.Error(err))
		os.Exit(1)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		setupLog.Error("failed to create prometheus client", zap.Error(err))
		os.Exit(1)
	}

	promAPI := promv1.NewAPI(promClient)

	// Validate that the API is working by testing a simple query with retry logic
	if err := utils.ValidatePrometheusAPI(context.Background(), promAPI); err != nil {
		logger.Log.Errorf("CRITICAL: Failed to connect to Prometheus - Inferno requires Prometheus connectivity for autoscaling decisions: error=%v", err)
		setupLog.Error("critical: failed to validate Prometheus API connection", zap.Error(err))
		os.Exit(1)
	}
	logger.Log.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Read Prometheus cache configuration from ConfigMap
	cacheConfig, err := utils.ReadPrometheusCacheConfig(context.Background(), mgr.GetClient())
	if err != nil {
		logger.Log.Warnf("Failed to read Prometheus cache config from ConfigMap, using defaults: %v", err)
		cacheConfig = nil // Use defaults
	}

	// Initialize metrics collector plugin (defaults to Prometheus)
	metricsCollector, err := collector.NewMetricsCollector(collector.Config{
		Type:        collector.CollectorTypePrometheus,
		PromAPI:     promAPI,
		CacheConfig: cacheConfig,
	})
	if err != nil {
		setupLog.Error("failed to create metrics collector", zap.Error(err))
		os.Exit(1)
	}

	// Set K8sClient on the collector if it supports it (for pod discovery in saturation metrics)
	if promCollector, ok := metricsCollector.(*prometheus.PrometheusCollector); ok {
		promCollector.SetK8sClient(mgr.GetClient())

		// Start background fetching executor using manager context
		// This ensures the executor stops gracefully when the manager stops
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			promCollector.StartBackgroundWorker(ctx)
			<-ctx.Done() // Wait for context cancellation
			return nil
		})); err != nil {
			logger.Log.Warnf("Failed to register background fetching executor with manager: %v", err)
		} else {
			logger.Log.Info("Registered background fetching executor with manager")
		}
	}

	logger.Log.Info("Metrics collector initialized successfully")

	// Create the reconciler
	reconciler := &controller.VariantAutoscalingReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Recorder:         mgr.GetEventRecorderFor("workload-variant-autoscaler-controller-manager"),
		PromAPI:          promAPI,
		MetricsCollector: metricsCollector,
	}

	// Setup the controller with the manager
	if err = reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error("unable to create controller", zap.String("controller", "variantautoscaling"), zap.Error(err))
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error("unable to add metrics certificate watcher to manager", zap.Error(err))
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error("unable to add webhook certificate watcher to manager", zap.Error(err))
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up health check", zap.Error(err))
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error("unable to set up ready check", zap.Error(err))
		os.Exit(1)
	}

	setupLog.Info("Starting manager")

	// Sync the custom logger before starting the manager
	if logger.Log != nil {
		//ignore sync errors: https://github.com/uber-go/zap/issues/328
		_ = logger.Log.Sync()
	}

	// Register custom metrics with the controller-runtime Prometheus registry
	// This makes the metrics available for scraping by Prometheus and direct endpoint access
	setupLog.Info("Registering custom metrics with Prometheus registry")
	if err := metrics.InitMetrics(crmetrics.Registry); err != nil {
		setupLog.Error("failed to initialize metrics", zap.Error(err))
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error("problem running manager", zap.Error(err))
		os.Exit(1)
	}
}
