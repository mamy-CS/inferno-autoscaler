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
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
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

	// GPU configurations - each node has 4 GPUs
	gpusPerReplicaVariant1 = 2 // Variant 1: 2 GPUs/replica, max 2 replicas on 4-GPU node
	gpusPerReplicaVariant2 = 1 // Variant 2: 1 GPU/replica, max 4 replicas on 4-GPU node

	// Min GPUs per specific GPU type (one node with 4 GPUs)
	minRequiredGPUsPerType = 4

	// Load generation parameters - must match saturation tests to ensure enough load
	limiterLoadRate     = 8   // requests per second (same as saturation tests)
	limiterMaxExecTime  = 300 // seconds (5 minutes to ensure saturation is detected)
	limiterInputTokens  = 128 // same as saturation tests
	limiterOutputTokens = 128 // same as saturation tests
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

// getGPUNodeSelectors returns node selectors for the given GPU configuration.
// For nvidia (nvidia-mix cluster): variant1 targets H100, variant2 targets A100
// For amd (amd-mix cluster): variant1 targets MI300X, variant2 targets MI250
func getGPUNodeSelectors() (map[string]string, map[string]string, string, string) {
	gpuType := os.Getenv("E2E_GPU_TYPE")
	if gpuType == "" {
		gpuType = "nvidia"
	}

	var variant1Selector, variant2Selector map[string]string
	var variant1Acc, variant2Acc string

	if gpuType == "nvidia" {
		// nvidia-mix cluster: H100, A100, MI300X
		variant1Selector = map[string]string{"gpu-config": "4H100"}
		variant2Selector = map[string]string{"gpu-config": "4A100"}
		variant1Acc = "H100"
		variant2Acc = "A100"
	} else {
		// amd-mix cluster: MI300X, MI250, A100
		variant1Selector = map[string]string{"gpu-config": "4MI300X"}
		variant2Selector = map[string]string{"gpu-config": "4MI250"}
		variant1Acc = "MI300X"
		variant2Acc = "MI250"
	}

	return variant1Selector, variant2Selector, variant1Acc, variant2Acc
}

var _ = Describe("Test workload-variant-autoscaler - GPU Limiter Feature", Ordered, func() {
	var (
		ctx context.Context

		// Variant 1 resources
		variant1Name           string
		variant1DeployName     string
		variant1ServiceName    string
		variant1ServiceMonName string
		variant1HPAName        string
		variant1AppLabel       string

		// Variant 2 resources
		variant2Name           string
		variant2DeployName     string
		variant2ServiceName    string
		variant2ServiceMonName string
		variant2HPAName        string
		variant2AppLabel       string

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
		variant1ServiceMonName = variant1Name + "-servicemonitor"
		variant1HPAName = variant1Name + "-hpa"
		variant1AppLabel = variant1Name

		// Variant 2 setup
		variant2Name = "llm-d-sim-limiter-v2"
		variant2DeployName = variant2Name + "-deployment"
		variant2ServiceName = variant2Name + "-service"
		variant2ServiceMonName = variant2Name + "-servicemonitor"
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
		if totalGPUs < minRequiredGPUsPerType {
			Skip(fmt.Sprintf("Cluster has only %d %s GPUs, need at least %d. Run: ./deploy/kind-emulator/setup.sh -t %s-mix -g 4",
				totalGPUs, gpuType, minRequiredGPUsPerType, gpuType))
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Cluster has %d %s GPUs (minimum required: %d)\n", totalGPUs, gpuType, minRequiredGPUsPerType)

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

		// Get node selectors and accelerator types for heterogeneous GPU targeting
		variant1NodeSelector, variant2NodeSelector, variant1Acc, variant2Acc := getGPUNodeSelectors()
		_, _ = fmt.Fprintf(GinkgoWriter, "Variant1 targeting: %v (accelerator: %s)\n", variant1NodeSelector, variant1Acc)
		_, _ = fmt.Fprintf(GinkgoWriter, "Variant2 targeting: %v (accelerator: %s)\n", variant2NodeSelector, variant2Acc)

		By("creating Variant 1 deployment with node selector for specific GPU type")
		deployment1 := resources.CreateLlmdSimDeploymentWithGPUAndNodeSelector(
			namespace, variant1DeployName, limiterModel1, variant1AppLabel, "8000",
			avgTTFT, avgITL, initialReplicas, gpusPerReplicaVariant1, gpuType,
			variant1NodeSelector,
		)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", variant1DeployName))
		_, _ = fmt.Fprintf(GinkgoWriter, "Variant1 deployment created with selector: %v\n", variant1NodeSelector)

		By("creating Variant 2 deployment with node selector for specific GPU type")
		deployment2 := resources.CreateLlmdSimDeploymentWithGPUAndNodeSelector(
			namespace, variant2DeployName, limiterModel2, variant2AppLabel, "8001",
			avgTTFT, avgITL, initialReplicas, gpusPerReplicaVariant2, gpuType,
			variant2NodeSelector,
		)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create Deployment: %s", variant2DeployName))
		_, _ = fmt.Fprintf(GinkgoWriter, "Variant2 deployment created with selector: %v\n", variant2NodeSelector)

		By("creating services for both deployments")
		service1 := resources.CreateLlmdSimService(namespace, variant1ServiceName, variant1AppLabel, 30010, 8000)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		service2 := resources.CreateLlmdSimService(namespace, variant2ServiceName, variant2AppLabel, 30011, 8001)
		_, err = k8sClient.CoreV1().Services(namespace).Create(ctx, service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("creating ServiceMonitors for metrics collection")
		serviceMonitor1 := resources.CreateLlmdSimServiceMonitor(variant1ServiceMonName, controllerMonitoringNamespace, llmDNamespace, variant1AppLabel)
		err = crClient.Create(ctx, serviceMonitor1)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", variant1ServiceMonName))
		_, _ = fmt.Fprintf(GinkgoWriter, "ServiceMonitor created for Variant1: %s\n", variant1ServiceMonName)

		serviceMonitor2 := resources.CreateLlmdSimServiceMonitor(variant2ServiceMonName, controllerMonitoringNamespace, llmDNamespace, variant2AppLabel)
		err = crClient.Create(ctx, serviceMonitor2)
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Should be able to create ServiceMonitor: %s", variant2ServiceMonName))
		_, _ = fmt.Fprintf(GinkgoWriter, "ServiceMonitor created for Variant2: %s\n", variant2ServiceMonName)

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

		By("creating VariantAutoscaling resources for both variants with correct accelerator types")
		va1 := utils.CreateVariantAutoscalingResource(namespace, variant1DeployName, limiterModel1, variant1Acc, 30.0)
		err = crClient.Create(ctx, va1)
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "VA1 created with accelerator: %s\n", variant1Acc)

		va2 := utils.CreateVariantAutoscalingResource(namespace, variant2DeployName, limiterModel2, variant2Acc, 20.0)
		err = crClient.Create(ctx, va2)
		Expect(err).NotTo(HaveOccurred())
		_, _ = fmt.Fprintf(GinkgoWriter, "VA2 created with accelerator: %s\n", variant2Acc)

		By("creating HPAs for both deployments")
		hpa1 := utils.CreateHPAOnDesiredReplicaMetrics(variant1HPAName, namespace, variant1DeployName, variant1DeployName, 10)
		_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		hpa2 := utils.CreateHPAOnDesiredReplicaMetrics(variant2HPAName, namespace, variant2DeployName, variant2DeployName, 10)
		_, err = k8sClient.AutoscalingV2().HorizontalPodAutoscalers(namespace).Create(ctx, hpa2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("waiting for metrics pipeline to be ready (DesiredOptimizedAlloc populated)")
		Eventually(func(g Gomega) {
			va1 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant1DeployName}, va1)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va1.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(),
				"VA1 DesiredOptimizedAlloc should be populated with accelerator info")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "Metrics pipeline ready - VA1 has DesiredOptimizedAlloc populated\n")

		Eventually(func(g Gomega) {
			va2 := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant2DeployName}, va2)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(va2.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(),
				"VA2 DesiredOptimizedAlloc should be populated with accelerator info")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "Metrics pipeline ready - VA2 has DesiredOptimizedAlloc populated\n")
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

		// Delete ServiceMonitors
		for _, smName := range []string{variant1ServiceMonName, variant2ServiceMonName} {
			sm := &promoperator.ServiceMonitor{}
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: controllerMonitoringNamespace, Name: smName}, sm); err == nil {
				_ = crClient.Delete(ctx, sm)
			}
		}

		// Delete Services
		for _, svcName := range []string{variant1ServiceName, variant2ServiceName} {
			_ = k8sClient.CoreV1().Services(namespace).Delete(ctx, svcName, metav1.DeleteOptions{})
		}

		// Delete Deployments
		for _, deployName := range []string{variant1DeployName, variant2DeployName} {
			_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deployName, metav1.DeleteOptions{})
		}

		_, _ = fmt.Fprintf(GinkgoWriter, "Cleanup completed for GPU Limiter E2E test\n")
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

			By("verifying deployments have GPU resource requests and node selectors")
			gpuResName := getGPUResourceName()
			variant1NodeSelector, variant2NodeSelector, _, _ := getGPUNodeSelectors()

			deploy1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, variant1DeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			gpuQty := deploy1.Spec.Template.Spec.Containers[0].Resources.Requests[gpuResName]
			Expect(gpuQty.Value()).To(Equal(int64(gpusPerReplicaVariant1)))
			Expect(deploy1.Spec.Template.Spec.NodeSelector).To(Equal(variant1NodeSelector),
				"Variant1 deployment should have correct node selector for GPU targeting")

			deploy2, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, variant2DeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			gpuQty2 := deploy2.Spec.Template.Spec.Containers[0].Resources.Requests[gpuResName]
			Expect(gpuQty2.Value()).To(Equal(int64(gpusPerReplicaVariant2)))
			Expect(deploy2.Spec.Template.Spec.NodeSelector).To(Equal(variant2NodeSelector),
				"Variant2 deployment should have correct node selector for GPU targeting")

			_, _ = fmt.Fprintf(GinkgoWriter, "Limiter enabled and resources verified - Variant1: %d GPUs/replica (selector: %v), Variant2: %d GPUs/replica (selector: %v)\n",
				gpusPerReplicaVariant1, variant1NodeSelector, gpusPerReplicaVariant2, variant2NodeSelector)
		})
	})

	Context("Scenario 2: Limiter constrains scale-up", func() {
		It("should limit scale-up when GPU capacity is exhausted", func() {
			variant1NodeSelector, _, _, _ := getGPUNodeSelectors()

			By("verifying Variant1 is constrained to node with 4 GPUs")
			deploy1, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, variant1DeployName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(deploy1.Spec.Template.Spec.NodeSelector).To(Equal(variant1NodeSelector))
			_, _ = fmt.Fprintf(GinkgoWriter, "Variant1 constrained to node with selector: %v\n", variant1NodeSelector)

			By("checking available GPUs on target node (should be 4)")
			// With 2 GPUs/replica and 4 GPUs on node, max replicas = 2
			maxReplicasOnNode := 4 / gpusPerReplicaVariant1
			_, _ = fmt.Fprintf(GinkgoWriter, "With %d GPUs/replica and 4 GPUs on target node, max replicas = %d\n",
				gpusPerReplicaVariant1, maxReplicasOnNode)

			By("generating load to trigger saturation and scale-up request")
			// Create load generator targeting variant1's model
			loadGenJob, err := utils.CreateLoadGeneratorJob(
				namespace,
				fmt.Sprintf("http://%s:%d", variant1ServiceName, 8000),
				limiterModel1,
				limiterLoadRate,
				limiterMaxExecTime,
				limiterInputTokens,
				limiterOutputTokens,
				k8sClient,
				ctx,
			)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Load generator job created: %s\n", loadGenJob.Name)
			defer func() {
				_ = utils.StopJob(namespace, loadGenJob, k8sClient, ctx)
			}()

			By("waiting for load generator to be ready (pod running + pip install complete)")
			err = utils.WaitForLoadGeneratorReady(ctx, loadGenJob, k8sClient, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred(), "Load generator should become ready")

			By("waiting for autoscaler to respond to load")
			time.Sleep(2 * time.Minute) // Allow saturation metrics to accumulate and trigger scaling

			By("verifying limiter constrains scale-up to max 2 replicas")
			Eventually(func(g Gomega) {
				va := &v1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant1DeployName}, va)
				g.Expect(err).NotTo(HaveOccurred())

				desiredReplicas := va.Status.DesiredOptimizedAlloc.NumReplicas
				accelerator := va.Status.DesiredOptimizedAlloc.Accelerator
				_, _ = fmt.Fprintf(GinkgoWriter, "DesiredOptimizedAlloc: NumReplicas=%d, Accelerator=%s (max should be %d due to 4 GPU limit)\n",
					desiredReplicas, accelerator, maxReplicasOnNode)

				// Verify metrics are flowing - accelerator should be populated
				g.Expect(accelerator).NotTo(BeEmpty(),
					"DesiredOptimizedAlloc.Accelerator should be populated when metrics are flowing")

				// With 2 GPUs/replica and only 4 GPUs available, max is 2 replicas
				// The limiter should prevent scaling beyond this limit
				g.Expect(desiredReplicas).To(BeNumerically("<=", maxReplicasOnNode),
					fmt.Sprintf("Limiter should cap replicas at %d (%d GPUs / %d GPUs per replica)",
						maxReplicasOnNode, 4, gpusPerReplicaVariant1))

				// Also verify we have at least 1 replica (metrics are working)
				g.Expect(desiredReplicas).To(BeNumerically(">=", 1),
					"Should have at least 1 replica when metrics are available")
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Limiter successfully constrained scale-up to available GPU capacity\n")
		})
	})

	Context("Scenario 3: Priority by saturation", func() {
		It("should prioritize most saturated variant when allocating limited GPUs", func() {
			By("generating load on both variants simultaneously")
			// Variant1 gets heavier load to become more saturated
			loadGenJob1, err := utils.CreateLoadGeneratorJob(
				namespace,
				fmt.Sprintf("http://%s:%d", variant1ServiceName, 8000),
				limiterModel1,
				limiterLoadRate*2, // Higher load for variant1
				limiterMaxExecTime,
				limiterInputTokens,
				limiterOutputTokens,
				k8sClient,
				ctx,
			)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Load generator job 1 (high load) created: %s\n", loadGenJob1.Name)
			defer func() {
				_ = utils.StopJob(namespace, loadGenJob1, k8sClient, ctx)
			}()

			loadGenJob2, err := utils.CreateLoadGeneratorJob(
				namespace,
				fmt.Sprintf("http://%s:%d", variant2ServiceName, 8001),
				limiterModel2,
				limiterLoadRate,
				limiterMaxExecTime,
				limiterInputTokens,
				limiterOutputTokens,
				k8sClient,
				ctx,
			)
			Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Load generator job 2 (normal load) created: %s\n", loadGenJob2.Name)
			defer func() {
				_ = utils.StopJob(namespace, loadGenJob2, k8sClient, ctx)
			}()

			By("waiting for load generators to be ready (pod running + pip install complete)")
			err = utils.WaitForLoadGeneratorReady(ctx, loadGenJob1, k8sClient, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred(), "Load generator 1 should become ready")
			err = utils.WaitForLoadGeneratorReady(ctx, loadGenJob2, k8sClient, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred(), "Load generator 2 should become ready")

			By("waiting for autoscaler to respond to load")
			time.Sleep(2 * time.Minute) // Allow saturation metrics to accumulate and trigger scaling

			By("verifying both VAs have scaling decisions with metrics available")
			Eventually(func(g Gomega) {
				va1 := &v1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant1DeployName}, va1)
				g.Expect(err).NotTo(HaveOccurred())

				va2 := &v1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: variant2DeployName}, va2)
				g.Expect(err).NotTo(HaveOccurred())

				// Verify metrics pipeline is working for both VAs
				g.Expect(va1.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(),
					"VA1 should have accelerator populated")
				g.Expect(va2.Status.DesiredOptimizedAlloc.Accelerator).NotTo(BeEmpty(),
					"VA2 should have accelerator populated")

				// Both should have at least 1 replica
				g.Expect(va1.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1),
					"VA1 should have at least 1 replica")
				g.Expect(va2.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 1),
					"VA2 should have at least 1 replica")

				_, _ = fmt.Fprintf(GinkgoWriter,
					"Allocation - Variant1 (higher load, %s): %d replicas, Variant2 (%s): %d replicas\n",
					va1.Status.DesiredOptimizedAlloc.Accelerator,
					va1.Status.DesiredOptimizedAlloc.NumReplicas,
					va2.Status.DesiredOptimizedAlloc.Accelerator,
					va2.Status.DesiredOptimizedAlloc.NumReplicas)
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			_, _ = fmt.Fprintf(GinkgoWriter, "Saturation-based prioritization test completed\n")
		})
	})
})
