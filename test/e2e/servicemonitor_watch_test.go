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
	"os"
	"strings"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	serviceMonitorName = "workload-variant-autoscaler-controller-manager-metrics-monitor"
)

// Helper function to get int64 pointer
func int64Ptr(i int64) *int64 {
	return &i
}

var _ = Describe("ServiceMonitor Watch E2E", Ordered, func() {
	var (
		ctx        context.Context
		serviceMon *unstructured.Unstructured
	)

	BeforeAll(func() {
		if os.Getenv("KUBECONFIG") == "" {
			Skip("KUBECONFIG is not set; skipping e2e test")
		}

		initializeK8sClient()
		ctx = context.Background()

		By("Ensuring controller namespace exists")
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: controllerNamespace,
			},
		}
		err := crClient.Create(ctx, ns)
		Expect(client.IgnoreAlreadyExists(err)).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Cleaning up ServiceMonitor if it exists")
		if serviceMon != nil {
			err := crClient.Delete(ctx, serviceMon)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		}
	})

	Context("ServiceMonitor deletion detection", func() {
		It("should detect ServiceMonitor deletion and log warning", func() {
			By("Checking if ServiceMonitor already exists (created by Helm chart)")
			serviceMon = utils.CreateControllerServiceMonitor(controllerNamespace)
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      serviceMonitorName,
				Namespace: controllerNamespace,
			}, serviceMon)

			if errors.IsNotFound(err) {
				By("ServiceMonitor doesn't exist, creating it")
				err = crClient.Create(ctx, serviceMon)
				Expect(err).NotTo(HaveOccurred(), "Should be able to create ServiceMonitor")
			} else if err != nil {
				Fail(fmt.Sprintf("Unexpected error checking ServiceMonitor: %v", err))
			} else {
				By("ServiceMonitor already exists (created by Helm chart), using existing one")
			}

			By("Waiting for ServiceMonitor to be created")
			Eventually(func() error {
				sm := &unstructured.Unstructured{}
				sm.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "monitoring.coreos.com",
					Version: "v1",
					Kind:    "ServiceMonitor",
				})
				return crClient.Get(ctx, client.ObjectKey{
					Name:      serviceMonitorName,
					Namespace: controllerNamespace,
				}, sm)
			}, 30*time.Second, 1*time.Second).Should(Succeed())

			By("Deleting the ServiceMonitor")
			err = crClient.Delete(ctx, serviceMon)
			Expect(err).NotTo(HaveOccurred(), "Should be able to delete ServiceMonitor")

			By("Waiting for ServiceMonitor to be deleted")
			Eventually(func() bool {
				sm := &unstructured.Unstructured{}
				sm.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "monitoring.coreos.com",
					Version: "v1",
					Kind:    "ServiceMonitor",
				})
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      serviceMonitorName,
					Namespace: controllerNamespace,
				}, sm)
				return client.IgnoreNotFound(err) == nil
			}, 30*time.Second, 1*time.Second).Should(BeTrue())

			By("Waiting a moment for controller to process the deletion event")
			time.Sleep(5 * time.Second)

			By("Verifying controller detected ServiceMonitor deletion via logs")
			// Note: Events on deleted objects may not be easily queryable, so we check logs instead
			// The watch handler should log immediately when ServiceMonitor is deleted
			Eventually(func() bool {
				pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				if err != nil || len(pods.Items) == 0 {
					return false
				}

				// Check logs from the controller pod
				pod := pods.Items[0]
				logs, err := k8sClient.CoreV1().Pods(controllerNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					TailLines: int64Ptr(200), // Get last 200 lines to catch recent log entries
				}).DoRaw(ctx)
				if err != nil {
					return false
				}

				logString := string(logs)
				// Check for the critical warning message about ServiceMonitor deletion
				// The exact message from the controller is: "CRITICAL: ServiceMonitor being deleted"
				hasCriticalLog := strings.Contains(logString, "CRITICAL: ServiceMonitor being deleted") ||
					strings.Contains(logString, "ServiceMonitor being deleted")

				if hasCriticalLog {
					// Also verify it mentions the ServiceMonitor name
					return strings.Contains(logString, serviceMonitorName)
				}
				return false
			}, 1*time.Minute, 3*time.Second).Should(BeTrue(), "Controller should log ServiceMonitor deletion with critical warning")

			By("Verifying controller continues to function")
			// Check that controller pod is still running
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "control-plane=controller-manager",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(pods.Items)).To(BeNumerically(">", 0), "Controller pod should exist")
			for _, pod := range pods.Items {
				Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), fmt.Sprintf("Controller pod %s should be running", pod.Name))
			}
		})
	})

	Context("ServiceMonitor creation detection", func() {
		It("should detect ServiceMonitor creation", func() {
			By("Checking if ServiceMonitor exists")
			sm := &unstructured.Unstructured{}
			sm.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "monitoring.coreos.com",
				Version: "v1",
				Kind:    "ServiceMonitor",
			})
			sm.SetName(serviceMonitorName)
			sm.SetNamespace(controllerNamespace)
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      serviceMonitorName,
				Namespace: controllerNamespace,
			}, sm)

			if err == nil {
				By("ServiceMonitor already exists, deleting it first to test creation")
				err = crClient.Delete(ctx, sm)
				Expect(err).NotTo(HaveOccurred(), "Should be able to delete ServiceMonitor")
			} else {
				By("ServiceMonitor doesn't exist, proceeding with creation test")
			}

			By("Waiting for ServiceMonitor to be deleted")
			Eventually(func() bool {
				sm := &unstructured.Unstructured{}
				sm.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "monitoring.coreos.com",
					Version: "v1",
					Kind:    "ServiceMonitor",
				})
				err := crClient.Get(ctx, client.ObjectKey{
					Name:      serviceMonitorName,
					Namespace: controllerNamespace,
				}, sm)
				return client.IgnoreNotFound(err) == nil
			}, 30*time.Second, 1*time.Second).Should(BeTrue())

			By("Creating the ServiceMonitor")
			serviceMon = utils.CreateControllerServiceMonitor(controllerNamespace)
			err = crClient.Create(ctx, serviceMon)
			Expect(err).NotTo(HaveOccurred(), "Should be able to create ServiceMonitor")

			By("Waiting for ServiceMonitor to be created")
			Eventually(func() error {
				sm := &unstructured.Unstructured{}
				sm.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "monitoring.coreos.com",
					Version: "v1",
					Kind:    "ServiceMonitor",
				})
				return crClient.Get(ctx, client.ObjectKey{
					Name:      serviceMonitorName,
					Namespace: controllerNamespace,
				}, sm)
			}, 30*time.Second, 1*time.Second).Should(Succeed())

			By("Verifying controller continues to function")
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "control-plane=controller-manager",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(pods.Items)).To(BeNumerically(">", 0), "Controller pod should exist")
		})
	})

	Context("Startup verification", func() {
		It("should verify ServiceMonitor exists on startup", func() {
			By("Ensuring ServiceMonitor exists")
			serviceMon = utils.CreateControllerServiceMonitor(controllerNamespace)
			err := crClient.Get(ctx, client.ObjectKey{
				Name:      serviceMonitorName,
				Namespace: controllerNamespace,
			}, serviceMon)
			if client.IgnoreNotFound(err) != nil {
				err = crClient.Create(ctx, serviceMon)
				Expect(err).NotTo(HaveOccurred(), "Should be able to create ServiceMonitor")
			}

			By("Verifying ServiceMonitor exists")
			Eventually(func() error {
				sm := &unstructured.Unstructured{}
				sm.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "monitoring.coreos.com",
					Version: "v1",
					Kind:    "ServiceMonitor",
				})
				return crClient.Get(ctx, client.ObjectKey{
					Name:      serviceMonitorName,
					Namespace: controllerNamespace,
				}, sm)
			}, 30*time.Second, 1*time.Second).Should(Succeed(), "ServiceMonitor should exist")

			By("Verifying controller is running")
			pods, err := k8sClient.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "control-plane=controller-manager",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(pods.Items)).To(BeNumerically(">", 0), "Controller pod should exist")
		})
	})
})
