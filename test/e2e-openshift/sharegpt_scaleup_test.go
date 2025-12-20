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

package e2eopenshift

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
)

var lowLoad = numPrompts <= 2000 && requestRate <= 8

// Load generation configuration constants
const (
	numLoadWorkers     = 10    // Number of parallel load generation workers
	requestsPerWorker  = 500   // Requests each worker sends
	batchSize          = 50    // Concurrent requests per batch
	curlTimeoutSeconds = 120   // Timeout for each curl request
	maxTokens          = 150   // Max tokens for completion requests
	batchSleepDuration = "0.1" // Sleep duration between batches to control rate
)

var _ = Describe("ShareGPT Scale-Up Test", Ordered, func() {
	var (
		ctx                  context.Context
		jobBaseName          string
		initialReplicas      int32
		initialOptimized     int32
		hpaMinReplicas       int32
		hpaName              string
		vaName               string
		vllmServiceName      string
		scaledReplicas       int32
		scaledOptimized      int32
		jobCompletionTimeout = 10 * time.Minute
	)

	BeforeAll(func() {
		ctx = context.Background()
		jobBaseName = "load-gen-e2e"

		By("recording initial state of the deployment")
		deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Should be able to get vLLM deployment")
		initialReplicas = deploy.Status.ReadyReplicas
		_, _ = fmt.Fprintf(GinkgoWriter, "Initial ready replicas: %d\n", initialReplicas)

		By("recording initial VariantAutoscaling state")
		// Find VariantAutoscaling by label selector (name includes release name)
		vaList := &v1alpha1.VariantAutoscalingList{}
		err = crClient.List(ctx, vaList, client.InNamespace(llmDNamespace), client.MatchingLabels{
			"app.kubernetes.io/name": "workload-variant-autoscaler",
		})
		Expect(err).NotTo(HaveOccurred(), "Should be able to list VariantAutoscalings")
		Expect(vaList.Items).NotTo(BeEmpty(), "At least one WVA VariantAutoscaling should exist")
		va := &vaList.Items[0]
		vaName = va.Name
		_, _ = fmt.Fprintf(GinkgoWriter, "Found VariantAutoscaling: %s\n", vaName)

		initialOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
		_, _ = fmt.Fprintf(GinkgoWriter, "Initial optimized replicas: %d\n", initialOptimized)

		By("verifying HPA exists and is configured correctly")
		// Find HPA by label selector (name includes release name)
		hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
		})
		Expect(err).NotTo(HaveOccurred(), "Should be able to list HPAs")
		Expect(hpaList.Items).NotTo(BeEmpty(), "At least one WVA HPA should exist")

		// Select the HPA that targets the expected deployment
		// This validation ensures we pick the correct HPA if multiple WVA releases exist
		var hpa *autoscalingv2.HorizontalPodAutoscaler
		for i := range hpaList.Items {
			if hpaList.Items[i].Spec.ScaleTargetRef.Name == deployment {
				hpa = &hpaList.Items[i]
				break
			}
		}
		Expect(hpa).NotTo(BeNil(), "An HPA targeting deployment %s should exist", deployment)
		hpaName = hpa.Name
		_, _ = fmt.Fprintf(GinkgoWriter, "Found HPA: %s (targets %s)\n", hpaName, deployment)

		By("finding vllm-service by label selector")
		// Use release-specific label selector if WVA_RELEASE_NAME is set
		// This prevents picking up services from previous/parallel test runs
		labelSelector := "app.kubernetes.io/name=workload-variant-autoscaler"
		if wvaReleaseName != "" {
			labelSelector = fmt.Sprintf("%s,app.kubernetes.io/instance=%s", labelSelector, wvaReleaseName)
			_, _ = fmt.Fprintf(GinkgoWriter, "Using release-specific label selector: %s\n", labelSelector)
		}
		svcList, err := k8sClient.CoreV1().Services(llmDNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		Expect(err).NotTo(HaveOccurred(), "Should be able to list services")
		Expect(svcList.Items).NotTo(BeEmpty(), "At least one WVA vllm-service should exist for release %s", wvaReleaseName)
		vllmServiceName = svcList.Items[0].Name
		_, _ = fmt.Fprintf(GinkgoWriter, "Found vllm-service: %s\n", vllmServiceName)
		Expect(hpa.Spec.Metrics).To(HaveLen(1), "HPA should have one metric")
		Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use external metrics")
		Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.InfernoDesiredReplicas), "HPA should use inferno_desired_replicas metric")

		// Store HPA minReplicas for assertions - we compare against this, not current state
		hpaMinReplicas = *hpa.Spec.MinReplicas
		_, _ = fmt.Fprintf(GinkgoWriter, "HPA minReplicas: %d\n", hpaMinReplicas)
	})

	It("should verify external metrics API is accessible", func() {
		By("querying external metrics API for inferno_desired_replicas")
		Eventually(func(g Gomega) {
			// Use raw API client to query external metrics
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + llmDNamespace + "/" + constants.InfernoDesiredReplicas).
				DoRaw(ctx)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to query external metrics API")
			g.Expect(string(result)).To(ContainSubstring(constants.InfernoDesiredReplicas), "Metric should be available")
			g.Expect(string(result)).To(ContainSubstring(deployment), "Metric should be for the correct variant")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should create and run parallel load generation jobs", func() {
		By("cleaning up any existing jobs")
		deleteParallelLoadJobs(ctx, jobBaseName, llmDNamespace, numLoadWorkers)
		// Wait a bit for cleanup
		time.Sleep(2 * time.Second)

		By("waiting for vllm-service endpoints to exist")
		Eventually(func(g Gomega) {
			// Check that the vllm-service exists and has endpoints
			endpoints, err := k8sClient.CoreV1().Endpoints(llmDNamespace).Get(ctx, vllmServiceName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "vllm-service endpoints should exist")
			g.Expect(endpoints.Subsets).NotTo(BeEmpty(), "vllm-service should have endpoints")

			// Count ready addresses
			readyCount := 0
			for _, subset := range endpoints.Subsets {
				readyCount += len(subset.Addresses)
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "%s has %d ready endpoints\n", vllmServiceName, readyCount)
			g.Expect(readyCount).To(BeNumerically(">", 0), "vllm-service should have at least one ready endpoint")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("waiting for vLLM to be ready to accept requests")
		// Create a port-forward or use a test pod to check vLLM health
		// We'll create a simple health check job that exits successfully when vLLM responds
		healthCheckBackoffLimit := int32(15)
		healthCheckJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vllm-health-check",
				Namespace: llmDNamespace,
			},
			Spec: batchv1.JobSpec{
				BackoffLimit: &healthCheckBackoffLimit,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:    "health-check",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args: []string{fmt.Sprintf(`
echo "Checking vLLM readiness at %s:8200..."
curl -sf --max-time 10 http://%s:8200/v1/models && echo "vLLM is ready!" && exit 0
echo "vLLM not ready yet"
exit 1`,
								vllmServiceName, vllmServiceName)},
						}},
					},
				},
			},
		}

		// Delete any existing health check job
		backgroundPropagation := metav1.DeletePropagationBackground
		_ = k8sClient.BatchV1().Jobs(llmDNamespace).Delete(ctx, "vllm-health-check", metav1.DeleteOptions{
			PropagationPolicy: &backgroundPropagation,
		})
		time.Sleep(2 * time.Second)

		// Create and wait for health check job
		_, createErr := k8sClient.BatchV1().Jobs(llmDNamespace).Create(ctx, healthCheckJob, metav1.CreateOptions{})
		Expect(createErr).NotTo(HaveOccurred(), "Should be able to create health check job")

		Eventually(func(g Gomega) {
			job, err := k8sClient.BatchV1().Jobs(llmDNamespace).Get(ctx, "vllm-health-check", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			_, _ = fmt.Fprintf(GinkgoWriter, "Health check job: succeeded=%d, failed=%d, active=%d\n",
				job.Status.Succeeded, job.Status.Failed, job.Status.Active)
			g.Expect(job.Status.Succeeded).To(BeNumerically(">=", 1), "Health check should succeed")
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		// Clean up health check job
		_ = k8sClient.BatchV1().Jobs(llmDNamespace).Delete(ctx, "vllm-health-check", metav1.DeleteOptions{
			PropagationPolicy: &backgroundPropagation,
		})

		_, _ = fmt.Fprintf(GinkgoWriter, "%s is ready and accepting requests, creating load generation jobs\n", vllmServiceName)

		// Clean up any existing load generation jobs from previous runs
		By("cleaning up any existing load generation jobs")
		_ = k8sClient.BatchV1().Jobs(llmDNamespace).DeleteCollection(ctx,
			metav1.DeleteOptions{
				PropagationPolicy: &backgroundPropagation,
			},
			metav1.ListOptions{
				LabelSelector: "experiment=load-gen-e2e",
			})
		time.Sleep(2 * time.Second)

		By(fmt.Sprintf("creating %d parallel load generation jobs", numLoadWorkers))
		loadErr := createParallelLoadJobs(ctx, jobBaseName, llmDNamespace, vllmServiceName, numLoadWorkers, requestsPerWorker)
		Expect(loadErr).NotTo(HaveOccurred(), "Should be able to create load generation jobs")

		By("waiting for job pods to be running")
		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(llmDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "experiment=load-gen-e2e",
			})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to list job pods")
			g.Expect(len(podList.Items)).To(BeNumerically(">=", numLoadWorkers), "All job pods should exist")

			runningCount := 0
			for _, pod := range podList.Items {
				if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodSucceeded {
					runningCount++
				}
			}
			g.Expect(runningCount).To(BeNumerically(">=", numLoadWorkers),
				fmt.Sprintf("At least %d job pods should be running, got %d", numLoadWorkers, runningCount))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "All %d load generation jobs are running\n", numLoadWorkers)
	})

	It("should detect increased load and recommend scale-up", func() {
		By("waiting for load generation to ramp up (30 seconds)")
		time.Sleep(30 * time.Second)

		By("monitoring VariantAutoscaling for scale-up recommendation")
		Eventually(func(g Gomega) {
			va := &v1alpha1.VariantAutoscaling{}
			err := crClient.Get(ctx, client.ObjectKey{
				Namespace: llmDNamespace,
				Name:      vaName,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			scaledOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
			currentRateStr := va.Status.CurrentAlloc.Load.ArrivalRate
			_, _ = fmt.Fprintf(GinkgoWriter, "Current optimized replicas: %d (initial: %d, minReplicas: %d), arrival rate: %s\n",
				scaledOptimized, initialOptimized, hpaMinReplicas, currentRateStr)

			// Expect scale-up recommendation (more than minReplicas)
			// We compare against minReplicas, not initial state, to ensure test passes
			// regardless of starting deployment state
			if !lowLoad {
				g.Expect(scaledOptimized).To(BeNumerically(">", hpaMinReplicas),
					fmt.Sprintf("WVA should recommend more replicas than minReplicas under load (current: %d, min: %d)", scaledOptimized, hpaMinReplicas))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping scale-up recommendation check\n")
			}

		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "WVA detected load and recommended %d replicas (up from %d)\n", scaledOptimized, initialOptimized)
	})

	It("should trigger HPA to scale up the deployment", func() {
		By("monitoring HPA for scale-up action")

		// Helper to dump diagnostic information
		dumpDiagnostics := func() {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n========== DIAGNOSTIC DUMP ==========\n")

			// Dump VariantAutoscaling status
			va := &v1alpha1.VariantAutoscaling{}
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: llmDNamespace, Name: vaName}, va); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "\n--- VariantAutoscaling Status ---\n")
				_, _ = fmt.Fprintf(GinkgoWriter, "DesiredOptimizedAlloc.NumReplicas: %d\n", va.Status.DesiredOptimizedAlloc.NumReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "CurrentAlloc.NumReplicas: %d\n", va.Status.CurrentAlloc.NumReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "CurrentAlloc.Load.ArrivalRate: %s\n", va.Status.CurrentAlloc.Load.ArrivalRate)
				_, _ = fmt.Fprintf(GinkgoWriter, "CurrentAlloc.Load.AvgInputTokens: %s\n", va.Status.CurrentAlloc.Load.AvgInputTokens)
				_, _ = fmt.Fprintf(GinkgoWriter, "CurrentAlloc.Load.AvgOutputTokens: %s\n", va.Status.CurrentAlloc.Load.AvgOutputTokens)
				for _, cond := range va.Status.Conditions {
					_, _ = fmt.Fprintf(GinkgoWriter, "Condition %s: %s (Reason: %s, Message: %s)\n",
						cond.Type, cond.Status, cond.Reason, cond.Message)
				}
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get VariantAutoscaling: %v\n", err)
			}

			// Dump external metrics API response
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- External Metrics API ---\n")
			result, err := k8sClient.RESTClient().
				Get().
				AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces/" + llmDNamespace + "/" + constants.InfernoDesiredReplicas).
				DoRaw(ctx)
			if err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to query external metrics API: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "External metrics response: %s\n", string(result))
			}

			// Dump HPA status and events
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, hpaName, metav1.GetOptions{})
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "\n--- HPA Status ---\n")
				_, _ = fmt.Fprintf(GinkgoWriter, "CurrentReplicas: %d\n", hpa.Status.CurrentReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "DesiredReplicas: %d\n", hpa.Status.DesiredReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "MinReplicas: %d\n", *hpa.Spec.MinReplicas)
				_, _ = fmt.Fprintf(GinkgoWriter, "MaxReplicas: %d\n", hpa.Spec.MaxReplicas)
				for _, metric := range hpa.Status.CurrentMetrics {
					if metric.External != nil {
						_, _ = fmt.Fprintf(GinkgoWriter, "Metric %s: CurrentValue=%v, AverageValue=%v\n",
							metric.External.Metric.Name, metric.External.Current.Value, metric.External.Current.AverageValue)
					}
				}
				for _, cond := range hpa.Status.Conditions {
					_, _ = fmt.Fprintf(GinkgoWriter, "HPA Condition %s: %s (Reason: %s, Message: %s)\n",
						cond.Type, cond.Status, cond.Reason, cond.Message)
				}
			}

			// Dump HPA events
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- HPA Events ---\n")
			events, err := k8sClient.CoreV1().Events(llmDNamespace).List(ctx, metav1.ListOptions{
				FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=HorizontalPodAutoscaler", hpaName),
			})
			if err == nil {
				for _, event := range events.Items {
					_, _ = fmt.Fprintf(GinkgoWriter, "[%s] %s: %s\n", event.Type, event.Reason, event.Message)
				}
			}

			// Dump deployment status
			deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "\n--- Deployment Status ---\n")
				_, _ = fmt.Fprintf(GinkgoWriter, "Replicas: %d, ReadyReplicas: %d, AvailableReplicas: %d\n",
					deploy.Status.Replicas, deploy.Status.ReadyReplicas, deploy.Status.AvailableReplicas)
			}

			// Dump WVA controller logs (last 50 lines)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- WVA Controller Logs (last 50 lines) ---\n")
			podList, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=workload-variant-autoscaler",
			})
			if err == nil && len(podList.Items) > 0 {
				tailLines := int64(50)
				logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(podList.Items[0].Name, &corev1.PodLogOptions{
					Container: "manager",
					TailLines: &tailLines,
				}).DoRaw(ctx)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", string(logs))
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get logs: %v\n", err)
				}
			}

			// Dump Prometheus Adapter logs (last 30 lines)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- Prometheus Adapter Logs (last 30 lines) ---\n")
			adapterPods, err := k8sClient.CoreV1().Pods(monitoringNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=prometheus-adapter",
			})
			if err == nil && len(adapterPods.Items) > 0 {
				tailLines := int64(30)
				logs, err := k8sClient.CoreV1().Pods(monitoringNamespace).GetLogs(adapterPods.Items[0].Name, &corev1.PodLogOptions{
					TailLines: &tailLines,
				}).DoRaw(ctx)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", string(logs))
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get adapter logs: %v\n", err)
				}
			}

			_, _ = fmt.Fprintf(GinkgoWriter, "========== END DIAGNOSTIC DUMP ==========\n\n")
		}

		// Track if we ever saw scaling
		scalingDetected := false

		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, hpaName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get HPA")

			// Get current arrival rate from VariantAutoscaling for logging
			arrivalRate := "unknown"
			va := &v1alpha1.VariantAutoscaling{}
			if err := crClient.Get(ctx, client.ObjectKey{Namespace: llmDNamespace, Name: vaName}, va); err == nil {
				arrivalRate = va.Status.CurrentAlloc.Load.ArrivalRate
			}

			// Check if HPA has processed the new metric value
			g.Expect(hpa.Status.CurrentMetrics).NotTo(BeEmpty(), "HPA should have current metrics")

			// The HPA should show a target value > 1 (indicating scale-up needed)
			if !lowLoad {
				for _, metric := range hpa.Status.CurrentMetrics {
					if metric.External != nil && metric.External.Metric.Name == constants.InfernoDesiredReplicas {
						currentValue := metric.External.Current.AverageValue
						g.Expect(currentValue).NotTo(BeNil(), "Current metric value should not be nil")

						currentReplicas := currentValue.AsApproximateFloat64()
						_, _ = fmt.Fprintf(GinkgoWriter, "HPA current metric value: %.2f (minReplicas: %d, desiredReplicas: %d, arrivalRate: %s req/s)\n",
							currentReplicas, hpaMinReplicas, hpa.Status.DesiredReplicas, arrivalRate)

						if currentReplicas > float64(hpaMinReplicas) {
							scalingDetected = true
						}

						g.Expect(currentReplicas).To(BeNumerically(">", float64(hpaMinReplicas)),
							"HPA should see increased replica recommendation above minReplicas")
					}
				}
				// Check desired replicas - compare against minReplicas, not current state
				// This ensures test passes regardless of starting deployment state
				g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", hpaMinReplicas),
					fmt.Sprintf("HPA should desire more replicas than minReplicas (desired: %d, min: %d)", hpa.Status.DesiredReplicas, hpaMinReplicas))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected (arrivalRate: %s), skipping HPA scale-up check\n", arrivalRate)
			}
		}, 3*time.Minute, 10*time.Second).Should(Succeed(), func() string {
			// On failure, dump diagnostics
			if !scalingDetected {
				dumpDiagnostics()
			}
			return "scaling did not occur - see diagnostic dump above"
		})

		_, _ = fmt.Fprintf(GinkgoWriter, "HPA triggered scale-up\n")
	})

	It("should scale deployment to match recommended replicas", func() {
		By("monitoring deployment for actual scale-up")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")

			scaledReplicas = deploy.Status.ReadyReplicas
			_, _ = fmt.Fprintf(GinkgoWriter, "Current ready replicas: %d (initial: %d, desired: %d)\n",
				scaledReplicas, initialReplicas, scaledOptimized)

			// Verify that deployment has scaled up
			if !lowLoad {
				// Only expect scaling when load is high - compare against minReplicas, not starting state
				g.Expect(deploy.Status.Replicas).To(BeNumerically(">", hpaMinReplicas),
					fmt.Sprintf("Deployment should have more total replicas than minReplicas under high load (current: %d, min: %d)", deploy.Status.Replicas, hpaMinReplicas))
				g.Expect(scaledReplicas).To(BeNumerically(">=", scaledOptimized),
					fmt.Sprintf("Deployment should have at least %d ready replicas to match optimizer recommendation", scaledOptimized))
			} else {
				// Under low load, scaling up is optional
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping scale-up check\n")
			}

		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Deployment scaled to %d replicas (up from %d, target was %d)\n", scaledReplicas, initialReplicas, scaledOptimized)
	})

	It("should maintain scaled state while load is active", func() {
		By("verifying deployment stays scaled for at least 30 seconds")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(llmDNamespace).Get(ctx, deployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get deployment")
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", scaledOptimized),
				fmt.Sprintf("Deployment should maintain at least %d replicas while job is running", scaledOptimized))
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "Deployment maintained %d replicas under load (target: %d)\n", scaledReplicas, scaledOptimized)
	})

	It("should complete the load generation jobs successfully", func() {
		By("waiting for jobs to complete")
		Eventually(func(g Gomega) {
			succeededCount := 0
			for i := 1; i <= numLoadWorkers; i++ {
				jobName := fmt.Sprintf("%s-%d", jobBaseName, i)
				job, err := k8sClient.BatchV1().Jobs(llmDNamespace).Get(ctx, jobName, metav1.GetOptions{})
				if err != nil {
					continue
				}
				if job.Status.Succeeded >= 1 {
					succeededCount++
				}
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Jobs completed: %d / %d\n", succeededCount, numLoadWorkers)
			g.Expect(succeededCount).To(BeNumerically(">=", numLoadWorkers),
				fmt.Sprintf("All %d jobs should have succeeded, got %d", numLoadWorkers, succeededCount))
		}, jobCompletionTimeout, 15*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "All load generation jobs completed successfully\n")
	})

	AfterAll(func() {
		By("cleaning up load generation jobs")
		deleteParallelLoadJobs(ctx, jobBaseName, llmDNamespace, numLoadWorkers)

		_, _ = fmt.Fprintf(GinkgoWriter, "Test completed - scaled from %d to %d replicas\n", initialReplicas, scaledReplicas)
	})
})

// createLoadGenerationJob creates a lightweight Kubernetes Job that generates load using curl
// This uses a small image and sends requests directly to the vllm service to avoid gateway routing issues
func createLoadGenerationJob(name, namespace, vllmService string, workerID, numRequests int) *batchv1.Job {
	backoffLimit := int32(0)

	// Script that sends concurrent requests to saturate the vLLM instance
	// All Go template parameters are defined at the top as shell variables for clarity
	script := fmt.Sprintf(`#!/bin/sh
# =============================================================================
# Load Generator Configuration (injected from Go constants)
# =============================================================================
WORKER_ID=%d
TOTAL_REQUESTS=%d
BATCH_SIZE=%d
CURL_TIMEOUT=%d
MAX_TOKENS=%d
BATCH_SLEEP=%s
MODEL_ID="%s"
VLLM_SERVICE="%s"
MAX_RETRIES=24
RETRY_DELAY=5

# =============================================================================
# Script Start
# =============================================================================
echo "Load generator worker $WORKER_ID starting..."
echo "Sending $TOTAL_REQUESTS requests to $VLLM_SERVICE:8200"

# Wait for vllm-service to be ready
echo "Waiting for $VLLM_SERVICE to be ready..."
CONNECTED=false
for i in $(seq 1 $MAX_RETRIES); do
  if curl -s -o /dev/null -w "%%{http_code}" http://$VLLM_SERVICE:8200/v1/models 2>/dev/null | grep -q 200; then
    echo "Connection test passed on attempt $i"
    CONNECTED=true
    break
  fi
  echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
  sleep $RETRY_DELAY
done

if [ "$CONNECTED" != "true" ]; then
  echo "ERROR: Cannot connect to $VLLM_SERVICE after $MAX_RETRIES attempts"
  exit 1
fi

# Send requests aggressively in parallel batches (ignore individual curl failures)
SENT=0
while [ $SENT -lt $TOTAL_REQUESTS ]; do
  for i in $(seq 1 $BATCH_SIZE); do
    if [ $SENT -ge $TOTAL_REQUESTS ]; then break; fi
    (curl -s -o /dev/null --max-time $CURL_TIMEOUT -X POST http://$VLLM_SERVICE:8200/v1/completions \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"$MODEL_ID\",\"prompt\":\"Write a detailed explanation of machine learning algorithms.\",\"max_tokens\":$MAX_TOKENS}" || true) &
    SENT=$((SENT + 1))
  done
  echo "Worker $WORKER_ID: sent $SENT / $TOTAL_REQUESTS requests..."
  sleep $BATCH_SLEEP
done

# Wait for all to complete at the end
wait || true

echo "Worker $WORKER_ID: completed all $TOTAL_REQUESTS requests"
exit 0
`, workerID, numRequests, batchSize, curlTimeoutSeconds, maxTokens, batchSleepDuration, modelID, vllmService)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"experiment": "load-gen-e2e",
				"worker":     fmt.Sprintf("%d", workerID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"experiment": "load-gen-e2e",
						"worker":     fmt.Sprintf("%d", workerID),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "load-generator",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("2Gi"),
									corev1.ResourceCPU:    resource.MustParse("2"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
}

// createParallelLoadJobs creates multiple parallel load generation jobs
func createParallelLoadJobs(ctx context.Context, baseName, namespace, vllmService string, numWorkers, requestsPerWorker int) error {
	for i := 1; i <= numWorkers; i++ {
		jobName := fmt.Sprintf("%s-%d", baseName, i)
		job := createLoadGenerationJob(jobName, namespace, vllmService, i, requestsPerWorker)
		_, err := k8sClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create job %s: %w", jobName, err)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Created load generation job: %s\n", jobName)
	}
	return nil
}

// deleteParallelLoadJobs deletes all parallel load generation jobs
func deleteParallelLoadJobs(ctx context.Context, baseName, namespace string, numWorkers int) {
	propagationPolicy := metav1.DeletePropagationBackground
	for i := 1; i <= numWorkers; i++ {
		jobName := fmt.Sprintf("%s-%d", baseName, i)
		err := k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Warning: failed to delete job %s: %v\n", jobName, err)
		}
	}
}
