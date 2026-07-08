package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

const secondaryControllerOverlayPathEnv = "WVA_E2E_SECONDARY_OVERLAY_PATH"

func splitImage(image string) (string, string) {
	lastColon := strings.LastIndex(image, ":")
	lastSlash := strings.LastIndex(image, "/")
	if lastColon == -1 || lastColon < lastSlash {
		return image, "latest"
	}
	return image[:lastColon], image[lastColon+1:]
}

// Multi-controller isolation test: two namespace-scoped WVA controllers manage VAs with the same
// name in different namespaces. Each has its own KEDA ScaledObject whose Prometheus trigger
// filters by namespace label, ensuring cross-namespace metric leakage cannot occur.
var _ = Describe("Multi-controller Tests - Dual namespace-scoped isolation", Label("multi-controller"), func() {
	// TODO: replace the patch-and-restore CRB workaround with a dedicated Kind cluster per scenario.
	// The secondary overlay overwrites shared ClusterRoleBindings; the current workaround restores
	// the primary and creates per-deployment secondary bindings. This is safe on Kind (cluster is
	// torn down after the suite) but unsafe on shared persistent clusters (OpenShift).
	Context("Dual namespace-scoped controllers isolation", Serial, Ordered, func() {
		var (
			primaryNamespace    = "llm-d-sim"
			secondaryNamespace  = "llm-d-sim-dual"
			secondaryController = "workload-variant-autoscaler-system-dual"
			primarySOName       = "smoke-test-dual-primary-hpa"
			secondarySOName     = "smoke-test-dual-secondary-hpa"
			primaryModelName    = "smoke-test-dual-primary-ms"
			secondaryModelName  = "smoke-test-dual-secondary-ms"
			poolName            = "smoke-test-dual-pool"
			sharedVAName        = "smoke-test-dual-shared-va"
			controllerInstance  = "dual-secondary"
		)

		BeforeAll(func() {
			if cfg.Environment == "openshift" {
				Skip("Dual-controller test skipped on OpenShift: patch-and-restore of cluster-scoped CRBs is unsafe on shared persistent clusters")
			}
			if cfg.Environment != envKindEmulator {
				Skip("Dual-controller smoke scenario currently targets kind-emulator setup")
			}

			By("Creating secondary workload namespace")
			_, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: secondaryNamespace},
			}, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred(), "Failed to create secondary workload namespace")
			}

			By("Installing secondary namespace-scoped controller via Kustomize")
			primaryController, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).Get(ctx, "wva-controller-manager", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Failed to read primary controller deployment image")
			Expect(primaryController.Spec.Template.Spec.Containers).NotTo(BeEmpty(), "Primary controller deployment should contain containers")
			imageRepo, imageTag := splitImage(primaryController.Spec.Template.Spec.Containers[0].Image)
			overlayPath := os.Getenv(secondaryControllerOverlayPathEnv)
			Expect(overlayPath).NotTo(BeEmpty(),
				"Missing %s; set it to the config/e2e/secondary-controller overlay directory (use an absolute path; go test cwd is the test package dir)", secondaryControllerOverlayPathEnv)
			_, statErr := os.Stat(overlayPath)
			Expect(statErr).NotTo(HaveOccurred(), "Invalid %s path: %s", secondaryControllerOverlayPathEnv, overlayPath)

			managerKustomizationPath := filepath.Join(overlayPath, "../../../../config/base/manager/kustomization.yaml")
			managerContent, managerReadErr := os.ReadFile(managerKustomizationPath)
			Expect(managerReadErr).NotTo(HaveOccurred(), "Failed to read config/base/manager/kustomization.yaml")
			var baseImageName string
			for _, line := range strings.Split(string(managerContent), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "newName:") {
					baseImageName = strings.TrimSpace(strings.TrimPrefix(trimmed, "newName:"))
					break
				}
			}
			Expect(baseImageName).NotTo(BeEmpty(), "Failed to extract base image name from config/base/manager/kustomization.yaml")

			tmpOverlay, tmpErr := os.MkdirTemp("", "wva-secondary-overlay-*")
			Expect(tmpErr).NotTo(HaveOccurred(), "Failed to create temp overlay dir")
			Expect(os.Symlink(overlayPath, tmpOverlay+"/base")).To(Succeed())

			kustomizationContent := strings.Join([]string{
				"apiVersion: kustomize.config.k8s.io/v1beta1",
				"kind: Kustomization",
				"namespace: " + secondaryController,
				"resources:",
				"- ./base",
				"images:",
				"- name: " + baseImageName,
				"  newName: " + imageRepo,
				`  newTag: "` + imageTag + `"`,
				"patches:",
				"- target:",
				"    kind: Deployment",
				"    name: wva-controller-manager",
				"  patch: |",
				`    - op: add`,
				`      path: /spec/template/spec/containers/0/env/-`,
				`      value: {"name": "CONTROLLER_INSTANCE", "value": "` + controllerInstance + `"}`,
			}, "\n")
			Expect(os.WriteFile(tmpOverlay+"/kustomization.yaml", []byte(kustomizationContent), 0600)).To(Succeed())

			cmd := exec.Command("kubectl", "apply", "-k", tmpOverlay, "--server-side", "--force-conflicts")
			out, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Secondary controller kustomize install failed: %s", string(out))

			const crbName = "wva-manager-rolebinding"
			const crbNameSecondary = "workload-variant-autoscaler-" + crbName + "-secondary"
			restoreOut, restoreErr := exec.Command("kubectl", "patch", "clusterrolebinding", crbName,
				"--type=json",
				"-p", `[{"op":"replace","path":"/subjects/0/namespace","value":"`+cfg.WVANamespace+`"}]`,
			).CombinedOutput()
			Expect(restoreErr).NotTo(HaveOccurred(), "Failed to restore primary ClusterRoleBinding: %s", string(restoreOut))

			createOut, createErr := exec.Command("kubectl", "create", "clusterrolebinding", crbNameSecondary,
				"--clusterrole=wva-manager-role",
				"--serviceaccount="+secondaryController+":wva-controller-manager",
			).CombinedOutput()
			Expect(createErr).NotTo(HaveOccurred(), "Failed to create secondary ClusterRoleBinding: %s", string(createOut))

			const eppCRBName = "wva-epp-metrics-reader-role-binding"
			const eppCRBNameSecondary = "workload-variant-autoscaler-" + eppCRBName + "-secondary"
			eppRestoreOut, eppRestoreErr := exec.Command("kubectl", "patch", "clusterrolebinding", eppCRBName,
				"--type=json",
				"-p", `[{"op":"replace","path":"/subjects/0/namespace","value":"`+cfg.WVANamespace+`"}]`,
			).CombinedOutput()
			Expect(eppRestoreErr).NotTo(HaveOccurred(), "Failed to restore primary epp-metrics ClusterRoleBinding: %s", string(eppRestoreOut))

			eppCreateOut, eppCreateErr := exec.Command("kubectl", "create", "clusterrolebinding", eppCRBNameSecondary,
				"--clusterrole=wva-epp-metrics-reader-role",
				"--serviceaccount="+secondaryController+":wva-epp-metrics-reader",
			).CombinedOutput()
			Expect(eppCreateErr).NotTo(HaveOccurred(), "Failed to create secondary epp-metrics ClusterRoleBinding: %s", string(eppCreateOut))

			const metricsAuthCRBName = "wva-metrics-auth-rolebinding"
			const metricsAuthCRBNameSecondary = "workload-variant-autoscaler-" + metricsAuthCRBName + "-secondary"
			metricsAuthRestoreOut, metricsAuthRestoreErr := exec.Command("kubectl", "patch", "clusterrolebinding", metricsAuthCRBName,
				"--type=json",
				"-p", `[{"op":"replace","path":"/subjects/0/namespace","value":"`+cfg.WVANamespace+`"}]`,
			).CombinedOutput()
			Expect(metricsAuthRestoreErr).NotTo(HaveOccurred(), "Failed to restore primary metrics-auth ClusterRoleBinding: %s", string(metricsAuthRestoreOut))

			metricsAuthCreateOut, metricsAuthCreateErr := exec.Command("kubectl", "create", "clusterrolebinding", metricsAuthCRBNameSecondary,
				"--clusterrole=wva-metrics-auth-role",
				"--serviceaccount="+secondaryController+":wva-controller-manager",
			).CombinedOutput()
			Expect(metricsAuthCreateErr).NotTo(HaveOccurred(), "Failed to create secondary metrics-auth ClusterRoleBinding: %s", string(metricsAuthCreateOut))

			DeferCleanup(func() {
				_ = exec.Command("kubectl", "delete", "clusterrolebinding", crbNameSecondary, "--ignore-not-found=true").Run()
				_ = exec.Command("kubectl", "delete", "clusterrolebinding", eppCRBNameSecondary, "--ignore-not-found=true").Run()
				_ = exec.Command("kubectl", "delete", "clusterrolebinding", metricsAuthCRBNameSecondary, "--ignore-not-found=true").Run()
				_ = exec.Command("kubectl", "delete", "namespace", secondaryController, "--ignore-not-found=true").Run()
				_ = exec.Command("kubectl", "delete", "namespace", secondaryNamespace, "--ignore-not-found=true").Run()
				_ = os.RemoveAll(tmpOverlay)
			})

			By("Waiting for secondary controller to be ready")
			Eventually(func(g Gomega) {
				pods, listErr := k8sClient.CoreV1().Pods(secondaryController).List(ctx, metav1.ListOptions{
					LabelSelector: "control-plane=controller-manager",
				})
				g.Expect(listErr).NotTo(HaveOccurred())
				g.Expect(pods.Items).NotTo(BeEmpty(), "Expected secondary controller pod")
				ready := 0
				for _, pod := range pods.Items {
					if pod.Status.Phase != corev1.PodRunning {
						continue
					}
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							ready++
							break
						}
					}
				}
				g.Expect(ready).To(BeNumerically(">", 0), "Expected at least one ready secondary controller pod")
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Creating model services in both namespaces")
			err = fixtures.EnsureModelService(ctx, k8sClient, primaryNamespace, primaryModelName, poolName, cfg.ModelID, sharedVAName, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary model service")
			err = fixtures.EnsureService(ctx, k8sClient, primaryNamespace, primaryModelName, primaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, primaryNamespace, primaryModelName, primaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary ServiceMonitor")

			err = fixtures.EnsureModelService(ctx, k8sClient, secondaryNamespace, secondaryModelName, poolName, cfg.ModelID, sharedVAName, cfg.UseSimulator, cfg.MaxNumSeqs)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary model service")
			err = fixtures.EnsureService(ctx, k8sClient, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode", 8000)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary service")
			err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, secondaryNamespace, secondaryModelName, secondaryModelName+"-decode")
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary ServiceMonitor")

			By("Creating overlapping VA names for each controller namespace")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, primaryNamespace, sharedVAName, primaryModelName+"-decode", cfg.ModelID, "H100", "")
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary VA")
			err = fixtures.EnsureVariantAutoscalingWithDefaults(ctx, crClient, secondaryNamespace, sharedVAName, secondaryModelName+"-decode", cfg.ModelID, "H100", controllerInstance)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary VA")

			By("Creating ScaledObjects in both namespaces")
			// buildScaledObject queries wva_desired_replicas{variant_name=..., namespace=...}.
			// The namespace label in the Prometheus query ensures each ScaledObject reads only
			// the metric emitted for its own workload namespace, providing cross-namespace isolation.
			err = fixtures.EnsureScaledObject(ctx, crClient, primaryNamespace, primarySOName, primaryModelName+"-decode", sharedVAName, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create primary ScaledObject")
			err = fixtures.EnsureScaledObject(ctx, crClient, secondaryNamespace, secondarySOName, secondaryModelName+"-decode", sharedVAName, 1, 10, cfg.MonitoringNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create secondary ScaledObject")
		})

		It("should reconcile VAs in both namespaces independently", func() {
			By("Waiting for both VAs to reach TargetResolved and MetricsAvailable")
			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				c := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				c = variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeTargetResolved)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				primaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err := crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: primaryNamespace}, primaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc := variantautoscalingv1alpha1.GetCondition(primaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))

				secondaryVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
				err = crClient.Get(ctx, client.ObjectKey{Name: sharedVAName, Namespace: secondaryNamespace}, secondaryVA)
				g.Expect(err).NotTo(HaveOccurred())
				mc = variantautoscalingv1alpha1.GetCondition(secondaryVA, variantautoscalingv1alpha1.TypeMetricsAvailable)
				g.Expect(mc).NotTo(BeNil())
				g.Expect(mc.Status).To(Equal(metav1.ConditionTrue))
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})

		It("should expose isolated wva_desired_replicas per namespace via KEDA", func() {
			// Each ScaledObject's Prometheus trigger queries wva_desired_replicas{namespace=<ns>},
			// so KEDA reads only the metric from the workload namespace it manages.
			// Populated CurrentMetrics on the KEDA-managed HPA in each namespace confirms
			// that the per-namespace Prometheus series is independently consumed.

			By("Verifying KEDA reads wva_desired_replicas for primary namespace")
			Eventually(func(g Gomega) {
				hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(primaryNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
				for i := range hpaList.Items {
					if hpaList.Items[i].Spec.ScaleTargetRef.Name == primaryModelName+"-decode" {
						kedaHPA = &hpaList.Items[i]
						break
					}
				}
				g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the primary deployment")
				g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
					"Primary KEDA HPA should have CurrentMetrics populated — wva_desired_replicas{namespace=%q} is being consumed", primaryNamespace)
				GinkgoWriter.Printf("Primary KEDA HPA CurrentMetrics: %d entries\n", len(kedaHPA.Status.CurrentMetrics))
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			By("Verifying KEDA reads wva_desired_replicas for secondary namespace independently")
			Eventually(func(g Gomega) {
				hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(secondaryNamespace).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
				for i := range hpaList.Items {
					if hpaList.Items[i].Spec.ScaleTargetRef.Name == secondaryModelName+"-decode" {
						kedaHPA = &hpaList.Items[i]
						break
					}
				}
				g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the secondary deployment")
				g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
					"Secondary KEDA HPA should have CurrentMetrics populated — wva_desired_replicas{namespace=%q} is being consumed", secondaryNamespace)
				GinkgoWriter.Printf("Secondary KEDA HPA CurrentMetrics: %d entries\n", len(kedaHPA.Status.CurrentMetrics))
			}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
		})
	})
})
