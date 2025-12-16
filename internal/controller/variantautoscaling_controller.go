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
	"fmt"
	"os"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d-incubation/workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/saturation"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// VariantAutoscalingReconciler reconciles a variantAutoscaling object
type VariantAutoscalingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Kubernetes events for observability. We keep it to follow Kubernetes
	// controller best practices and provide visibility into critical issues (e.g., ServiceMonitor
	// deletion) that may not be immediately apparent from logs alone. Events are accessible via
	// `kubectl get events` and can be monitored by cluster operators and external tooling.
	Recorder record.EventRecorder

	PromAPI promv1.API

	// MetricsCollector is the interface for collecting metrics from various backends
	// Defaults to Prometheus collector, but can be swapped for other backends (e.g., EPP)
	MetricsCollector interfaces.MetricsCollector

	// Saturation scaling config cache (thread-safe, updated on ConfigMap changes)
	saturationConfigCache      map[string]interfaces.SaturationScalingConfig
	saturationConfigCacheMutex sync.RWMutex
	saturationConfigLoaded     bool // Track if initial load succeeded
}

// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;update;patch;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	configMapName = "workload-variant-autoscaler-variantautoscaling-config"
	// ServiceMonitor constants for watching controller's own metrics ServiceMonitor
	serviceMonitorName = "workload-variant-autoscaler-controller-manager-metrics-monitor"
	// Environment variable to enable experimental hybrid-based optimization
	// When "on", runs both saturation analyzer and model-based optimizer with arbitration
	// When "model-only" runs model-based optimizer only
	// When "off" or unset, runs saturation analyzer only (default, reactive mode)
	EnvExperimentalHybridOptimization = "EXPERIMENTAL_HYBRID_OPTIMIZATION"
	saturationConfigMapName           = "saturation-scaling-config"
)

func getNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "workload-variant-autoscaler-system"
}

var (
	// ServiceMonitor GVK for watching controller's own metrics ServiceMonitor
	serviceMonitorGVK = schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	}
	configMapNamespace = getNamespace()
)

func (r *VariantAutoscalingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// NOTE: The reconciliation loop is being incrementally refactored so things may look a bit messy.
	// Changes in progress:
	// - reconcile loop will process one VA at a time. During the refactoring it does both, one and all

	// BEGIN: Per VA logic

	// Get the specific VA object that triggered this reconciliation
	var va llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	if err := r.Get(ctx, req.NamespacedName, &va); err != nil { // Get returns, by default, a deep copy of the object
		if apierrors.IsNotFound(err) {
			logger.Log.Infof("VariantAutoscaling resource not found, may have been deleted: name=%s, namespace=%s", req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		logger.Log.Errorf("Unable to fetch VariantAutoscaling: name=%s, namespace=%s, error=%v", req.Name, req.Namespace, err)
		return ctrl.Result{}, err
	}

	// Skip if the VA is being deleted
	if !va.DeletionTimestamp.IsZero() {
		logger.Log.Infof("VariantAutoscaling is being deleted, skipping reconciliation: name=%s, namespace=%s", va.Name, va.Namespace)
		return ctrl.Result{}, nil
	}
	logger.Log.Infof("Reconciling VariantAutoscaling: name=%s, namespace=%s, modelID=%s", va.Name, va.Namespace, va.Spec.ModelID)

	// Attempts to resolve the target model variant
	// TODO: replace by proper lookup mechanism using spec.scaleTargetRef in future
	scaleTargetName := va.Name

	// TODO: generalize to other scale target kind in future
	var deploy appsv1.Deployment
	if err := utils.GetDeploymentWithBackoff(ctx, r.Client, scaleTargetName, va.Namespace, &deploy); err != nil {
		logger.Log.Errorf("Failed to get scale target Deployment: name=%s, namespace=%s, error=%v", scaleTargetName, va.Namespace, err)
		llmdVariantAutoscalingV1alpha1.SetCondition(&va,
			llmdVariantAutoscalingV1alpha1.TypeTargetResolved,
			metav1.ConditionFalse,
			"ScaleTargetNotFound",
			fmt.Sprintf("Scale target Deployment not found: name=%s, namespace=%s", scaleTargetName, va.Namespace),
		)

		// Update VA status
		// TODO: refactor to use retry utility function.
		// UpdateStatusWithBackoff does not work as it goes not refresh the object before update
		// UpdateStatusWithOptimisticLocking is too complex and not suitable for this case
		if err := r.Status().Update(ctx, &va); err != nil {
			logger.Log.Errorf("Failed to update VariantAutoscaling status: name=%s, namespace=%s, error=%v", va.Name, va.Namespace, err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, err
	}

	// TODO: Refactor to record mutation and apply as one update operation.
	llmdVariantAutoscalingV1alpha1.SetCondition(&va,
		llmdVariantAutoscalingV1alpha1.TypeTargetResolved,
		metav1.ConditionTrue,
		"ScaleTargetFound",
		fmt.Sprintf("Scale target Deployment found: name=%s, namespace=%s", scaleTargetName, va.Namespace),
	)

	// END: Per VA logic

	// BELOW is the logic that processes all VAs together for optimization
	// Note: This logic has been moved to the Saturation Engine (internal/engines/saturation/engine.go).
	// The controller now only handles pure reconciliation tasks (VA status updates, etc.)
	// dependent on the Engine's decisions or other external factors.

	// Currently, the Engine runs its own optimization loop, so we don't need to do anything here regarding global optimization.

	return ctrl.Result{}, nil
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) ([]interfaces.VariantReplicaState, error) {
	states := make([]interfaces.VariantReplicaState, 0, len(vas))

	for _, va := range vas {
		// Get current replicas from deployment using ScaleTargetRef
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy); err != nil {
			logger.Log.Warnf("Failed to get deployment for VA, using status: name=%s, deployment=%s, error=%v", va.Name, va.GetScaleTargetName(), err)
			// Fallback to status if deployment fetch fails
			states = append(states, interfaces.VariantReplicaState{
				VariantName:     va.Name,
				CurrentReplicas: va.Status.CurrentAlloc.NumReplicas,
				DesiredReplicas: va.Status.DesiredOptimizedAlloc.NumReplicas,
			})
			continue
		}

		currentReplicas := int(deploy.Status.Replicas)
		if currentReplicas == 0 && deploy.Spec.Replicas != nil {
			currentReplicas = int(*deploy.Spec.Replicas)
		}

		states = append(states, interfaces.VariantReplicaState{
			VariantName:     va.Name,
			CurrentReplicas: currentReplicas,
			DesiredReplicas: va.Status.DesiredOptimizedAlloc.NumReplicas,
		})
	}

	return states, nil
}

// convertSaturationTargetsToDecisions converts saturation-only targets to VariantDecisions.
// Used when model-based optimizer is disabled (saturation-only mode).
func convertSaturationTargetsToDecisions(
	saturationTargets map[string]int,
	saturationAnalysis *interfaces.ModelSaturationAnalysis,
	variantStates []interfaces.VariantReplicaState,
) []interfaces.VariantDecision {
	decisions := make([]interfaces.VariantDecision, 0, len(saturationTargets))

	// Build variant analysis map for quick lookup
	vaMap := make(map[string]*interfaces.VariantSaturationAnalysis)
	for i := range saturationAnalysis.VariantAnalyses {
		va := &saturationAnalysis.VariantAnalyses[i]
		vaMap[va.VariantName] = va
	}

	// Build state map for quick lookup
	stateMap := make(map[string]interfaces.VariantReplicaState)
	for _, state := range variantStates {
		stateMap[state.VariantName] = state
	}

	for variantName, targetReplicas := range saturationTargets {
		state := stateMap[variantName]
		va := vaMap[variantName]

		var action interfaces.SaturationAction
		if targetReplicas > state.CurrentReplicas {
			action = interfaces.ActionScaleUp
		} else if targetReplicas < state.CurrentReplicas {
			action = interfaces.ActionScaleDown
		} else {
			action = interfaces.ActionNoChange
		}

		decision := interfaces.VariantDecision{
			VariantName:        variantName,
			Namespace:          saturationAnalysis.Namespace,
			ModelID:            saturationAnalysis.ModelID,
			CurrentReplicas:    state.CurrentReplicas,
			TargetReplicas:     targetReplicas,
			DesiredReplicas:    state.DesiredReplicas,
			Action:             action,
			SaturationBased:    true,
			SaturationOnly:     true,
			ModelBasedDecision: false,
			SafetyOverride:     false,
			Reason:             "saturation-only mode: " + string(action),
		}

		if va != nil {
			decision.AcceleratorName = va.AcceleratorName
			decision.Cost = va.Cost
		} else {
			logger.Log.Warnf("No variant analysis found for decision: variant=%s (metrics may be unavailable)", variantName)
		}

		decisions = append(decisions, decision)
	}

	return decisions
}

// RunSaturationAnalysis performs saturation analysis for a model and returns Saturation targets.
func RunSaturationAnalysis(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	SaturationConfig interfaces.SaturationScalingConfig,
	k8sClient client.Client,
	metricsCollector interfaces.MetricsCollector,
) (map[string]int, *interfaces.ModelSaturationAnalysis, []interfaces.VariantReplicaState, error) {
	if len(modelVAs) == 0 {
		return nil, nil, nil, fmt.Errorf("no VAs provided for model %s", modelID)
	}

	namespace := modelVAs[0].Namespace // All VAs of same model are in same namespace

	// Build variant costs map, deployments map, and VAs map for metrics collection
	variantCosts := make(map[string]float64)
	deployments := make(map[string]*appsv1.Deployment)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]
		cost := saturation.DefaultVariantCost // default
		if va.Spec.VariantCost != "" {
			if parsedCost, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
				cost = parsedCost
			}
		}
		variantCosts[va.Name] = cost

		// Get the deployment for this VA using ScaleTargetRef
		var deploy appsv1.Deployment
		err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.Log.Debugf("Could not get deployment for VA: variant=%s, deployment=%s, error=%v", va.Name, va.GetScaleTargetName(), err)
			continue
		}
		deployments[va.Name] = &deploy
		variantAutoscalings[va.Name] = va
	}

	// Collect Saturation metrics using the configured collector
	replicaMetrics, err := metricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.Log.Debugf("Collected Saturation metrics: modelID=%s, namespace=%s, metricsCount=%d",
		modelID, namespace, len(replicaMetrics))

	// If no metrics available, skip saturation analysis entirely
	// This prevents creating invalid decisions when pods are not ready or metrics are unavailable
	if len(replicaMetrics) == 0 {
		logger.Log.Infof("No saturation metrics available for model, skipping analysis: modelID=%s, namespace=%s",
			modelID, namespace)
		return nil, nil, nil, nil // Return nil to signal skip due to metrics unavailable, not error
	}

	// Analyze saturation across all variants
	saturationAnalyzer := saturation.NewAnalyzer()
	saturationAnalysis, err := saturationAnalyzer.AnalyzeModelSaturation(ctx, modelID, namespace, replicaMetrics, SaturationConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to analyze Saturation for model %s: %w", modelID, err)
	}

	logger.Log.Infof("saturation analysis completed: modelID=%s, totalReplicas=%d, nonSaturated=%d, shouldScaleUp=%v, scaleDownSafe=%v",
		modelID, saturationAnalysis.TotalReplicas, saturationAnalysis.NonSaturatedCount,
		saturationAnalysis.ShouldScaleUp, saturationAnalysis.ScaleDownSafe)

	// Build variant states (current and desired replicas)
	variantStates, err := BuildVariantStates(ctx, modelVAs, k8sClient)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to build variant states for model %s: %w", modelID, err)
	}

	// Calculate saturation-based targets
	saturationTargets := saturationAnalyzer.CalculateSaturationTargets(saturationAnalysis, variantStates)

	logger.Log.Debugf("Saturation targets calculated: modelID=%s, targets=%v",
		modelID, saturationTargets)

	return saturationTargets, saturationAnalysis, variantStates, nil
}

// CollectMetricsForSaturationMode collects metrics and populates CurrentAlloc for VAs in saturation-only mode.
func CollectMetricsForSaturationMode(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
	metricsCollector interfaces.MetricsCollector,
) error {
	for i := range modelVAs {
		va := &modelVAs[i]
		modelName := va.Spec.ModelID

		// Get accelerator name from VA labels - required field
		accName := va.Labels["inference.optimization/acceleratorName"]
		if accName == "" {
			logger.Log.Warnf("Missing accelerator name label for VA, skipping: variant=%s", va.Name)
			continue
		}

		// Extract accelerator cost from VA.Spec.VariantCost - required field
		if va.Spec.VariantCost == "" {
			logger.Log.Warnf("Missing variant cost for VA, skipping: variant=%s", va.Name)
			continue
		}
		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			logger.Log.Warnf("Invalid variant cost for VA, skipping: variant=%s, cost=%s, error=%v", va.Name, va.Spec.VariantCost, err)
			continue
		}

		// Get Deployment using ScaleTargetRef
		var deploy appsv1.Deployment
		err = utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.Log.Debugf("Could not get deployment for VA, skipping: variant=%s, deployment=%s, error=%v", va.Name, va.GetScaleTargetName(), err)
			continue // Skip VAs without deployments
		}

		// Fetch latest VA from API server (use VA name, not deployment name - they are now decoupled)
		var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		err = utils.GetVariantAutoscalingWithBackoff(ctx, k8sClient, va.Name, va.Namespace, &updateVA)
		if err != nil {
			logger.Log.Debugf("Unable to get VA: variant=%s, error=%v", va.Name, err)
			continue
		}

		// Validate metrics availability before collecting
		metricsValidation := metricsCollector.ValidateMetricsAvailability(ctx, modelName, deploy.Namespace)

		// Update MetricsAvailable condition based on validation result
		if metricsValidation.Available {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionTrue,
				metricsValidation.Reason,
				metricsValidation.Message)
		} else {
			// Metrics unavailable - set condition and skip
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				metricsValidation.Reason,
				metricsValidation.Message)

			logger.Log.Warnf("Metrics unavailable for VA, skipping: variant=%s, reason=%s, troubleshooting=%s",
				updateVA.Name, metricsValidation.Reason, metricsValidation.Message)
			continue
		}

		// Collect raw metrics from collector
		metrics, err := metricsCollector.AddMetricsToOptStatus(ctx, &updateVA, deploy, cost)
		if err != nil {
			logger.Log.Debugf("Unable to fetch metrics for VA: variant=%s, error=%v", updateVA.Name, err)
			continue
		}

		// Assemble Allocation struct from raw metrics
		currentAllocation, err := BuildAllocationFromMetrics(metrics, &updateVA, deploy, cost)
		if err != nil {
			logger.Log.Debugf("Unable to build allocation for VA: variant=%s, error=%v", updateVA.Name, err)
			continue
		}

		// Update the VA in vaMap with populated CurrentAlloc
		updateVA.Status.CurrentAlloc = currentAllocation

		// Update vaMap with the VA that has CurrentAlloc populated
		vaMap[updateVA.Name] = &updateVA

		logger.Log.Infof("Metrics collected for VA: variant=%s, replicas=%d, accelerator=%s, ttft=%sms, itl=%sms, cost=%s",
			updateVA.Name,
			currentAllocation.NumReplicas,
			currentAllocation.Accelerator,
			currentAllocation.TTFTAverage,
			currentAllocation.ITLAverage,
			currentAllocation.VariantCost)
	}

	return nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (r *VariantAutoscalingReconciler) applySaturationDecisions(
	ctx context.Context,
	decisions []interfaces.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) error {
	for _, decision := range decisions {
		logger.Log.Infof("Processing decision: variant=%s, action=%s, current=%d→target=%d",
			decision.VariantName, decision.Action, decision.CurrentReplicas, decision.TargetReplicas)

		va, ok := vaMap[decision.VariantName]
		if !ok {
			logger.Log.Errorf("VA not found in vaMap: variant=%s", decision.VariantName)
			continue
		}

		logger.Log.Debugf("Found VA in map: variant=%s, hasCurrentAlloc=%v, accelerator=%s",
			va.Name, va.Status.CurrentAlloc.Accelerator != "", va.Status.CurrentAlloc.Accelerator)

		// Fetch latest version from API server to avoid conflicts
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Log.Errorf("failed to get latest VA from API server: name=%s, error=%v", va.Name, err)
			continue
		}

		// Skip status update if we don't have valid metrics (CurrentAlloc) OR valid decision (AcceleratorName)
		// This prevents CRD validation errors when accelerator field is invalid
		if va.Status.CurrentAlloc.Accelerator == "" || decision.AcceleratorName == "" || len(decision.AcceleratorName) < 2 {
			logger.Log.Warnf("Skipping status update for VA without valid metrics or accelerator: variant=%s, hasCurrentAlloc=%v, decisionAccelerator=%s",
				decision.VariantName, va.Status.CurrentAlloc.Accelerator != "", decision.AcceleratorName)
			continue
		}

		// Update CurrentAlloc from vaMap
		updateVa.Status.CurrentAlloc = va.Status.CurrentAlloc

		// Update DesiredOptimizedAlloc with Saturation decision
		acceleratorName := decision.AcceleratorName

		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: decision.TargetReplicas,
			Accelerator: acceleratorName,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false

		// Set condition based on decision characteristics
		if decision.SafetyOverride {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				"SaturationSafetyOverride",
				fmt.Sprintf("saturation safety override: %s", decision.Reason))
		} else if decision.SaturationOnly {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				"SaturationOnlyMode",
				fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", decision.Reason, decision.TargetReplicas))
		} else {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", decision.Reason, decision.TargetReplicas))
		}

		// Emit metrics for external autoscalers
		act := actuator.NewActuator(r.Client)
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			logger.Log.Errorf("failed to emit metrics for external autoscalers: variant=%s, error=%v", updateVa.Name, err)
		} else {
			logger.Log.Infof("Successfully emitted metrics for external autoscalers: variant=%s, targetReplicas=%d, accelerator=%s, SaturationOnly=%v",
				updateVa.Name, decision.TargetReplicas, decision.AcceleratorName, decision.SaturationOnly)
			updateVa.Status.Actuation.Applied = true
		}

		// Update VA status
		if err := utils.UpdateStatusWithBackoff(ctx, r.Client, &updateVa, utils.StandardBackoff, "VariantAutoscaling"); err != nil {
			logger.Log.Errorf("failed to update VA status after retries: name=%s, error=%v", updateVa.Name, err)
			continue
		}

		logger.Log.Infof("Applied Saturation decision: variant=%s, action=%s, current=%d, target=%d, reason=%s",
			decision.VariantName, decision.Action, decision.CurrentReplicas, decision.TargetReplicas, decision.Reason)

		// Invalidate cache when scaling occurs (replica count changes)
		if decision.Action != interfaces.ActionNoChange {
			// Type assert to PrometheusCollector to access cache invalidation
			if promCollector, ok := r.MetricsCollector.(*prometheus.PrometheusCollector); ok {
				modelID := decision.ModelID
				namespace := decision.Namespace
				variantName := decision.VariantName
				promCollector.InvalidateCacheForVariant(modelID, namespace, variantName)
				logger.Log.Debugf("Invalidated metrics cache after scaling: variant=%s, action=%s",
					variantName, decision.Action)
			}
		}
	}

	return nil
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
// Strategy: Use previous desired replicas if available, otherwise use current replicas.
// This prevents HPA from using completely stale metrics and provides a safe no-op signal.
func (r *VariantAutoscalingReconciler) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) {
	act := actuator.NewActuator(r.Client)

	for _, va := range modelVAs {
		// Get latest version from API server
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Log.Errorf("Safety net: failed to get latest VA from API server: name=%s, error=%v", va.Name, err)
			continue
		}

		// Determine fallback desired replicas
		var desiredReplicas int32
		var fallbackSource string

		// Strategy 1: Use previous desired replicas if available
		if updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = int32(updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
			fallbackSource = "previous-desired"
		} else {
			// Strategy 2: Use current replicas from deployment (safe no-op)
			currentReplicas, err := act.GetCurrentDeploymentReplicas(ctx, &updateVa)
			if err != nil {
				logger.Log.Warnf("Safety net: failed to get current replicas, using VA status: variant=%s, error=%v",
					updateVa.Name, err)
				currentReplicas = int32(updateVa.Status.CurrentAlloc.NumReplicas)
			}
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Get current replicas for metric emission
		currentReplicas, err := act.GetCurrentDeploymentReplicas(ctx, &updateVa)
		if err != nil {
			logger.Log.Warnf("Safety net: failed to get current replicas for metrics: variant=%s, error=%v",
				updateVa.Name, err)
			currentReplicas = int32(updateVa.Status.CurrentAlloc.NumReplicas)
		}

		// Determine accelerator - try status first, then labels, skip if unavailable
		accelerator := updateVa.Status.DesiredOptimizedAlloc.Accelerator
		if accelerator == "" {
			accelerator = updateVa.Status.CurrentAlloc.Accelerator
		}
		if accelerator == "" {
			// Try to get from VA labels as last resort
			if val, ok := updateVa.Labels["inference.optimization/acceleratorName"]; ok && val != "" {
				accelerator = val
			}
		}
		if accelerator == "" {
			logger.Log.Warnf("Safety net: skipping metric emission - no accelerator name available: variant=%s",
				updateVa.Name)
			continue
		}

		// Emit safety net metrics
		if err := act.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			&updateVa,
			currentReplicas,
			desiredReplicas,
			accelerator,
		); err != nil {
			logger.Log.Errorf("Safety net: failed to emit metrics: variant=%s, error=%v", updateVa.Name, err)
			continue
		}

		logger.Log.Infof("Safety net activated: emitted fallback metrics: variant=%s, currentReplicas=%d, desiredReplicas=%d, accelerator=%s, fallbackSource=%s",
			updateVa.Name,
			currentReplicas,
			desiredReplicas,
			accelerator,
			fallbackSource)
	}
}

// prepareVariantAutoscalings collects and prepares all data for optimization.
func (r *VariantAutoscalingReconciler) prepareVariantAutoscalings(
	ctx context.Context,
	activeVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	acceleratorCm map[string]map[string]string,
	serviceClassCm map[string]string,
	systemData *infernoConfig.SystemData,
) (*llmdVariantAutoscalingV1alpha1.VariantAutoscalingList, map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, map[string]*interfaces.ModelAnalyzeResponse, error) {
	var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	allAnalyzerResponses := make(map[string]*interfaces.ModelAnalyzeResponse)
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for _, va := range activeVAs {
		modelName := va.Spec.ModelID
		if modelName == "" {
			logger.Log.Infof("variantAutoscaling missing modelName label, skipping optimization: variantAutoscaling-name=%s", va.Name)
			continue
		}

		entry, className, err := utils.FindModelSLO(serviceClassCm, modelName)
		if err != nil {
			logger.Log.Errorf("failed to locate SLO for model: variantAutoscaling-name=%s, modelName=%s, error=%v", va.Name, modelName, err)
			continue
		}
		logger.Log.Infof("Found SLO for model: model=%s, class=%s, slo-tpot=%d, slo-ttft=%d", modelName, className, entry.SLOTPOT, entry.SLOTTFT)

		for _, modelAcceleratorProfile := range va.Spec.ModelProfile.Accelerators {
			if utils.AddModelAcceleratorProfileToSystemData(systemData, modelName, &modelAcceleratorProfile) != nil {
				logger.Log.Errorf("variantAutoscaling bad model accelerator profile data, skipping optimization: variantAutoscaling-name=%s", va.Name)
				continue
			}
		}

		accName := va.Labels["inference.optimization/acceleratorName"]
		acceleratorCostVal, ok := acceleratorCm[accName]["cost"]
		if !ok {
			logger.Log.Errorf("variantAutoscaling missing accelerator cost in configMap, skipping optimization: variantAutoscaling-name=%s", va.Name)
			continue
		}
		acceleratorCostValFloat, err := strconv.ParseFloat(acceleratorCostVal, 32)
		if err != nil {
			logger.Log.Errorf("variantAutoscaling unable to parse accelerator cost in configMap, skipping optimization: variantAutoscaling-name=%s", va.Name)
			continue
		}

		// Get Deployment using ScaleTargetRef
		var deploy appsv1.Deployment
		err = utils.GetDeploymentWithBackoff(ctx, r.Client, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.Log.Errorf("failed to get Deployment after retries: variantAutoscaling-name=%s, deployment=%s, error=%v", va.Name, va.GetScaleTargetName(), err)
			continue
		}

		// Fetch latest VA from API server (use VA name, not deployment name - they are now decoupled)
		var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		err = utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA)
		if err != nil {
			logger.Log.Errorf("unable to get variantAutoscaling: variantAutoscaling-name=%s, namespace=%s, error=%v", va.Name, va.Namespace, err)
			continue
		}

		// Set ownerReference early, before metrics validation, to ensure it's always set
		// This ensures the VA will be garbage collected when the Deployment is deleted
		if !metav1.IsControlledBy(&updateVA, &deploy) {
			original := updateVA.DeepCopy()
			err := controllerutil.SetControllerReference(&deploy, &updateVA, r.Scheme, controllerutil.WithBlockOwnerDeletion(false))
			if err != nil {
				logger.Log.Errorf("failed to set ownerReference: variantAutoscaling-name=%s, error=%v", updateVA.Name, err)
				continue
			}

			// Patch metadata change (ownerReferences)
			patch := client.MergeFrom(original)
			if err := r.Patch(ctx, &updateVA, patch); err != nil {
				logger.Log.Errorf("failed to patch ownerReference: variantAutoscaling-name=%s, error=%v", updateVA.Name, err)
				continue
			}
			logger.Log.Infof("Set ownerReference on VariantAutoscaling: variantAutoscaling-name=%s, owner=%s", updateVA.Name, deploy.Name)
		}

		// Validate metrics availability before collecting metrics
		metricsValidation := r.MetricsCollector.ValidateMetricsAvailability(ctx, modelName, deploy.Namespace)

		// Update MetricsAvailable condition based on validation result
		if metricsValidation.Available {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionTrue,
				metricsValidation.Reason,
				metricsValidation.Message)
		} else {
			// Metrics unavailable - just log and skip (don't update status yet to avoid CRD validation errors)
			// Conditions will be set properly once metrics become available or after first successful collection
			logger.Log.Warnf("Metrics unavailable, skipping optimization for variant: variant=%s, namespace=%s, model=%s, reason=%s, troubleshooting=%s",
				updateVA.Name,
				updateVA.Namespace,
				modelName,
				metricsValidation.Reason,
				metricsValidation.Message)
			continue
		}

		// Collect raw metrics from collector
		metrics, err := r.MetricsCollector.AddMetricsToOptStatus(ctx, &updateVA, deploy, acceleratorCostValFloat)
		if err != nil {
			logger.Log.Errorf("unable to fetch metrics, skipping this variantAutoscaling loop: error=%v", err)
			// Don't update status here - will be updated in next reconcile when metrics are available
			continue
		}

		// Assemble Allocation struct from raw metrics
		currentAllocation, err := BuildAllocationFromMetrics(metrics, &updateVA, deploy, acceleratorCostValFloat)
		if err != nil {
			logger.Log.Errorf("unable to build allocation, skipping this variantAutoscaling loop: error=%v", err)
			continue
		}
		updateVA.Status.CurrentAlloc = currentAllocation

		if err := utils.AddServerInfoToSystemData(systemData, &updateVA, className); err != nil {
			logger.Log.Infof("variantAutoscaling bad deployment server data, skipping optimization: variantAutoscaling-name=%s", updateVA.Name)
			continue
		}

		vaFullName := utils.FullName(va.Name, va.Namespace)
		updateList.Items = append(updateList.Items, updateVA)
		vaMap[vaFullName] = &va
	}
	return &updateList, vaMap, allAnalyzerResponses, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VariantAutoscalingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}).
		// Watch the specific ConfigMap to trigger global reconcile
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == configMapName || obj.GetName() == saturationConfigMapName && obj.GetNamespace() == configMapNamespace {
					return []reconcile.Request{{}}
				}
				return nil
			}),
			// Predicate to filter only the target configmap
			builder.WithPredicates(ConfigMapPredicate()),
		).
		// Watch ServiceMonitor for controller's own metrics
		// This enables detection when ServiceMonitor is deleted, which would prevent
		// Prometheus from scraping controller metrics (including optimized replicas).
		Watches(
			&promoperator.ServiceMonitor{},
			handler.EnqueueRequestsFromMapFunc(r.handleServiceMonitorEvent),
			// Predicate to filter only the target ServiceMonitor
			builder.WithPredicates(ServiceMonitorPredicate()),
		).
		Named("variantAutoscaling").
		WithEventFilter(EventFilter()).
		Complete(r)
}

// handleServiceMonitorEvent handles events for the controller's own ServiceMonitor.
// When ServiceMonitor is deleted, it logs an error and emits a Kubernetes event.
// This ensures that administrators are aware when the ServiceMonitor that enables
// Prometheus scraping of controller metrics (including optimized replicas) is missing.
//
// Note: This handler does not enqueue reconcile requests. ServiceMonitor deletion doesn't
// affect the optimization logic (which reads from Prometheus), but it prevents future
// metrics from being scraped. The handler exists solely for observability - logging and
// emitting Kubernetes events to alert operators of the issue.
func (r *VariantAutoscalingReconciler) handleServiceMonitorEvent(ctx context.Context, obj client.Object) []reconcile.Request {
	serviceMonitor, ok := obj.(*promoperator.ServiceMonitor)
	if !ok {
		return nil
	}

	name := serviceMonitor.Name
	namespace := serviceMonitor.Namespace

	// Check if ServiceMonitor is being deleted
	if !serviceMonitor.GetDeletionTimestamp().IsZero() {
		logger.Log.Errorf("ServiceMonitor being deleted - Prometheus will not scrape controller metrics: servicemonitor=%s, namespace=%s, impact=%s, action=%s",
			name,
			namespace,
			"Actuator will not be able to access optimized replicas metrics",
			"ServiceMonitor must be recreated for metrics scraping to resume")

		// Emit Kubernetes event for observability
		if r.Recorder != nil {
			r.Recorder.Eventf(
				serviceMonitor,
				corev1.EventTypeWarning,
				"ServiceMonitorDeleted",
				"ServiceMonitor %s/%s is being deleted. Prometheus will not scrape controller metrics. Actuator will not be able to access optimized replicas metrics. Please recreate the ServiceMonitor.",
				namespace,
				name,
			)
		}

		// Don't trigger reconciliation - ServiceMonitor deletion doesn't affect optimization logic
		return nil
	}

	// For create/update events, no action needed
	// Don't trigger reconciliation - ServiceMonitor changes don't affect optimization logic
	return nil
}
