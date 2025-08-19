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

package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/llm-d-incubation/inferno-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/inferno-autoscaler/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	controllerNamespace           = "inferno-autoscaler-system"
	controllerMonitoringNamespace = "inferno-autoscaler-monitoring"
	llmDNamespace                 = "llm-d-sim"
)

const (
	defaultModelId       = "default/default"
	llamaModelId         = "meta/llama0-70b"
	defaultAcc           = "A100"
	maximumAvailableGPUs = 4
)

const loadThresholdDiff = 3.0

var (
	k8sClient *kubernetes.Clientset
	crClient  client.Client
	scheme    = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// initializeK8sClient initializes the Kubernetes client for testing
func initializeK8sClient() {
	cfg, err := func() (*rest.Config, error) {
		if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
			return clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
		return rest.InClusterConfig()
	}()
	if err != nil {
		Skip("failed to load kubeconfig: " + err.Error())
	}

	// Suppress warnings to avoid spam in test output
	cfg.WarningHandler = rest.NoWarnings{}

	k8sClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		Skip("failed to create kubernetes client: " + err.Error())
	}

	// Initialize controller-runtime client for custom resources
	crClient, err = client.New(cfg, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		Skip("failed to create controller-runtime client: " + err.Error())
	}
}

func isPortAvailable(port int) (bool, error) {
	// Try to bind to the port to check if it's available
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return false, err // Port is already in use
	}
	if err := listener.Close(); err != nil {
		Expect(err).NotTo(HaveOccurred(), "Failed to close listener")
	}
	return true, nil // Port is available
}

func startPortForwarding(service *corev1.Service, namespace string, port int) *exec.Cmd {
	// Check if the port is already in use
	if available, err := isPortAvailable(port); !available {
		Fail(fmt.Sprintf("Port %d is already in use. Cannot start port forwarding for service: %s. Error: %v", port, service.Name, err))
	}

	portForwardCmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("service/%s", service.Name),
		fmt.Sprintf("%d:80", port), "-n", namespace)
	err := portForwardCmd.Start()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward command should start successfully for service: %s", service.Name))

	// Check if the port-forward process is still running
	Eventually(func() error {
		if portForwardCmd.ProcessState != nil && portForwardCmd.ProcessState.Exited() {
			return fmt.Errorf("port-forward process exited unexpectedly with code: %d", portForwardCmd.ProcessState.ExitCode())
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(Succeed(), fmt.Sprintf("Port-forward to port %d should keep running for service: %s", port, service.Name))

	return portForwardCmd
}

func startLoadGenerator(rate, contentLength int, port int) *exec.Cmd {
	// Install the load generator requirements
	requirementsCmd := exec.Command("pip", "install", "-r", "hack/vllme/vllm_emulator/requirements.txt")
	_, err := utils.Run(requirementsCmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install loadgen requirements")
	loadGenCmd := exec.Command("python",
		"hack/vllme/vllm_emulator/loadgen.py",
		"--url", fmt.Sprintf("http://localhost:%d/v1", port),
		"--rate", fmt.Sprintf("%d", rate),
		"--content", fmt.Sprintf("%d", contentLength),
		"--model", "vllm")
	err = loadGenCmd.Start()
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to start load generator sending requests to \"http://localhost:%d/v1\"", port))

	// Check if the loadgen process is still running
	Eventually(func() error {
		if loadGenCmd.ProcessState != nil && loadGenCmd.ProcessState.Exited() {
			return fmt.Errorf("load generator exited unexpectedly with code: %d", loadGenCmd.ProcessState.ExitCode())
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(Succeed(), fmt.Sprintf("Load generator sending requests to \"http://localhost:%d/v1\" should keep running", port))

	return loadGenCmd
}

func stopCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("command or process is nil")
	}

	// Try graceful shutdown with SIGINT
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("failed to send interrupt signal: %w", err)
	}

	// Wait for graceful shutdown with timeout
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		// Process exited gracefully, return any error from Wait()
		if err != nil {
			return fmt.Errorf("process exited with error: %w", err)
		}
		return nil
	case <-time.After(5 * time.Second):
		// Timeout - force kill
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}
		// Wait for the kill to complete
		<-done
		return nil
	}
}

// validateAppLabelUniqueness checks if the appLabel is already in use by other resources and fails if it's not unique
func validateAppLabelUniqueness(namespace, appLabel string) {
	// Create a context with timeout to prevent hanging tests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check if any pods exist with the specified app label
	podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		Fail(fmt.Sprintf("Failed to check existing pods for label uniqueness: %v", err))
	}

	// Check if any deployments exist with the specified app label
	deploymentList, err := k8sClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		Fail(fmt.Sprintf("Failed to check existing deployments for label uniqueness: %v", err))
	}

	// Check if any services exist with the specified app label
	serviceList, err := k8sClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", appLabel),
	})
	if err != nil {
		Fail(fmt.Sprintf("Failed to check existing services for label uniqueness: %v", err))
	}

	// Check if any ServiceMonitors exist with the specified app label
	serviceMonitorList := &unstructured.UnstructuredList{}
	serviceMonitorList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	err = crClient.List(ctx, serviceMonitorList, client.InNamespace(namespace), client.MatchingLabels{"app": appLabel})
	if err != nil {
		Fail(fmt.Sprintf("Failed to check existing ServiceMonitors for label uniqueness: %v", err))
	}

	// Collects conflicting resources to show in error logs
	var conflicting []string

	if len(podList.Items) > 0 {
		for _, pod := range podList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Pod: %s", pod.Name))
		}
	}

	if len(deploymentList.Items) > 0 {
		for _, deployment := range deploymentList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Deployment: %s", deployment.Name))
		}
	}

	if len(serviceList.Items) > 0 {
		for _, service := range serviceList.Items {
			conflicting = append(conflicting, fmt.Sprintf("Service: %s", service.Name))
		}
	}

	if len(serviceMonitorList.Items) > 0 {
		for _, serviceMonitor := range serviceMonitorList.Items {
			name, found, err := unstructured.NestedString(serviceMonitor.Object, "metadata", "name")
			if err != nil {
				Fail(fmt.Sprintf("Wrong ServiceMonitor name: %v", err))
			} else if !found {
				Fail("ServiceMonitor name not found")
			}
			conflicting = append(conflicting, fmt.Sprintf("ServiceMonitor: %s", name))
		}
	}

	// Fails if any conflicts are found
	if len(conflicting) > 0 {
		Fail(fmt.Sprintf("App label '%s' is not unique in namespace '%s'. Make sure to delete conflicting resources: %s",
			appLabel, namespace, strings.Join(conflicting, ", ")))
	}
}

// validateVariantAutoscalingUniqueness checks if the VariantAutoscaling configuration is unique within the namespace
func validateVariantAutoscalingUniqueness(namespace, modelId, acc string) {
	// Create a context with timeout to prevent hanging tests
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	variantAutoscalingList := &v1alpha1.VariantAutoscalingList{}
	err := crClient.List(ctx, variantAutoscalingList, client.InNamespace(namespace), client.MatchingLabels{"inference.optimization/acceleratorName": acc})
	if err != nil {
		Fail(fmt.Sprintf("Failed to check existing VariantAutoscalings for accelerator label uniqueness: %v", err))
	}

	// found VAs with the same accelerator
	if len(variantAutoscalingList.Items) > 0 {
		var conflicting []string
		for _, va := range variantAutoscalingList.Items {
			// check for same modelId
			if va.Spec.ModelID == modelId {
				conflicting = append(conflicting, fmt.Sprintf("VariantAutoscaling: %s", va.Name))
			}
		}
		// Fails if any conflicts are found
		if len(conflicting) > 0 {
			Fail(fmt.Sprintf("VariantAutoscaling '%s' is not unique in namespace '%s'. Make sure to delete conflicting VAs: %s",
				modelId, namespace, strings.Join(conflicting, ", ")))
		}
	}
}

// creates a vllme deployment with the specified configuration
func createVllmeDeployment(namespace, deployName, modelName, appLabel string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                       appLabel,
						"llm-d.ai/inferenceServing": "true",
						"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "vllme",
							Image:           "quay.io/infernoautoscaler/vllme:0.2.1-multi-arch",
							ImagePullPolicy: corev1.PullAlways,
							Ports: []corev1.ContainerPort{
								{ContainerPort: 80},
							},
							Env: []corev1.EnvVar{
								{Name: "MODEL_NAME", Value: modelName},
								{Name: "DECODE_TIME", Value: "20"},
								{Name: "PREFILL_TIME", Value: "20"},
								{Name: "MODEL_SIZE", Value: "25000"},
								{Name: "KVC_PER_TOKEN", Value: "2"},
								{Name: "MAX_SEQ_LEN", Value: "2048"},
								{Name: "MEM_SIZE", Value: "80000"},
								{Name: "AVG_TOKENS", Value: "128"},
								{Name: "TOKENS_DISTRIBUTION", Value: "deterministic"},
								{Name: "MAX_BATCH_SIZE", Value: "8"},
								{Name: "REALTIME", Value: "True"},
								{Name: "MUTE_PRINT", Value: "False"},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:                    resource.MustParse("500m"),
									corev1.ResourceMemory:                 resource.MustParse("1Gi"),
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                    resource.MustParse("100m"),
									corev1.ResourceMemory:                 resource.MustParse("500Mi"),
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

// creates a service for the vllme deployment
func createVllmeService(namespace, serviceName, appLabel string, nodePort int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Labels: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":                       appLabel,
				"llm-d.ai/inferenceServing": "true",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       appLabel,
					Port:       80,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(80),
					NodePort:   int32(nodePort),
				},
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}
}

// creates a VariantAutoscaling resource with owner reference to deployment
func createVariantAutoscalingResource(namespace, resourceName, modelId, acc string) *v1alpha1.VariantAutoscaling {
	return &v1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: namespace,
			Labels: map[string]string{
				"inference.optimization/acceleratorName": acc,
			},
		},
		Spec: v1alpha1.VariantAutoscalingSpec{
			ModelID: modelId,
			SLOClassRef: v1alpha1.ConfigMapKeyRef{
				Name: "premium",
				Key:  "slo",
			},
			ModelProfile: v1alpha1.ModelProfile{
				Accelerators: []v1alpha1.AcceleratorProfile{
					{
						Acc:          "A100",
						AccCount:     1,
						Alpha:        "20.58",
						Beta:         "0.41",
						MaxBatchSize: 4,
					},
					{
						Acc:          "MI300X",
						AccCount:     1,
						Alpha:        "7.77",
						Beta:         "0.15",
						MaxBatchSize: 4,
					},
					{
						Acc:          "G2",
						AccCount:     1,
						Alpha:        "17.15",
						Beta:         "0.34",
						MaxBatchSize: 4,
					},
				},
			},
		},
	}
}

// creates a ServiceMonitor for vllme metrics collection
func createVllmeServiceMonitor(name, appLabel string) *unstructured.Unstructured {
	serviceMonitor := &unstructured.Unstructured{}
	serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	})
	serviceMonitor.SetName(name)
	serviceMonitor.SetNamespace(controllerMonitoringNamespace)
	serviceMonitor.SetLabels(map[string]string{
		"app":     appLabel,
		"release": "kube-prometheus-stack",
	})

	spec := map[string]any{
		"selector": map[string]any{
			"matchLabels": map[string]any{
				"app": appLabel,
			},
		},
		"endpoints": []any{
			map[string]any{
				"port":     appLabel,
				"path":     "/metrics",
				"interval": "15s",
			},
		},
		"namespaceSelector": map[string]any{
			"any": true,
		},
	}
	serviceMonitor.Object["spec"] = spec

	return serviceMonitor
}

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", controllerNamespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", controllerNamespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})
		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})

var _ = Describe("Test Inferno-autoscaler with vllme deployment - single VA - critical requests", Ordered, func() {
	var (
		namespace      string
		deployName     string
		serviceName    string
		serviceMonName string
		appLabel       string
		loadGenCmd     *exec.Cmd
		portForwardCmd *exec.Cmd
		port           int
		loadRate       int
		ctx            context.Context
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		deployName = "vllme-deployment"
		serviceName = "vllme-service"
		serviceMonName = "vllme-servicemonitor"
		appLabel = "vllme"
		port = 8000
		loadRate = 50

		By("ensuring unique app label for deployment and service")
		validateAppLabelUniqueness(namespace, appLabel)
		validateVariantAutoscalingUniqueness(namespace, defaultModelId, defaultAcc)

		By("creating vllme deployment")
		deployment := createVllmeDeployment(namespace, deployName, defaultModelId, appLabel)
		_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", deployName))

		By("creating vllme service")
		service := createVllmeService(namespace, serviceName, appLabel, 30000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Service: %s", serviceName))

		By("creating ServiceMonitor for vllme metrics")
		serviceMonitor := createVllmeServiceMonitor(serviceMonName, appLabel)
		err = crClient.Create(ctx, serviceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", serviceMonName))

		By("creating VariantAutoscaling resource")
		variantAutoscaling := createVariantAutoscalingResource(namespace, deployName, defaultModelId, defaultAcc)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create VariantAutoscaling for: %s", deployName))

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployment should be running", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 4*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))
	})

	It("deployment should have correct deployment labels", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying deployment selector labels")
		selector := deployment.Spec.Selector.MatchLabels
		Expect(selector).To(HaveKeyWithValue("app", appLabel))
		Expect(selector).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(selector).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))

		By("verifying pod template labels")
		podLabels := deployment.Spec.Template.Labels
		Expect(podLabels).To(HaveKeyWithValue("app", appLabel))
		Expect(podLabels).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(podLabels).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))
	})

	It("deployment should have correct resource configuration", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		By("verifying container resource limits")
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))

		By("verifying environment variables")
		var modelNameEnv, maxBatchSizeEnv *corev1.EnvVar
		for _, env := range container.Env {
			if env.Name == "MODEL_NAME" {
				modelNameEnv = &env
			}
			if env.Name == "MAX_BATCH_SIZE" {
				maxBatchSizeEnv = &env
			}
		}
		Expect(modelNameEnv).NotTo(BeNil(), "MODEL_NAME environment variable should be set")
		Expect(modelNameEnv.Value).To(Equal("default/default"))
		Expect(maxBatchSizeEnv).NotTo(BeNil(), "MAX_BATCH_SIZE environment variable should be set")
		Expect(maxBatchSizeEnv.Value).To(Equal("8"))
	})

	It("deployment should have corresponding service with correct selector", func() {
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", serviceName))

		By("verifying service selector")
		Expect(service.Spec.Selector).To(HaveKeyWithValue("app", appLabel))
	})

	It("deployment should create pods with correct labels", func() {
		Eventually(func() ([]corev1.Pod, error) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + appLabel,
			})
			if err != nil {
				return nil, err
			}
			return podList.Items, nil
		}, 2*time.Minute, 5*time.Second).Should(HaveLen(1))

		podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + appLabel,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(podList.Items).To(HaveLen(1))

		pod := podList.Items[0]
		By("verifying pod labels")
		Expect(pod.Labels).To(HaveKeyWithValue("app", appLabel))
		Expect(pod.Labels).To(HaveKeyWithValue("llm-d.ai/inferenceServing", "true"))
		Expect(pod.Labels).To(HaveKeyWithValue("llm-d.ai/model", "ms-sim-llm-d-modelservice"))
	})

	It("should have correct GPU resource requests", func() {
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))

		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceName("nvidia.com/gpu"), resource.MustParse("1")))
	})

	It("should have VariantAutoscaling resource created", func() {
		By("verifying VariantAutoscaling resource exists")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling: %s", deployName))

		By("verifying VariantAutoscaling spec")
		Expect(variantAutoscaling.Spec.ModelID).To(Equal("default/default"))
		Expect(variantAutoscaling.Spec.SLOClassRef.Name).To(Equal("premium"))
		Expect(variantAutoscaling.Spec.ModelProfile.Accelerators).To(HaveLen(3))
	})

	It("should have VariantAutoscaling with correct ownerReference to Deployment", func() {
		By("first ensuring both Deployment and VariantAutoscaling exist")
		var deployment *appsv1.Deployment
		Eventually(func(g Gomega) {
			var err error
			deployment, err = k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment for: %s", deployName))
			g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1), fmt.Sprintf("Deployment: %s should have ready replicas", deployment.Name))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("ensuring VariantAutoscaling resource exists")
		Eventually(func(g Gomega) {
			variantAutoscaling := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("VariantAutoscaling should exist for: %s", deployName))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for VariantAutoscaling to have ownerReference set by controller")
		Eventually(func(g Gomega) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment for: %s", deployName))

			variantAutoscaling := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			g.Expect(variantAutoscaling.OwnerReferences).To(HaveLen(1), fmt.Sprintf("VariantAutoscaling should have exactly one ownerReference related to: %s", deployName))

			ownerRef := variantAutoscaling.OwnerReferences[0]
			g.Expect(ownerRef.APIVersion).To(Equal("apps/v1"), fmt.Sprintf("ownerReference should have correct APIVersion for: %s", deployName))
			g.Expect(ownerRef.Kind).To(Equal("Deployment"), fmt.Sprintf("ownerReference should point to a Deployment for: %s", deployName))
			g.Expect(ownerRef.Name).To(Equal(deployment.Name), fmt.Sprintf("ownerReference should point to the correct Deployment name for: %s", deployName))
			g.Expect(ownerRef.UID).To(Equal(deployment.UID), fmt.Sprintf("ownerReference should have the correct UID for: %s", deployName))
			g.Expect(ownerRef.Controller).NotTo(BeNil(), fmt.Sprintf("ownerReference should have Controller field set for: %s", deployName))
			g.Expect(*ownerRef.Controller).To(BeTrue(), fmt.Sprintf("ownerReference Controller should be true for: %s", deployName))
		}, 4*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("should scale up optimized replicas when load increases", func() {
		By("verifying initial state of VariantAutoscaling")
		initialVA := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, initialVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

		By("getting the service endpoint for load generation")
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Service: %s", serviceName))

		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")

		portForwardCmd = startPortForwarding(service, namespace, port)
		defer func() {
			err = stopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", serviceName))
		}()

		By("waiting for port-forward to be ready")
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("starting load generation to create traffic")
		loadGenCmd = startLoadGenerator(loadRate, 100, port)
		defer func() {
			err = stopCmd(loadGenCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", deployName))
		}()

		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			// Verify that the optimized allocation has been computed
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 0),
				fmt.Sprintf("DesiredOptimizedAlloc should have calculated optimized replicas for: %s - actual replicas: %d", va.Name, va.Status.DesiredOptimizedAlloc.NumReplicas))

			// Verify that the number of replicas has scaled up
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", va.Name, va.Status.CurrentAlloc.NumReplicas))

			// Verify load detection accuracy
			g.Expect(strconv.ParseFloat(va.Status.CurrentAlloc.Load.ArrivalRate, 64)).To(BeNumerically("~", loadRate, loadThresholdDiff), fmt.Sprintf("Detected load rate: %s should be approximately the actual load rate: %d", va.Status.CurrentAlloc.Load.ArrivalRate, loadRate))

		}, 4*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		finalVA := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, finalVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA.Name,
			finalVA.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Current Allocation for VA: %s - Replicas: %d, Accelerator: %s, \n",
			finalVA.Name,
			finalVA.Status.CurrentAlloc.NumReplicas,
			finalVA.Status.CurrentAlloc.Accelerator)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA.Name,
			finalVA.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA.Status.DesiredOptimizedAlloc.Accelerator)

		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment: %s", deployName))
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			deployName,
			deployment.Status.Replicas)
	})

	It("should keep the same replicas if the load stays constant", func() {
		service, err := k8sClient.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Service: %s", serviceName))

		By("setting up port-forward to the vllme service")
		portForwardCmd = startPortForwarding(service, namespace, port)
		defer func() {
			err = stopCmd(portForwardCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", serviceName))
		}()
		By("waiting for port-forward to be ready")
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")

		By("restarting load generation at the same rate")
		loadGenCmd = startLoadGenerator(loadRate, 100, port)
		defer func() {
			err = stopCmd(loadGenCmd)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", deployName))
		}()

		By("getting the current number of replicas")
		var initialReplicas int32
		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", deployName))
		initialReplicas = deployment.Status.Replicas

		By("verifying that the number of replicas remains constant over several minutes with constant load")
		Consistently(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment: %s", deployName))

			// Verify that the deployment replicas remain stable
			g.Expect(deployment.Status.Replicas).To(Equal(initialReplicas),
				fmt.Sprintf("Deployment replicas for %s should stay at %d replicas with constant load equal to %s", deployName, initialReplicas, va.Status.CurrentAlloc.Load.ArrivalRate))

			// Verify that the desired allocation also remains stable
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(Equal(int(initialReplicas)),
				fmt.Sprintf("DesiredOptimizedAlloc for VA %s should stay at %d replicas with constant load equal to %s", deployName, initialReplicas, va.Status.CurrentAlloc.Load.ArrivalRate))

		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		finalVA := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, finalVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", deployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA.Name,
			finalVA.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA.Name,
			finalVA.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA.Status.DesiredOptimizedAlloc.Accelerator)

		deployment, err = k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("Current replicas for Deployment - %s: %d -- initial replicas: %d\n",
			deployName,
			deployment.Status.Replicas,
			initialReplicas)
	})

	It("should scale down with no load", func() {
		By("waiting for scaling down decision to be made")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

			// Verify that the number of replicas has scaled down to 1
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", 1),
				fmt.Sprintf("No load should trigger scale-down recommendation for: %s", va.Name))

		}, 6*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		finalVA := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      deployName,
		}, finalVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", deployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA.Name,
			finalVA.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA.Name,
			finalVA.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA.Status.DesiredOptimizedAlloc.Accelerator)

		deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch Deployment: %s", deployName))
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			deployName,
			deployment.Status.Replicas)
	})

	It("should connect to Prometheus using HTTPS with TLS", func() {
		By("verifying Prometheus is accessible via HTTPS")
		Eventually(func(g Gomega) {
			// Check if Prometheus service is running with TLS
			service, err := k8sClient.CoreV1().Services("inferno-autoscaler-monitoring").Get(ctx, "kube-prometheus-stack-prometheus", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Prometheus service should exist")
			g.Expect(service.Spec.Ports).To(ContainElement(HaveField("Port", int32(9090))), "Prometheus should be listening on port 9090")

			// Verify TLS secret exists
			secret, err := k8sClient.CoreV1().Secrets("inferno-autoscaler-monitoring").Get(ctx, "prometheus-tls", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "TLS secret should exist")
			g.Expect(secret.Data).To(HaveKey("tls.crt"), "TLS secret should contain certificate")
			g.Expect(secret.Data).To(HaveKey("tls.key"), "TLS secret should contain private key")

		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying controller can connect to Prometheus with TLS")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=inferno-autoscaler",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list controller pods")
			g.Expect(pods.Items).NotTo(BeEmpty(), "Controller pods should exist")

			// Check logs for TLS-related messages
			pod := pods.Items[0]
			logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				// Get all logs instead of just tail lines to find the TLS message from startup
			}).DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get controller logs")

			logString := string(logs)
			g.Expect(logString).To(ContainSubstring("TLS configuration applied to Prometheus HTTPS transport"),
				"Controller should log TLS configuration")
			g.Expect(logString).NotTo(ContainSubstring("http: server gave HTTP response to HTTPS client"),
				"Controller should not have HTTP/HTTPS mismatch errors")

		}, 4*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("should handle TLS certificate verification correctly", func() {
		By("verifying TLS configuration in controller ConfigMap")
		configMap, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, "inferno-autoscaler-variantautoscaling-config", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "ConfigMap should exist")

		// Verify HTTPS URL is configured
		Expect(configMap.Data["PROMETHEUS_BASE_URL"]).To(ContainSubstring("https://"),
			"Prometheus URL should use HTTPS")

		// Verify TLS settings are configured
		Expect(configMap.Data["PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"]).To(Equal("true"),
			"TLS insecure skip verify should be enabled for e2e tests")

		By("verifying controller startup with TLS configuration")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=inferno-autoscaler",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list controller pods")

			for _, pod := range pods.Items {
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
					fmt.Sprintf("Pod %s should be running", pod.Name))
			}
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("should have VariantAutoscaling deleted when Deployment is deleted", func() {
		By("deleting the Deployment")
		err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		By("verifying VariantAutoscaling is deleted due to owner reference")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		Eventually(func() error {
			return crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      deployName,
			}, variantAutoscaling)
		}, 4*time.Minute, 2*time.Second).Should(HaveOccurred(), fmt.Sprintf("VariantAutoscaling for: %s should be deleted", deployName))
	})

	AfterAll(func() {
		By("deleting VariantAutoscaling resource")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployName,
				Namespace: namespace,
			},
		}
		err := crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", deployName))

		By("deleting ServiceMonitor")
		serviceMonitor := &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(serviceMonName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", serviceMonName))

		By("deleting vllme service")
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", serviceName))

		By("deleting vllme deployment")
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", deployName))

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + appLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", appLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())

		By("cleaning up Prometheus operator resources")
		cmd := exec.Command("kubectl", "delete", "-f", "hack/vllme/deploy/prometheus-operator/prometheus-deploy-all-in-one.yaml", "--ignore-not-found=true")
		output, err := utils.Run(cmd)
		if err != nil {
			fmt.Printf("Prometheus cleanup output: %s\n", output)
		}
	})
})

var _ = Describe("Test Inferno-autoscaler with vllme deployment - multiple VAs - critical requests", Ordered, func() {
	var (
		namespace                string
		firstDeployName          string
		secondDeployName         string
		firstAppLabel            string
		secondAppLabel           string
		firstServiceName         string
		firstServiceMonitorName  string
		secondServiceName        string
		secondServiceMonitorName string
		ctx                      context.Context
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		By("ensuring unique app labels for deployment and service")
		validateAppLabelUniqueness(namespace, firstAppLabel)
		validateAppLabelUniqueness(namespace, secondAppLabel)
		validateVariantAutoscalingUniqueness(namespace, defaultModelId, defaultAcc)
		validateVariantAutoscalingUniqueness(namespace, llamaModelId, defaultAcc)

		ctx = context.Background()
		namespace = llmDNamespace
		firstDeployName = "vllme-deployment-1"
		firstAppLabel = "vllme-1"
		firstServiceName = "vllme-service-1"
		firstServiceMonitorName = "vllme-servicemonitor-1"
		secondDeployName = "vllme-deployment-2"
		secondServiceName = "vllme-service-2"
		secondServiceMonitorName = "vllme-servicemonitor-2"
		secondAppLabel = "vllme-2"

		By("creating resources for the first deployment")
		deployment := createVllmeDeployment(namespace, firstDeployName, defaultModelId, firstAppLabel)
		_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first Deployment: %s", firstDeployName))

		firstService := createVllmeService(namespace, firstServiceName, firstAppLabel, 30000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, firstService, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first Service: %s", firstServiceName))

		firstServiceMonitor := createVllmeServiceMonitor(firstServiceMonitorName, firstAppLabel)
		err = crClient.Create(ctx, firstServiceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first ServiceMonitor: %s", firstServiceMonitorName))

		variantAutoscaling := createVariantAutoscalingResource(namespace, firstDeployName, defaultModelId, defaultAcc)
		err = crClient.Create(ctx, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create first VariantAutoscaling for: %s", firstDeployName))

		By("creating resources for the second deployment")
		secondDeployment := createVllmeDeployment(namespace, secondDeployName, llamaModelId, secondAppLabel)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, secondDeployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second Deployment: %s", secondDeployName))

		secondVariantAutoscaling := createVariantAutoscalingResource(namespace, secondDeployName, llamaModelId, defaultAcc)
		err = crClient.Create(ctx, secondVariantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second VariantAutoscaling for: %s", secondDeployName))

		secondService := createVllmeService(namespace, secondServiceName, secondAppLabel, 30001)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, secondService, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second Service: %s", secondServiceName))

		secondServiceMonitor := createVllmeServiceMonitor(secondServiceMonitorName, secondAppLabel)
		err = crClient.Create(ctx, secondServiceMonitor)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create second ServiceMonitor: %s", secondServiceMonitorName))

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	})

	It("deployments should be running", func() {
		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 4*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))

		Eventually(func() (appsv1.DeploymentStatus, error) {
			deployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
			if err != nil {
				return appsv1.DeploymentStatus{}, err
			}
			return deployment.Status, nil
		}, 4*time.Minute, 10*time.Second).Should(And(
			HaveField("ReadyReplicas", BeNumerically("==", 1)),
			HaveField("Replicas", BeNumerically("==", 1)),
		))
	})

	It("should have VariantAutoscaling resource created", func() {
		By("verifying VariantAutoscaling resources exist")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		variantAutoscaling = &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, variantAutoscaling)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", secondDeployName))
	})

	It("should scale up optimized replicas when load increases", func() {
		By("verifying initial state of VariantAutoscaling")
		initialVA := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, initialVA)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		By("getting the first service endpoint for load generation")
		firstService, err := k8sClient.CoreV1().Services(namespace).Get(ctx, firstServiceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")
		port1 := 8000
		portForwardCmd1 := startPortForwarding(firstService, namespace, port1)
		defer func() {
			err = stopCmd(portForwardCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for Service: %s", firstServiceName))
		}()

		By("getting the second service endpoint for load generation")
		secondService, err := k8sClient.CoreV1().Services(namespace).Get(ctx, secondServiceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", secondServiceName))

		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")
		port2 := 8001
		portForwardCmd2 := startPortForwarding(secondService, namespace, port2)
		defer func() {
			err = stopCmd(portForwardCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for Service: %s", secondServiceName))
		}()

		By("waiting for port-forwards to be ready")
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port1))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), "Port-forward should be ready within timeout")
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port2))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward should be ready within timeout for Service: %s", secondServiceName))

		By("starting load generation to create traffic for both deployments")
		loadRate := 30
		loadGenCmd1 := startLoadGenerator(loadRate, 100, port1)
		defer func() {
			err = stopCmd(loadGenCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", firstServiceName))
		}()
		loadGenCmd2 := startLoadGenerator(loadRate, 100, port2)
		defer func() {
			err = stopCmd(loadGenCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", secondServiceName))
		}()

		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s", va1.Name))

			// Verify load detection accuracy for first deployment
			g.Expect(strconv.ParseFloat(va1.Status.CurrentAlloc.Load.ArrivalRate, 64)).To(BeNumerically("~", loadRate, loadThresholdDiff), fmt.Sprintf("Detected load rate %s should be approximately the actual load rate %d for %s", va1.Status.CurrentAlloc.Load.ArrivalRate, loadRate, firstDeployName))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">", 1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s", va2.Name))

			// Verify load detection accuracy for second deployment
			g.Expect(strconv.ParseFloat(va2.Status.CurrentAlloc.Load.ArrivalRate, 64)).To(BeNumerically("~", loadRate, loadThresholdDiff), fmt.Sprintf("Detected load rate %s should be approximately the actual load rate %d for %s", va2.Status.CurrentAlloc.Load.ArrivalRate, loadRate, secondDeployName))

		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		finalVA1 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, finalVA1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA1.Name,
			finalVA1.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA1.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA1.Name,
			finalVA1.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA1.Status.DesiredOptimizedAlloc.Accelerator)

		deployment1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", firstDeployName))
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			firstDeployName,
			deployment1.Status.Replicas)

		finalVA2 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, finalVA2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", secondDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA2.Name,
			finalVA2.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA2.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA2.Name,
			finalVA2.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA2.Status.DesiredOptimizedAlloc.Accelerator)

		deployment2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			secondDeployName,
			deployment2.Status.Replicas)
	})

	PIt("should not scale up optimized replicas over cluster limits", func() {
		// PENDING: This test is disabled because Inferno no longer enforces cluster capacity constraints.
		// Cluster capacity limits are now the responsibility of external autoscalers (e.g., HPA).
		// Inferno's role is limited to emitting optimization metrics for external consumption.
		By("verifying initial state of VariantAutoscaling")
		initialVA1 := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, initialVA1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", firstDeployName))

		initialVA2 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, initialVA2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get VariantAutoscaling for: %s", secondDeployName))

		initialOptimized1 := initialVA1.Status.DesiredOptimizedAlloc.NumReplicas
		initialOptimized2 := initialVA2.Status.DesiredOptimizedAlloc.NumReplicas

		deployment1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", firstDeployName))

		deployment2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", secondDeployName))

		initialReplicas1 := deployment1.Status.Replicas
		initialReplicas2 := deployment2.Status.Replicas

		By("getting the first service endpoint for load generation")
		firstService, err := k8sClient.CoreV1().Services(namespace).Get(ctx, firstServiceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", firstServiceName))

		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")
		port1 := 8000
		portForwardCmd1 := startPortForwarding(firstService, namespace, port1)
		defer func() {
			err = stopCmd(portForwardCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", firstServiceName))
		}()

		By("getting the second service endpoint for load generation")
		secondService, err := k8sClient.CoreV1().Services(namespace).Get(ctx, secondServiceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Service: %s", secondServiceName))

		// Port-forward the vllme service to send requests to it
		By("setting up port-forward to the vllme service")
		port2 := 8001
		portForwardCmd2 := startPortForwarding(secondService, namespace, port2)
		defer func() {
			err = stopCmd(portForwardCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop port-forwarding for: %s", secondServiceName))
		}()

		By("waiting for port-forwards to be ready")
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port1))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward should be ready within timeout for: %s", firstServiceName))
		err = wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			// Try to connect to the forwarded port
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://localhost:%d/v1", port2))
			if err != nil {
				return false, nil // Retrying
			}
			defer func() {
				err := resp.Body.Close()
				Expect(err).NotTo(HaveOccurred(), "Should be able to close response body")
			}()
			return resp.StatusCode < 500, nil // Accept any non-server error status
		})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Port-forward should be ready within timeout for: %s", secondServiceName))

		By("starting load generation to create traffic for both deployments")
		loadRate := 30
		loadGenCmd1 := startLoadGenerator(loadRate, 100, port1)
		defer func() {
			err = stopCmd(loadGenCmd1)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", firstDeployName))
		}()
		higherLoadRate := 50
		loadGenCmd2 := startLoadGenerator(higherLoadRate, 100, port2)
		defer func() {
			err = stopCmd(loadGenCmd2)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to stop load generator sending requests to: %s", secondDeployName))
		}()

		By("waiting for load to be processed and scaling decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", initialOptimized1),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", firstDeployName, va1.Status.DesiredOptimizedAlloc.NumReplicas))

			deployment1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", firstDeployName))
			g.Expect(deployment1.Status.Replicas).To(BeNumerically(">=", initialReplicas1), fmt.Sprintf("Deployment %s should have scaled up - actual replicas: %d", deployment1.Name, deployment1.Status.Replicas))
			g.Expect(strconv.ParseFloat(va1.Status.CurrentAlloc.Load.ArrivalRate, 64)).To(BeNumerically("~", loadRate, loadThresholdDiff), fmt.Sprintf("Detected load rate %s should be approximately the actual load rate %d",
				va1.Status.CurrentAlloc.Load.ArrivalRate, loadRate))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled up
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", initialOptimized2),
				fmt.Sprintf("High load should trigger scale-up recommendation for VA: %s - actual replicas: %d", secondDeployName, va2.Status.DesiredOptimizedAlloc.NumReplicas))

			deployment2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", secondDeployName))
			g.Expect(deployment2.Status.Replicas).To(BeNumerically(">=", initialReplicas2), fmt.Sprintf("Deployment %s should have scaled up - actual replicas: %d", deployment2.Name, deployment2.Status.Replicas))
			g.Expect(strconv.ParseFloat(va2.Status.CurrentAlloc.Load.ArrivalRate, 64)).To(BeNumerically("~", higherLoadRate, loadThresholdDiff), fmt.Sprintf("Detected load rate %s should be approximately the actual load rate %d",
				va2.Status.CurrentAlloc.Load.ArrivalRate, higherLoadRate))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("showing the status of VAs and deployments, including the number of pods in pending state")
		finalVA1 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, finalVA1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA1.Name,
			finalVA1.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA1.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA1.Name,
			finalVA1.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA1.Status.DesiredOptimizedAlloc.Accelerator)

		deployment1, err = k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", firstDeployName))
		fmt.Printf("Current replicas for Deployment - %s: %d\n Available Replicas: %d\n Unavailable replicas: %d\n",
			firstDeployName,
			deployment1.Status.Replicas,
			deployment1.Status.AvailableReplicas,
			deployment1.Status.UnavailableReplicas)

		finalVA2 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, finalVA2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA2.Name,
			finalVA2.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA2.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA2.Name,
			finalVA2.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA2.Status.DesiredOptimizedAlloc.Accelerator)

		deployment2, err = k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to get Deployment: %s", secondDeployName))
		fmt.Printf("Current replicas for Deployment - %s: %d\n Available Replicas: %d\n Unavailable replicas: %d\n",
			secondDeployName,
			deployment2.Status.Replicas,
			deployment2.Status.AvailableReplicas,
			deployment2.Status.UnavailableReplicas)
	})

	It("should scale down with no load", func() {
		By("waiting for scaling down decision to be made")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      firstDeployName,
			}, va1)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

			// Verify that the number of replicas has scaled down to 1
			g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", 1),
				fmt.Sprintf("No load should trigger scale-down recommendation for VA: %s - actual replicas: %d", firstDeployName, va1.Status.CurrentAlloc.NumReplicas))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{
				Namespace: namespace,
				Name:      secondDeployName,
			}, va2)
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

			// Verify that the number of replicas has scaled down to 1
			g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically("==", 1),
				fmt.Sprintf("No load should trigger scale-down recommendation for VA: %s - actual replicas: %d", secondDeployName, va2.Status.CurrentAlloc.NumReplicas))

		}, 4*time.Minute, 10*time.Second).Should(Succeed())

		By("verifying that the controller has updated the status")
		finalVA1 := &v1alpha1.VariantAutoscaling{}
		err := crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      firstDeployName,
		}, finalVA1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", firstDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA1.Name,
			finalVA1.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA1.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA1.Name,
			finalVA1.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA1.Status.DesiredOptimizedAlloc.Accelerator)

		deployment1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, firstDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			firstDeployName,
			deployment1.Status.Replicas)

		finalVA2 := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      secondDeployName,
		}, finalVA2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to fetch VariantAutoscaling for: %s", secondDeployName))

		// Log the status for debugging
		fmt.Printf("Load Profile for VA: %s - Arrival Rate: %s, Avg Length: %s\n",
			finalVA2.Name,
			finalVA2.Status.CurrentAlloc.Load.ArrivalRate,
			finalVA2.Status.CurrentAlloc.Load.AvgLength)
		fmt.Printf("Desired Optimized Allocation for VA: %s - Replicas: %d, Accelerator: %s\n",
			finalVA2.Name,
			finalVA2.Status.DesiredOptimizedAlloc.NumReplicas,
			finalVA2.Status.DesiredOptimizedAlloc.Accelerator)

		deployment2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, secondDeployName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		fmt.Printf("Current replicas for Deployment - %s: %d\n",
			secondDeployName,
			deployment2.Status.Replicas)
	})

	AfterAll(func() {
		By("deleting resources for first deployment")
		variantAutoscaling := &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      firstDeployName,
				Namespace: namespace,
			},
		}
		err := crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", firstDeployName))

		serviceMonitor := &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(firstServiceMonitorName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", firstServiceMonitorName))

		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, firstServiceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", firstServiceName))

		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, firstDeployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", firstDeployName))

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + firstAppLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", firstAppLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())

		By("deleting resources for second deployment")
		variantAutoscaling = &v1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secondDeployName,
				Namespace: namespace,
			},
		}
		err = crClient.Delete(ctx, variantAutoscaling)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete VariantAutoscaling for: %s", secondDeployName))

		serviceMonitor = &unstructured.Unstructured{}
		serviceMonitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "monitoring.coreos.com",
			Version: "v1",
			Kind:    "ServiceMonitor",
		})
		serviceMonitor.SetName(secondServiceMonitorName)
		serviceMonitor.SetNamespace(controllerMonitoringNamespace)
		err = crClient.Delete(ctx, serviceMonitor)
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete ServiceMonitor: %s", secondServiceMonitorName))

		By("deleting vllme service")
		err = k8sClient.CoreV1().Services(namespace).Delete(ctx, secondServiceName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Service: %s", secondServiceName))

		By("deleting vllme deployment")
		err = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, secondDeployName, metav1.DeleteOptions{})
		err = client.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to delete Deployment: %s", secondDeployName))

		By("waiting for all pods to be deleted")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + secondAppLabel})
			if err != nil {
				g.Expect(err).NotTo(HaveOccurred(), "Should be able to list Pods")
			}
			g.Expect(podList.Items).To(BeEmpty(), fmt.Sprintf("All Pods labelled: %s should be deleted", secondAppLabel))
		}, 1*time.Minute, 1*time.Second).Should(Succeed())

		By("cleaning up Prometheus operator resources")
		cmd := exec.Command("kubectl", "delete", "-f", "hack/vllme/deploy/prometheus-operator/prometheus-deploy-all-in-one.yaml", "--ignore-not-found=true")
		output, err := utils.Run(cmd)
		if err != nil {
			fmt.Printf("Prometheus cleanup output: %s\n", output)
		}
	})
})
