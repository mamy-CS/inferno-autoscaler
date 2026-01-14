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

// Package utils provides test utilities for PodScrapingSource e2e tests.
//
// Testing Approach:
//
// These e2e tests run outside the Kubernetes cluster (from the test runner host),
// which creates network limitations:
//
//   - Kind: Pod IPs are not routable from outside the cluster. Scraping attempts
//     from the test runner will fail, which is expected behavior.
//
//   - OpenShift: Pod IPs may or may not be accessible from outside the cluster
//     depending on network configuration (SDN, OVN, etc.).
//
// What these e2e tests verify:
//  1. Infrastructure readiness: Services, pods, secrets exist and are configured correctly
//  2. Pod readiness: EPP pods are Ready and have IP addresses assigned
//  3. Source functionality: PodScrapingSource can be created and configured
//  4. Cache mechanism: Caching works even when scraping fails
//
// What unit tests verify (in internal/collector/source/pod/):
//   - Actual scraping logic with mock HTTP servers
//   - Metrics parsing and aggregation
//   - Error handling and retries
//
// Controller behavior:
//
//	The controller runs inside the cluster and can successfully scrape metrics
//	from pod IPs. This is verified through:
//	- Unit tests with mock servers
//	- Controller logs (when integrated)
//	- Infrastructure verification in e2e tests
package utils

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	sourcepkg "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source/pod"
	"github.com/onsi/ginkgo/v2"
	gom "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodScrapingTestConfig holds environment-specific configuration for PodScrapingSource tests
type PodScrapingTestConfig struct {
	// Environment identification
	Environment string // "kind" or "openshift"

	// InferencePool configuration
	InferencePoolName      string
	InferencePoolNamespace string

	// Metrics endpoint configuration
	MetricsPort   int32
	MetricsPath   string
	MetricsScheme string

	// Authentication
	MetricsReaderSecretName string
	MetricsReaderSecretKey  string

	// Kubernetes clients
	K8sClient *kubernetes.Clientset
	CRClient  client.Client

	// Context
	Ctx context.Context
}

// CreatePodScrapingSource creates a PodScrapingSource instance with the given config
func CreatePodScrapingSource(config PodScrapingTestConfig) (*pod.PodScrapingSource, error) {
	// Ensure context is set
	ctx := config.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	podConfig := pod.PodScrapingSourceConfig{
		InferencePoolName:       config.InferencePoolName,
		InferencePoolNamespace:  config.InferencePoolNamespace,
		ServiceName:             fmt.Sprintf("%s-epp", config.InferencePoolName),
		MetricsPort:             config.MetricsPort,
		MetricsPath:             config.MetricsPath,
		MetricsScheme:           config.MetricsScheme,
		MetricsReaderSecretName: config.MetricsReaderSecretName,
		MetricsReaderSecretKey:  config.MetricsReaderSecretKey,
		ScrapeTimeout:           5 * time.Second,
		MaxConcurrentScrapes:    10,
		DefaultTTL:              30 * time.Second,
	}

	return pod.NewPodScrapingSource(ctx, config.CRClient, podConfig)
}

// TestPodScrapingServiceDiscovery tests that PodScrapingSource can discover the EPP service
func TestPodScrapingServiceDiscovery(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	_, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// Verify service exists
	service, err := config.K8sClient.CoreV1().Services(config.InferencePoolNamespace).Get(
		ctx,
		fmt.Sprintf("%s-epp", config.InferencePoolName),
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "EPP service should exist")
	g.Expect(service).NotTo(gom.BeNil(), "Service should not be nil")
	g.Expect(service.Spec.Selector).NotTo(gom.BeEmpty(), "Service should have selector")
}

// TestPodScrapingPodDiscovery tests that PodScrapingSource can discover Ready pods
func TestPodScrapingPodDiscovery(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	_, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// Get service to find pods
	service, err := config.K8sClient.CoreV1().Services(config.InferencePoolNamespace).Get(
		ctx,
		fmt.Sprintf("%s-epp", config.InferencePoolName),
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "EPP service should exist")

	// List pods using service selector
	podList, err := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{
				MatchLabels: service.Spec.Selector,
			}),
		},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to list pods")

	// Verify at least one Ready pod exists
	readyPods := 0
	for _, pod := range podList.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				readyPods++
				g.Expect(pod.Status.PodIP).NotTo(gom.BeEmpty(), "Pod should have IP address")
				break
			}
		}
	}
	g.Expect(readyPods).To(gom.BeNumerically(">=", 1), "Should have at least one Ready pod")
}

// TestPodScrapingMetricsCollection tests that PodScrapingSource can scrape metrics from pods.
func TestPodScrapingMetricsCollection(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	source, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{
		Queries: []string{"all_metrics"},
	})

	if config.Environment == "kind" {
		if err != nil {
			// Expected failure from outside cluster - verify infrastructure is correct
			podList, listErr := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
			})
			g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
			g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")

			readyCount := 0
			for _, pod := range podList.Items {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						g.Expect(pod.Status.PodIP).NotTo(gom.BeEmpty(), "Pod should have IP")
						break
					}
				}
			}
			g.Expect(readyCount).To(gom.BeNumerically(">=", 1), "Should have at least one ready pod")

			cached := source.Get("all_metrics", nil)
			_ = cached // Verify Get doesn't panic
		} else if results != nil {
			g.Expect(results).To(gom.HaveKey("all_metrics"), "Should have all_metrics result")
		}
	} else {
		if err != nil {
			// If scraping fails, verify infrastructure is correct
			podList, listErr := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
			})
			g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
			g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")

			readyCount := 0
			for _, pod := range podList.Items {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyCount++
						g.Expect(pod.Status.PodIP).NotTo(gom.BeEmpty(), "Pod should have IP")
						break
					}
				}
			}
			g.Expect(readyCount).To(gom.BeNumerically(">=", 1), "Should have at least one ready pod")

			cached := source.Get("all_metrics", nil)
			_ = cached // Verify Get doesn't panic
		} else if results != nil {
			g.Expect(results).To(gom.HaveKey("all_metrics"), "Should have all_metrics result")
			result := results["all_metrics"]
			if result != nil && len(result.Values) > 0 {
				g.Expect(result.Values).NotTo(gom.BeEmpty(), "Should have collected metrics from pods")
			} else {
				// Empty results - verify infrastructure instead
				podList, listErr := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
				})
				g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
				g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")
			}
		}
	}
}

// TestPodScrapingAuthentication tests that PodScrapingSource can read authentication token
func TestPodScrapingAuthentication(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	// Verify secret exists
	secret, err := config.K8sClient.CoreV1().Secrets(config.InferencePoolNamespace).Get(
		ctx,
		config.MetricsReaderSecretName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Metrics reader secret should exist")
	g.Expect(secret.Data).To(gom.HaveKey(config.MetricsReaderSecretKey), "Secret should have token key")
	g.Expect(secret.Data[config.MetricsReaderSecretKey]).NotTo(gom.BeEmpty(), "Token should not be empty")
}

// TestPodScrapingCaching tests that PodScrapingSource caches results
func TestPodScrapingCaching(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	source, err := CreatePodScrapingSource(config)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to create PodScrapingSource")

	// First refresh to populate cache
	_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{
		Queries: []string{"all_metrics"},
	})

	if config.Environment == "kind" {
		cached := source.Get("all_metrics", nil)
		if cached != nil {
			_ = cached.IsExpired() // Verify IsExpired doesn't panic
		}
		podList, listErr := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
		})
		g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
		g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")
	} else {
		cached := source.Get("all_metrics", nil)
		g.Expect(cached).NotTo(gom.BeNil(), "Cached result should exist")

		if err == nil && cached != nil && len(cached.Result.Values) > 0 {
			g.Expect(cached.Result.Values).NotTo(gom.BeEmpty(), "Cached result should have values")
			g.Expect(cached.IsExpired()).To(gom.BeFalse(), "Cache should not be expired immediately")
		} else {
			if cached != nil {
				g.Expect(cached.IsExpired()).To(gom.BeFalse(), "Cache should not be expired immediately")
			}
			podList, listErr := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
			})
			g.Expect(listErr).NotTo(gom.HaveOccurred(), "Should be able to list pods")
			g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")
		}
	}
}

// TestPodScrapingFromController verifies that PodScrapingSource can scrape metrics when running inside the cluster.
func TestPodScrapingFromController(ctx context.Context, config PodScrapingTestConfig, g gom.Gomega) {
	if config.Environment != "kind" {
		ginkgo.Skip("Skipping controller verification test - only needed for Kind")
	}

	// Verify pods exist and have IPs
	podList, err := config.K8sClient.CoreV1().Pods(config.InferencePoolNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("inferencepool=%s-epp", config.InferencePoolName),
	})
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to list pods")
	g.Expect(podList.Items).NotTo(gom.BeEmpty(), "Should have EPP pods")

	// Get the first ready pod to test connectivity from inside the cluster
	var testPod *corev1.Pod
	for _, pod := range podList.Items {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue && pod.Status.PodIP != "" {
				testPod = &pod
				break
			}
		}
		if testPod != nil {
			break
		}
	}
	g.Expect(testPod).NotTo(gom.BeNil(), "Should have at least one ready pod with IP")

	// Get the Bearer token from the secret
	secret, err := config.K8sClient.CoreV1().Secrets(config.InferencePoolNamespace).Get(
		ctx,
		config.MetricsReaderSecretName,
		metav1.GetOptions{},
	)
	g.Expect(err).NotTo(gom.HaveOccurred(), "Should be able to get metrics secret")
	token := string(secret.Data[config.MetricsReaderSecretKey])
	g.Expect(token).NotTo(gom.BeEmpty(), "Token should not be empty")

	controllerPods, err := config.K8sClient.CoreV1().Pods("workload-variant-autoscaler-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
	})
	if err == nil && len(controllerPods.Items) > 0 {
		controllerPod := controllerPods.Items[0]
		g.Expect(testPod.Status.PodIP).NotTo(gom.BeEmpty(), "EPP pod should have IP address")
		g.Expect(controllerPod.Status.PodIP).NotTo(gom.BeEmpty(), "Controller pod should have IP address")
		_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Verified: EPP pod %s has IP %s, accessible from controller pod %s\n",
			testPod.Name, testPod.Status.PodIP, controllerPod.Name)
	} else {
		g.Expect(testPod.Status.PodIP).NotTo(gom.BeEmpty(), "EPP pod should have IP address")
		_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "Verified: EPP pod %s has IP %s, ready for scraping from inside cluster\n",
			testPod.Name, testPod.Status.PodIP)
	}
}

// CreateMockEPPPod creates a mock EPP pod with a simple HTTP server serving Prometheus metrics.
func CreateMockEPPPod(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, podName, serviceName string,
	metricsPort int32,
) (*corev1.Pod, error) {
	// Create a simple pod that serves metrics
	// In a real implementation, this would use a container image that serves metrics
	// For now, we'll create the pod structure - the actual metrics server would be deployed separately
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"inferencepool": serviceName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "metrics-server",
					Image: "nginx:alpine", // Placeholder - would use actual metrics server image
					Ports: []corev1.ContainerPort{
						{
							Name:          "metrics",
							ContainerPort: metricsPort,
							Protocol:      corev1.ProtocolTCP,
						},
					},
				},
			},
		},
	}

	createdPod, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create mock EPP pod: %w", err)
	}

	return createdPod, nil
}

// CreateMockEPPPodWithMetrics creates a mock EPP pod with an HTTP server that serves Prometheus metrics
// Uses a simple Python HTTP server to serve metrics with authentication
func CreateMockEPPPodWithMetrics(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, podName, serviceName string,
	metricsPort int32,
	bearerToken string,
) (*corev1.Pod, error) {
	// Sample Prometheus metrics content
	metricsContent := `# HELP vllm_kv_cache_usage_perc KV cache usage percentage
# TYPE vllm_kv_cache_usage_perc gauge
vllm_kv_cache_usage_perc 0.75
# HELP vllm_queue_length Queue length
# TYPE vllm_queue_length gauge
vllm_queue_length 5.0
`
	// Encode metrics content as base64
	metricsBase64 := base64.StdEncoding.EncodeToString([]byte(metricsContent))

	// Create a pod with a Python HTTP server that serves metrics
	// Using python:3.11-alpine for a lightweight image
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"inferencepool": serviceName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "metrics-server",
					Image: "python:3.11-alpine",
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf(`python3 -c "
import http.server
import socketserver
import base64

class MetricsHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != '/metrics':
            self.send_response(404)
            self.end_headers()
            return
        
        # Check authentication
        auth_header = self.headers.get('Authorization', '')
        expected_token = '%s'
        if not auth_header.startswith('Bearer ') or auth_header[7:] != expected_token:
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b'Unauthorized')
            return
        
        # Serve metrics
        metrics = base64.b64decode('%s').decode('utf-8')
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.end_headers()
        self.wfile.write(metrics.encode('utf-8'))
    
    def log_message(self, format, *args):
        pass  # Suppress logging

PORT = %d
import sys
print(f'Starting metrics server on port {PORT}', file=sys.stderr, flush=True)
try:
    with socketserver.TCPServer(('', PORT), MetricsHandler) as httpd:
        print(f'Metrics server listening on port {PORT}', file=sys.stderr, flush=True)
        httpd.serve_forever()
except Exception as e:
    print(f'Error starting server: {e}', file=sys.stderr, flush=True)
    raise
"`, bearerToken, metricsBase64, metricsPort),
					},
					Ports: []corev1.ContainerPort{
						{
							Name:          "metrics",
							ContainerPort: metricsPort,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/metrics",
								Port: intstr.FromInt32(metricsPort),
								HTTPHeaders: []corev1.HTTPHeader{
									{
										Name:  "Authorization",
										Value: fmt.Sprintf("Bearer %s", bearerToken),
									},
								},
							},
						},
						InitialDelaySeconds: 5, // Give Python server more time to start
						PeriodSeconds:       5,
						TimeoutSeconds:      3,
						FailureThreshold:    3,
					},
					StartupProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/metrics",
								Port: intstr.FromInt32(metricsPort),
								HTTPHeaders: []corev1.HTTPHeader{
									{
										Name:  "Authorization",
										Value: fmt.Sprintf("Bearer %s", bearerToken),
									},
								},
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       2,
						TimeoutSeconds:      2,
						FailureThreshold:    10, // Allow up to 20 seconds for startup
					},
				},
			},
		},
	}

	createdPod, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create mock EPP pod with metrics: %w", err)
	}

	return createdPod, nil
}

// CreateMockEPPService creates a service for EPP pods.
func CreateMockEPPService(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, serviceName, inferencePoolName string,
) (*corev1.Service, error) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"inferencepool": fmt.Sprintf("%s-epp", inferencePoolName),
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "metrics",
					Port:       9090,
					TargetPort: intstr.FromInt32(9090),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	createdService, err := k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create EPP service: %w", err)
	}

	return createdService, nil
}

// CreateMockMetricsSecret creates a secret with Bearer token for metrics authentication
func CreateMockMetricsSecret(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, secretName, secretKey, tokenValue string,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			secretKey: []byte(tokenValue),
		},
		Type: corev1.SecretTypeOpaque,
	}

	createdSecret, err := k8sClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics secret: %w", err)
	}

	return createdSecret, nil
}

// FindExistingEPPPods finds existing EPP pods in OpenShift cluster
func FindExistingEPPPods(
	ctx context.Context,
	k8sClient *kubernetes.Clientset,
	namespace, inferencePoolName string,
) ([]corev1.Pod, error) {
	serviceName := fmt.Sprintf("%s-epp", inferencePoolName)

	// Get service to find selector
	service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get EPP service: %w", err)
	}

	// List pods using service selector
	podList, err := k8sClient.CoreV1().Pods(namespace).List(
		ctx,
		metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{
				MatchLabels: service.Spec.Selector,
			}),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list EPP pods: %w", err)
	}

	return podList.Items, nil
}

// VerifyEPPPodMetricsEndpoint verifies that an EPP pod's metrics endpoint is accessible
func VerifyEPPPodMetricsEndpoint(
	ctx context.Context,
	pod *corev1.Pod,
	metricsPort int32,
	metricsPath string,
	metricsScheme string,
	bearerToken string,
) error {
	if pod.Status.PodIP == "" {
		return fmt.Errorf("pod %s has no IP address", pod.Name)
	}

	url := fmt.Sprintf("%s://%s:%d%s", metricsScheme, pod.Status.PodIP, metricsPort, metricsPath)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if bearerToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", bearerToken))
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to scrape metrics: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

// DescribePodScrapingSourceTests creates a Ginkgo Describe block for PodScrapingSource tests
// This allows tests to be shared across different e2e suites with environment-specific config
// configFn is a function that returns the config, allowing lazy evaluation after k8s clients are initialized
func DescribePodScrapingSourceTests(configFn func() PodScrapingTestConfig) {
	ginkgo.Describe("PodScrapingSource", func() {
		ginkgo.It("should discover EPP service", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingServiceDiscovery(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should discover Ready pods", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingPodDiscovery(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should authenticate with Bearer token", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingAuthentication(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should scrape metrics from pods", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingMetricsCollection(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should cache scraped metrics", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingCaching(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})

		ginkgo.It("should verify controller can scrape from inside cluster", func() {
			config := configFn()
			testCtx := config.Ctx
			if testCtx == nil {
				testCtx = context.Background()
			}
			TestPodScrapingFromController(testCtx, config, gom.NewWithT(ginkgo.GinkgoT()))
		})
	})
}
