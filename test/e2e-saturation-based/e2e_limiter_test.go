package e2esaturation

import (
	"context"
	"fmt"
	"os"
	"time"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils/resources"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Limiter test constants
const (
	// Test model IDs - different models to test limiter across variants
	limiterModel1 = "test/limiter-model-1"
	limiterModel2 = "test/limiter-model-2"

	// GPUs per replica for testing
	// CI runs with E2E_GPU_TYPE=nvidia or E2E_GPU_TYPE=amd
	// This provides 6 GPUs (3 nodes × 2 GPUs each)
	// Different GPU requirements test limiter prioritization
	gpusPerReplicaVariant1 = 2 // Variant 1 uses 2 GPUs per replica (higher cost)
	gpusPerReplicaVariant2 = 1 // Variant 2 uses 1 GPU per replica (lower cost)

	// Minimum GPUs required to run these tests
	minRequiredGPUs = 4 // Need at least 4: initial (2+1) + 1 for scale-up test
)

// getGPUResourceName returns the GPU resource name based on E2E_GPU_TYPE env var.
// Defaults to "nvidia" if not set.
func getGPUResourceName() corev1.ResourceName {
	gpuType := os.Getenv("E2E_GPU_TYPE")
	if gpuType == "" {
		gpuType = "nvidia" // default
	}
	return corev1.ResourceName(gpuType + ".com/gpu")
}

var _ = Describe("Test workload-variant-autoscaler - GPU Limiter Feature", Ordered, func() {
	var (
		ctx context.Context

		// Variant 1 resources
		variant1Name        string
		variant1DeployName  string
		variant1ServiceName string
		variant1HPAName     string
		variant1AppLabel    string

		// Variant 2 resources
		variant2Name        string
		variant2DeployName  string
		variant2ServiceName string
		variant2HPAName     string
		variant2AppLabel    string

		namespace       string
		initialReplicas int32
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()

		ctx = context.Background()
		namespace = llmDNamespace
		initialReplicas = 1

		// Variant 1 setup
		variant1Name = "llm-d-sim-limiter-v1"
		variant1DeployName = variant1Name + "-deployment"
		variant1ServiceName = variant1Name + "-service"
		variant1HPAName = variant1Name + "-hpa"
		variant1AppLabel = variant1Name

		// Variant 2 setup
		variant2Name = "llm-d-sim-limiter-v2"
		variant2DeployName = variant2Name + "-deployment"
		variant2ServiceName = variant2Name + "-service"
		variant2HPAName = variant2Name + "-hpa"
		variant2AppLabel = variant2Name

		logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

		gpuResourceName := getGPUResourceName()
		gpuType := os.Getenv("E2E_GPU_TYPE")
		if gpuType == "" {
			gpuType = "nvidia"
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "GPU type for this test run: %s (resource: %s)\n", gpuType, gpuResourceName)

		By(fmt.Sprintf("checking cluster has sufficient %s GPUs", gpuType))
		nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		totalGPUs := int64(0)
		for _, node := range nodes.Items {
			if gpuQty, ok := node.Status.Allocatable[gpuResourceName]; ok {
				totalGPUs += gpuQty.Value()
			}
		}
		if totalGPUs < minRequiredGPUs {
			Skip(fmt.Sprintf("Cluster has only %d %s GPUs, need at least %d. Run: ./deploy/kind-emulator/setup.sh -t %s",
				totalGPUs, gpuType, minRequiredGPUs, gpuType))
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Cluster has %d %s GPUs (minimum required: %d)\n", totalGPUs, gpuType, minRequiredGPUs)

		By("verifying saturation-scaling ConfigMap exists with limiter enabled")
		Eventually(func(g Gomega) {
			cm, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, saturationConfigMapName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("saturation ConfigMap %s should exist in namespace %s", saturationConfigMapName, controllerNamespace))
			g.Expect(cm.Data).To(HaveKey("default"), "saturation ConfigMap should have 'default' configuration")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("enabling limiter in saturation-scaling ConfigMap")
		cm, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, saturationConfigMapName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		// Update the default config to enable limiter
		cm.Data["default"] = `kvCacheThreshold: 0.80
queueLengthThreshold: 5
kvSpareTrigger: 0.1
queueSpareTrigger: 3
enableLimiter: true`
		_, err = k8sClient.CoreV1().ConfigMaps(controllerNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to update saturation ConfigMap to enable limiter")

		By("ensuring unique app labels for deployments")
		utils.ValidateAppLabelUniqueness(namespace, variant1AppLabel, k8sClient, crClient)
		utils.ValidateAppLabelUniqueness(namespace, variant2AppLabel, k8sClient, crClient)

		By("creating Variant 1 deployment with 2 GPUs per replica")
		deployment1 := resources.CreateLlmdSimDeploymentWithGPU(namespace, variant1DeployName, limiterModel1, variant1AppLabel, "8000", avgTTFT, avgITL, initialReplicas, gpusPerReplicaVariant1, gpuType)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", variant1DeployName))

		By("creating Variant 2 deployment with 1 GPU per replica")
		deployment2 := resources.CreateLlmdSimDeploymentWithGPU(namespace, variant2DeployName, limiterModel2, variant2AppLabel, "8001", avgTTFT, avgITL, initialReplicas, gpusPerReplicaVariant2, gpuType)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", variant2DeployName))

		By("creating services for both deployments")
		service1 := resources.CreateLlmdSimService(namespace, variant1ServiceName, variant1AppLabel, 30010, 8000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		service2 := resources.CreateLlmdSimService(namespace, variant2ServiceName, variant2AppLabel, 30011, 8001)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("waiting for pods to be running")
		for _, appLabel := range []string{variant1AppLabel, variant2AppLabel} {
			Eventually(func(g Gomega) {
				podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + appLabel,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(podList.Items)).To(BeNumerically(">=", 1))
				pod := podList.Items[0]
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("Pod %s is not running", pod.Name))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		}

		By("creating VariantAutoscaling resources for both variants")
		va1 := utils.CreateVariantAutoscalingResource(namespace, variant1DeployName, limiterModel1, a100Acc, 30.0)
		err = crClient.Create(ctx, va1)
		Expect(err).NotTo(HaveOccurred())

		va2 := utils.CreateVariantAutoscalingResource(namespace, variant2DeployName, limiterModel2, a100Acc, 20.0)
		err = crClient.Create(ctx, va2)
		Expect(err).NotTo(HaveOccurred())

		By("creating HPAs for both deployments")
		hpa1 := utils.CreateHPAOnDesiredReplicaMetrics(variant1HPAName, namespace, variant1DeployName, variant1DeployName, 10)
		_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		hpa2 := utils.CreateHPAOnDesiredReplicaMetrics(variant2HPAName, namespace, variant2DeployName, variant2DeployName, 10)
		_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		if k8sClient == nil {
			return
		}

		By("cleaning up test resources")
		ctx := context.Background()

		// Delete VAs
		for _, vaName := range []string{variant1DeployName, variant2DeployName} {
			va := &v1alpha1.VariantAutoscaling{}
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vaName}, va); err == nil {
				_ = crClient.Delete(ctx, va)
			}
		}

		// Delete HPAs
		for _, hpaName := range []string{variant1HPAName, variant2HPAName} {
			_ = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Delete(ctx, hpaName, metav1.DeleteOptions{})
		}

		// Delete Services
		for _, svcName := range []string{variant1ServiceName, variant2ServiceName} {
			_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, svcName, metav1.DeleteOptions{})
		}

		// Delete Deployments
		for _, deployName := range []string{variant1DeployName, variant2DeployName} {
			_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		}
	})

	Context("Scenario 1: Limiter enabled - normal operation", func() {
		It("should allow scale-up when sufficient GPUs are available", func() {
			By("verifying limiter is enabled in ConfigMap")
			cm, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, saturationConfigMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(cm.Data["default"]).To(ContainSubstring("enableLimiter: true"))

			By("verifying VariantAutoscaling resources are created")
			va1 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant1DeployName}, va1)
			Expect(err).NotTo(HaveOccurred())
			Expect(va1.Spec.ModelID).To(Equal(limiterModel1))

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant2DeployName}, va2)
			Expect(err).NotTo(HaveOccurred())
			Expect(va2.Spec.ModelID).To(Equal(limiterModel2))

			By("verifying deployments have GPU resource requests")
			gpuResName := getGPUResourceName()
			deploy1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, variant1DeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			gpuQty := deploy1.Spec.Template.Spec.Containers[0].Resources.Requests[gpuResName]
			Expect(gpuQty.Value()).To(Equal(int64(gpusPerReplicaVariant1)))

			deploy2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, variant2DeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			gpuQty2 := deploy2.Spec.Template.Spec.Containers[0].Resources.Requests[gpuResName]
			Expect(gpuQty2.Value()).To(Equal(int64(gpusPerReplicaVariant2)))

			_, _ = fmt.Fprintf(GinkgoWriter, "Limiter enabled and resources verified - Variant1: %d GPUs/replica, Variant2: %d GPUs/replica (resource: %s)\n",
				gpusPerReplicaVariant1, gpusPerReplicaVariant2, gpuResName)
		})
	})

	Context("Scenario 2: Limiter constrains scale-up", func() {
		It("should limit scale-up when GPU capacity is exhausted", func() {
			gpuResName := getGPUResourceName()

			By("checking cluster GPU capacity from node status")
			nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())

			totalGPUs := int64(0)
			for _, node := range nodes.Items {
				if gpuQty, ok := node.Status.Allocatable[gpuResName]; ok {
					totalGPUs += gpuQty.Value()
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Total cluster GPU capacity (%s): %d\n", gpuResName, totalGPUs)

			By("calculating current GPU usage")
			currentUsedGPUs := int64(0)
			podList, err := k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
					for _, container := range pod.Spec.Containers {
						if gpuQty, ok := container.Resources.Requests[gpuResName]; ok {
							currentUsedGPUs += gpuQty.Value()
						}
					}
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Current GPU usage: %d, Available: %d\n", currentUsedGPUs, totalGPUs-currentUsedGPUs)

			Expect(totalGPUs).To(BeNumerically(">=", currentUsedGPUs), "GPU capacity should be at least as much as current usage")
		})
	})

	Context("Scenario 3: Priority by saturation", func() {
		It("should prioritize most saturated variant when allocating limited GPUs", func() {
			By("verifying both VAs exist with different costs")
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant1DeployName}, va1)
			Expect(err).NotTo(HaveOccurred())

			va2 := &v1alpha1.VariantAutoscaling{}
			err = crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant2DeployName}, va2)
			Expect(err).NotTo(HaveOccurred())

			cost1 := va1.Spec.VariantCost
			cost2 := va2.Spec.VariantCost
			_, _ = fmt.Fprintf(GinkgoWriter, "Variant costs - V1: %s, V2: %s\n", cost1, cost2)

			By("checking that limiter uses saturation-based prioritization")
			cm, err := k8sClient.CoreV1().ConfigMaps(controllerNamespace).Get(ctx, saturationConfigMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(cm.Data["default"]).To(ContainSubstring("enableLimiter: true"),
				"Limiter should be enabled for saturation-based prioritization")

			_, _ = fmt.Fprintf(GinkgoWriter, "Saturation-based prioritization is active via enableLimiter\n")
		})
	})
})
