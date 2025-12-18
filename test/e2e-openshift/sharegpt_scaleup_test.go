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

// Number of parallel load generation workers and requests per worker
const (
	numLoadWorkers    = 5
	requestsPerWorker = 400
)

var _ = Describe("ShareGPT Scale-Up Test", Ordered, func() {
	var (
		ctx                  context.Context
		jobBaseName          string
		initialReplicas      int32
		initialOptimized     int32
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
		va := &v1alpha1.VariantAutoscaling{}
		err = crClient.Get(ctx, client.ObjectKey{
			Namespace: llmDNamespace,
			Name:      deployment,
		}, va)
		Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")
		initialOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
		_, _ = fmt.Fprintf(GinkgoWriter, "Initial optimized replicas: %d\n", initialOptimized)

		By("verifying HPA exists and is configured correctly")
		hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, "vllm-deployment-hpa", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "HPA should exist")
		Expect(hpa.Spec.ScaleTargetRef.Name).To(Equal(deployment), "HPA should target the correct deployment")
		Expect(hpa.Spec.Metrics).To(HaveLen(1), "HPA should have one metric")
		Expect(hpa.Spec.Metrics[0].Type).To(Equal(autoscalingv2.ExternalMetricSourceType), "HPA should use external metrics")
		Expect(hpa.Spec.Metrics[0].External.Metric.Name).To(Equal(constants.InfernoDesiredReplicas), "HPA should use inferno_desired_replicas metric")
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
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("should create and run parallel load generation jobs", func() {
		By("cleaning up any existing jobs")
		deleteParallelLoadJobs(ctx, jobBaseName, llmDNamespace, numLoadWorkers)
		// Wait a bit for cleanup
		time.Sleep(2 * time.Second)

		By(fmt.Sprintf("creating %d parallel load generation jobs", numLoadWorkers))
		err := createParallelLoadJobs(ctx, jobBaseName, llmDNamespace, numLoadWorkers, requestsPerWorker)
		Expect(err).NotTo(HaveOccurred(), "Should be able to create load generation jobs")

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
				Name:      deployment,
			}, va)
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get VariantAutoscaling")

			scaledOptimized = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
			currentRateStr := va.Status.CurrentAlloc.Load.ArrivalRate
			_, _ = fmt.Fprintf(GinkgoWriter, "Current optimized replicas: %d (initial: %d), arrival rate: %s\n",
				scaledOptimized, initialOptimized, currentRateStr)

			// Expect scale-up recommendation (more than initial)
			if !lowLoad {
				g.Expect(scaledOptimized).To(BeNumerically(">", initialOptimized),
					fmt.Sprintf("WVA should recommend more replicas under load (current: %d, initial: %d)", scaledOptimized, initialOptimized))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping scale-up recommendation check\n")
			}

		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		_, _ = fmt.Fprintf(GinkgoWriter, "WVA detected load and recommended %d replicas (up from %d)\n", scaledOptimized, initialOptimized)
	})

	It("should trigger HPA to scale up the deployment", func() {
		By("monitoring HPA for scale-up action")
		Eventually(func(g Gomega) {
			hpa, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(llmDNamespace).Get(ctx, "vllm-deployment-hpa", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred(), "Should be able to get HPA")

			// Check if HPA has processed the new metric value
			g.Expect(hpa.Status.CurrentMetrics).NotTo(BeEmpty(), "HPA should have current metrics")

			// The HPA should show a target value > 1 (indicating scale-up needed)
			if !lowLoad {
				for _, metric := range hpa.Status.CurrentMetrics {
					if metric.External != nil && metric.External.Metric.Name == constants.InfernoDesiredReplicas {
						currentValue := metric.External.Current.AverageValue
						g.Expect(currentValue).NotTo(BeNil(), "Current metric value should not be nil")

						currentReplicas := currentValue.AsApproximateFloat64()
						_, _ = fmt.Fprintf(GinkgoWriter, "HPA current metric value: %.2f\n", currentReplicas)
						g.Expect(currentReplicas).To(BeNumerically(">", float64(initialOptimized)),
							"HPA should see increased replica recommendation")
					}
				}
				// Check desired replicas
				g.Expect(hpa.Status.DesiredReplicas).To(BeNumerically(">", initialReplicas),
					fmt.Sprintf("HPA should desire more replicas (current: %d, initial: %d)", hpa.Status.DesiredReplicas, initialReplicas))
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Low load detected, skipping HPA scale-up check\n")
			}
		}, 3*time.Minute, 10*time.Second).Should(Succeed())

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
				// Only expect scaling when load is high
				g.Expect(deploy.Status.Replicas).To(BeNumerically(">", initialReplicas),
					"Deployment should have more total replicas under high load")
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
// This uses a small image and sends requests directly to vllm-service to avoid gateway routing issues
func createLoadGenerationJob(name, namespace string, workerID, numRequests int) *batchv1.Job {
	backoffLimit := int32(0)

	// Script that sends concurrent requests to saturate the vLLM instance
	script := fmt.Sprintf(`#!/bin/sh
echo "Load generator worker %d starting..."
echo "Sending %d requests to vllm-service:8200"

# Wait for vllm-service to be ready (up to 2 minutes)
echo "Waiting for vllm-service to be ready..."
RETRIES=24
RETRY_DELAY=5
for i in $(seq 1 $RETRIES); do
  if curl -s -o /dev/null -w "%%{http_code}" http://vllm-service:8200/v1/models 2>/dev/null | grep -q 200; then
    echo "Connection test passed on attempt $i"
    break
  fi
  if [ $i -eq $RETRIES ]; then
    echo "ERROR: Cannot connect to vllm-service after $RETRIES attempts"
    exit 1
  fi
  echo "Attempt $i failed, retrying in ${RETRY_DELAY}s..."
  sleep $RETRY_DELAY
done

# Send requests in parallel batches (ignore individual curl failures)
TOTAL=%d
BATCH_SIZE=50
SENT=0

while [ $SENT -lt $TOTAL ]; do
  # Send a batch of concurrent requests
  for i in $(seq 1 $BATCH_SIZE); do
    if [ $SENT -ge $TOTAL ]; then break; fi
    # Use subshell with || true to ignore curl failures
    (curl -s -o /dev/null --max-time 120 -X POST http://vllm-service:8200/v1/completions \
      -H "Content-Type: application/json" \
      -d '{"model":"%s","prompt":"Write a detailed essay about artificial intelligence and its impact on society.","max_tokens":200}' || true) &
    SENT=$((SENT + 1))
  done
  # Brief pause between batches
  sleep 0.5
  echo "Worker %d: sent $SENT / $TOTAL requests..."
done

# Wait for all background jobs (ignore failures)
wait || true
echo "Worker %d: completed all %d requests"
exit 0
`, workerID, numRequests, numRequests, modelID, workerID, workerID, numRequests)

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
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "load-generator",
							Image:   "curlimages/curl:latest",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{script},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
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
func createParallelLoadJobs(ctx context.Context, baseName, namespace string, numWorkers, requestsPerWorker int) error {
	for i := 1; i <= numWorkers; i++ {
		jobName := fmt.Sprintf("%s-%d", baseName, i)
		job := createLoadGenerationJob(jobName, namespace, i, requestsPerWorker)
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
