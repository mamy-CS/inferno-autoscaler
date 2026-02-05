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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	testutils "github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("ConfigMap Handler", func() {
	Context("Namespace-Local ConfigMap Watching", func() {
		ctx := context.Background()
		var controllerReconciler *VariantAutoscalingReconciler

		BeforeEach(func() {
			logging.NewTestLogger()
			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workload-variant-autoscaler-system",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, ns))).NotTo(HaveOccurred())

			By("creating the required configmaps")
			configMap := testutils.CreateVariantAutoscalingConfigMap(config.DefaultConfigMapName, ns.Name)
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, configMap))).NotTo(HaveOccurred())

			By("creating the reconciler")
			controllerReconciler = &VariantAutoscalingReconciler{
				Client:   k8sClient,
				Recorder: record.NewFakeRecorder(10),
				Config:   config.NewTestConfig(),
			}
		})

		It("should watch namespace-local ConfigMaps for namespaces with opt-in label", func() {
			By("Creating a namespace with opt-in label")
			labeledNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "labeled-namespace",
					Labels: map[string]string{
						constants.NamespaceConfigEnabledLabelKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, labeledNS)).To(Succeed())

			By("Creating a namespace-local ConfigMap in labeled namespace")
			namespaceLocalCM := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.SaturationConfigMapName(),
					Namespace: "labeled-namespace",
				},
				Data: map[string]string{
					"default": "kvCacheThreshold: 0.70\nqueueLengthThreshold: 3",
				},
			}
			Expect(k8sClient.Create(ctx, namespaceLocalCM)).To(Succeed())

			By("Verifying namespace is considered for watching")
			// The shouldWatchNamespaceLocalConfigMap should return true for labeled namespace
			// even without VAs
			result := controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "labeled-namespace")
			Expect(result).To(BeTrue(), "Labeled namespace should be watched even without VAs")

			By("Verifying namespace without label is not watched (without VAs)")
			unlabeledNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "unlabeled-namespace",
				},
			}
			Expect(k8sClient.Create(ctx, unlabeledNS)).To(Succeed())

			result = controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "unlabeled-namespace")
			Expect(result).To(BeFalse(), "Unlabeled namespace without VAs should not be watched")

			// Cleanup
			Expect(k8sClient.Delete(ctx, namespaceLocalCM)).To(Succeed())
			Expect(k8sClient.Delete(ctx, labeledNS)).To(Succeed())
			Expect(k8sClient.Delete(ctx, unlabeledNS)).To(Succeed())
		})

		It("should watch namespace-local ConfigMaps for namespaces with VAs (VA-based tracking)", func() {
			By("Creating a namespace without opt-in label")
			vaNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "va-namespace",
				},
			}
			Expect(k8sClient.Create(ctx, vaNS)).To(Succeed())

			By("Creating a VA in the namespace to trigger tracking")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "va-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					ModelID: "test-model",
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("Tracking the namespace (simulating VA reconciliation)")
			controllerReconciler.trackNamespace("test-va", "va-namespace")

			By("Verifying namespace is considered for watching")
			result := controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "va-namespace")
			Expect(result).To(BeTrue(), "Namespace with VAs should be watched")

			// Cleanup
			controllerReconciler.untrackNamespace("test-va", "va-namespace")
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, vaNS)).To(Succeed())
		})

		It("should handle idempotent namespace tracking (retry-safe)", func() {
			By("Tracking the same VA multiple times (simulating retries)")
			controllerReconciler.trackNamespace("test-va-1", "test-namespace")
			controllerReconciler.trackNamespace("test-va-1", "test-namespace") // Same VA again (retry)
			controllerReconciler.trackNamespace("test-va-1", "test-namespace") // Same VA again (retry)

			By("Verifying namespace is tracked only once")
			result := controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "test-namespace")
			Expect(result).To(BeTrue(), "Namespace should be tracked")

			By("Adding another VA to the same namespace")
			controllerReconciler.trackNamespace("test-va-2", "test-namespace")

			By("Verifying namespace is still tracked")
			result = controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "test-namespace")
			Expect(result).To(BeTrue(), "Namespace should still be tracked with multiple VAs")

			By("Untracking one VA")
			controllerReconciler.untrackNamespace("test-va-1", "test-namespace")

			By("Verifying namespace is still tracked (other VA still exists)")
			result = controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "test-namespace")
			Expect(result).To(BeTrue(), "Namespace should still be tracked")

			By("Untracking the last VA")
			controllerReconciler.untrackNamespace("test-va-2", "test-namespace")

			By("Verifying namespace is no longer tracked")
			result = controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "test-namespace")
			Expect(result).To(BeFalse(), "Namespace should no longer be tracked")
		})

		It("should exclude namespaces with exclude annotation from ConfigMap watching", func() {
			By("Creating a namespace with exclude annotation")
			excludedNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "excluded-namespace",
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, excludedNS)).To(Succeed())

			By("Creating a VA in the excluded namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va",
					Namespace: "excluded-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					ModelID: "test-model",
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("Tracking the namespace (simulating VA reconciliation)")
			controllerReconciler.trackNamespace("test-va", "excluded-namespace")

			By("Verifying excluded namespace is not watched for ConfigMaps (even with VAs)")
			result := controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "excluded-namespace")
			Expect(result).To(BeFalse(), "Excluded namespace should not be watched even if it has VAs")

			By("Verifying exclusion takes precedence over opt-in label")
			excludedNSWithOptIn := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "excluded-with-optin",
					Labels: map[string]string{
						constants.NamespaceConfigEnabledLabelKey: "true",
					},
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, excludedNSWithOptIn)).To(Succeed())

			result = controllerReconciler.shouldWatchNamespaceLocalConfigMap(ctx, "excluded-with-optin")
			Expect(result).To(BeFalse(), "Exclusion should take precedence over opt-in label")

			// Cleanup
			controllerReconciler.untrackNamespace("test-va", "excluded-namespace")
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, excludedNS)).To(Succeed())
			Expect(k8sClient.Delete(ctx, excludedNSWithOptIn)).To(Succeed())
		})

		It("should exclude namespaces with exclude annotation from VA reconciliation (predicate)", func() {
			By("Creating a namespace with exclude annotation")
			excludedNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "excluded-va-namespace",
					Annotations: map[string]string{
						constants.NamespaceExcludeAnnotationKey: "true",
					},
				},
			}
			Expect(k8sClient.Create(ctx, excludedNS)).To(Succeed())

			By("Creating a VA in the excluded namespace")
			va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va-excluded",
					Namespace: "excluded-va-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					ModelID: "test-model",
				},
			}
			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			By("Verifying predicate filters out VA from excluded namespace")
			predicate := VariantAutoscalingPredicate(k8sClient)
			result := predicate.Create(event.CreateEvent{Object: va})
			Expect(result).To(BeFalse(), "VA in excluded namespace should be filtered out by predicate")

			By("Verifying predicate allows VA from non-excluded namespace")
			normalNS := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "normal-namespace",
				},
			}
			Expect(k8sClient.Create(ctx, normalNS)).To(Succeed())

			normalVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-va-normal",
					Namespace: "normal-namespace",
				},
				Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
					ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
						Kind: "Deployment",
						Name: "test-deployment",
					},
					ModelID: "test-model",
				},
			}
			Expect(k8sClient.Create(ctx, normalVA)).To(Succeed())

			result = predicate.Create(event.CreateEvent{Object: normalVA})
			Expect(result).To(BeTrue(), "VA in non-excluded namespace should pass predicate")

			// Cleanup
			Expect(k8sClient.Delete(ctx, va)).To(Succeed())
			Expect(k8sClient.Delete(ctx, normalVA)).To(Succeed())
			Expect(k8sClient.Delete(ctx, excludedNS)).To(Succeed())
			Expect(k8sClient.Delete(ctx, normalNS)).To(Succeed())
		})
	})

	Context("Main ConfigMap Handler - Immutable Parameter Handling", func() {
		ctx := context.Background()
		var controllerReconciler *VariantAutoscalingReconciler
		var fakeRecorder *record.FakeRecorder

		BeforeEach(func() {
			logging.NewTestLogger()
			fakeRecorder = record.NewFakeRecorder(10)
			cfg := config.NewTestConfig()
			cfg.Static.Prometheus = &interfaces.PrometheusConfig{
				BaseURL: "https://prometheus-initial:9090",
			}
			// Set initial optimization interval
			cfg.Dynamic.OptimizationInterval = 60 * time.Second
			// Initialize Prometheus cache config with defaults
			cfg.Dynamic.PrometheusCache = &config.CacheConfig{
				Enabled:             true,
				TTL:                 30 * time.Second,
				CleanupInterval:     1 * time.Minute,
				FetchInterval:       30 * time.Second,
				FreshnessThresholds: config.DefaultFreshnessThresholds(),
			}

			controllerReconciler = &VariantAutoscalingReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Config:   cfg,
				Recorder: fakeRecorder,
			}
		})

		It("should apply mutable parameters even when immutable changes are detected", func() {
			By("Creating a ConfigMap with both immutable and mutable parameter changes")
			cm := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.ConfigMapName(),
					Namespace: config.Namespace(),
				},
				Data: map[string]string{
					"GLOBAL_OPT_INTERVAL":          "120s",                        // Mutable - should be applied
					"PROMETHEUS_BASE_URL":          "https://prometheus-new:9090", // Immutable - should be rejected
					"PROMETHEUS_METRICS_CACHE_TTL": "60s",                         // Mutable - should be applied
				},
			}

			By("Calling handleMainConfigMap")
			controllerReconciler.handleMainConfigMap(ctx, cm, config.Namespace(), true)

			By("Verifying immutable parameter change was rejected (warning event emitted)")
			// Check that event was emitted - Eventf sends to channel synchronously
			// Event message contains "Prometheus BaseURL" (human-readable name), not "PROMETHEUS_BASE_URL"
			Eventually(fakeRecorder.Events, 5*time.Second).Should(Receive(And(
				ContainSubstring("ImmutableConfigChangeRejected"),
				ContainSubstring("Prometheus BaseURL"),
			)))

			By("Verifying mutable parameter (GLOBAL_OPT_INTERVAL) was still applied")
			Expect(controllerReconciler.Config.OptimizationInterval()).To(Equal(120*time.Second),
				"GLOBAL_OPT_INTERVAL should be updated even when immutable changes are detected")

			By("Verifying Prometheus BaseURL was not changed (immutable)")
			Expect(controllerReconciler.Config.Static.Prometheus.BaseURL).To(Equal("https://prometheus-initial:9090"),
				"PROMETHEUS_BASE_URL should not be changed (immutable)")

			By("Verifying Prometheus cache config was updated")
			cacheConfig := controllerReconciler.Config.PrometheusCacheConfig()
			Expect(cacheConfig).NotTo(BeNil())
			Expect(cacheConfig.TTL).To(Equal(60*time.Second),
				"PROMETHEUS_METRICS_CACHE_TTL should be updated even when immutable changes are detected")
		})

		It("should apply only mutable parameters when ConfigMap contains both types", func() {
			By("Creating a ConfigMap with multiple immutable and mutable changes")
			cm := &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.ConfigMapName(),
					Namespace: config.Namespace(),
				},
				Data: map[string]string{
					"GLOBAL_OPT_INTERVAL":              "90s",                         // Mutable
					"PROMETHEUS_BASE_URL":              "https://prometheus-new:9090", // Immutable
					"METRICS_BIND_ADDRESS":             ":9443",                       // Immutable
					"PROMETHEUS_METRICS_CACHE_ENABLED": "false",                       // Mutable
				},
			}

			initialInterval := controllerReconciler.Config.OptimizationInterval()
			initialCacheConfig := controllerReconciler.Config.PrometheusCacheConfig()
			Expect(initialCacheConfig).NotTo(BeNil(), "Initial cache config should not be nil")
			initialCacheEnabled := initialCacheConfig.Enabled

			By("Calling handleMainConfigMap")
			controllerReconciler.handleMainConfigMap(ctx, cm, config.Namespace(), true)

			By("Verifying warning event was emitted for immutable changes")
			// Check that event was emitted - Eventf sends to channel synchronously
			Eventually(fakeRecorder.Events, 5*time.Second).Should(Receive(ContainSubstring("ImmutableConfigChangeRejected")))

			By("Verifying mutable parameters were applied")
			Expect(controllerReconciler.Config.OptimizationInterval()).To(Equal(90*time.Second),
				"GLOBAL_OPT_INTERVAL should be updated")
			Expect(controllerReconciler.Config.OptimizationInterval()).NotTo(Equal(initialInterval),
				"Interval should have changed")

			cacheConfig := controllerReconciler.Config.PrometheusCacheConfig()
			Expect(cacheConfig).NotTo(BeNil())
			Expect(cacheConfig.Enabled).To(BeFalse(),
				"PROMETHEUS_METRICS_CACHE_ENABLED should be updated")
			Expect(cacheConfig.Enabled).NotTo(Equal(initialCacheEnabled),
				"Cache enabled should have changed")

			By("Verifying immutable parameters were not changed")
			Expect(controllerReconciler.Config.Static.Prometheus.BaseURL).To(Equal("https://prometheus-initial:9090"),
				"PROMETHEUS_BASE_URL should not be changed")
			Expect(controllerReconciler.Config.Static.MetricsAddr).NotTo(Equal(":9443"),
				"METRICS_BIND_ADDRESS should not be changed")
		})
	})
})
