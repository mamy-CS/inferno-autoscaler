/*
Copyright 2025 The llm-d Authors

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

package saturation

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d-incubation/workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

const (
	defaultConfigMapName = "workload-variant-autoscaler-variantautoscaling-config"
	// Environment variable to enable experimental hybrid-based optimization
	// When "off" or unset, runs saturation analyzer only (default, reactive mode)

	defaultSaturationConfigMapName = "saturation-scaling-config"
)

var (
	configMapNamespace = getNamespace()
)

func getNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "workload-variant-autoscaler-system"
}

func getConfigMapName() string {
	if name := os.Getenv("CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return defaultConfigMapName
}

func getSaturationConfigMapName() string {
	if name := os.Getenv("SATURATION_CONFIG_MAP_NAME"); name != "" {
		return name
	}
	return defaultSaturationConfigMapName
}

type Engine struct {
	client   client.Client
	scheme   *runtime.Scheme
	executor executor.Executor

	Recorder         record.EventRecorder
	MetricsCollector interfaces.MetricsCollector

	// Saturation scaling config cache (thread-safe, updated on ConfigMap changes)
}

// NewEngine creates a new instance of the saturation engine.
func NewEngine(client client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, collector interfaces.MetricsCollector) *Engine {
	engine := Engine{
		client:           client,
		scheme:           scheme,
		Recorder:         recorder,
		MetricsCollector: collector,
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     30 * time.Second,
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine
}

// StartOptimizeLoop starts the optimization loop for the saturation engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	//TODO: move interval to manager.yaml
	logger := ctrl.LoggerFrom(ctx)

	interval, err := e.readOptimizationConfig(ctx)
	if err != nil {
		logger.Error(err, "Unable to read optimization config")
		return err
	}

	// Update the executor interval if changed
	// Note: simple polling executor might not support dynamic interval update easily without restart,
	// but here we just check it. The original code used RequeueAfter.
	// The PollingExecutor uses fixed interval.
	// TODO: Support dynamic interval in Executor if needed. For now, we log and proceed.
	if interval != "" {
		if dur, err := time.ParseDuration(interval); err == nil {
			// e.executor.SetInterval(dur) // If supported
			_ = dur
		}
	}

	if strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true") {
		logger.Info("Scaling to zero is enabled")
	}

	activeVAs, err := utils.ActiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		logger.Info("No active VariantAutoscalings found, skipping optimization")
		return nil
	}

	saturationConfigMap, err := e.readSaturationScalingConfig(ctx, getSaturationConfigMapName(), configMapNamespace)
	if err != nil {
		logger.Error(err, "Failed to read saturation scaling config, skipping optimization iteration")
		//TODO: should we retry again? readSaturationScalingConfig already has backoff with retry logic
		return nil
	}

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	logger.Info("Grouped VAs by model",
		"modelCount", len(modelGroups),
		"totalVAs", len(activeVAs))

	// Process each model independently
	allDecisions := make([]interfaces.VariantDecision, 0)
	// Track error count for final reconciliation summary
	errorCount := 0
	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Copy slice elements to local variable to ensure stable pointers
	// Use deployment name (ScaleTargetName) as key since decision.VariantName uses deployment name
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		va := activeVAs[i] // Copy to local variable to ensure stable pointer
		vaMap[va.GetScaleTargetName()] = &va
	}

	for groupKey, modelVAs := range modelGroups {
		// The groupKey is "modelID|namespace" - extract actual modelID from VAs
		// All VAs in the group have the same modelID and namespace
		modelID := modelVAs[0].Spec.ModelID
		logger.Info("Processing model",
			"modelID", modelID,
			"namespace", modelVAs[0].Namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Collect metrics and populate CurrentAlloc for saturation-only mode
		// This validates metrics availability and populates the VariantAutoscalings with CurrentAlloc
		if err := e.CollectMetricsForSaturationMode(ctx, modelVAs, vaMap, e.client, e.MetricsCollector); err != nil {
			logger.Error(err, "Failed to collect metrics for saturation mode",
				"modelID", modelID)
			// Metrics collection error - individual VAs are skipped
		}

		// Get saturation config for this model (use default)
		var saturationConfig interfaces.SaturationScalingConfig
		//TODO: if modelVAs is less than zero we continue saturation analysis and fail??
		if len(modelVAs) > 0 {
			saturationConfig = saturationConfigMap["default"]
		}
		saturationTargets, saturationAnalysis, variantStates, err := e.RunSaturationAnalysis(ctx, modelID, modelVAs, saturationConfig, e.client, e.MetricsCollector)
		if err != nil {
			logger.Error(err, "Saturation analysis failed",
				"modelID", modelID)
			errorCount++
			// Activate safety net to ensure HPA doesn't scale to zero on partial failure
			e.emitSafetyNetMetrics(ctx, modelVAs)
			continue
		}

		var finalDecisions []interfaces.VariantDecision
		if saturationAnalysis != nil {
			finalDecisions = e.convertSaturationTargetsToDecisions(ctx, saturationTargets, saturationAnalysis, variantStates)
			logger.Info("Saturation-only decisions made for model",
				"modelID", modelID,
				"decisionCount", len(finalDecisions))
			allDecisions = append(allDecisions, finalDecisions...)
		} else {
			// If saturationAnalysis is nil (e.g. no metrics), we just skip this model
			logger.V(logging.DEBUG).Info("Skipping decision application for model: saturation analysis is nil (likely no metrics)",
				"modelID", modelID)
		}
	}

	// STEP 3: Apply decisions and update VA status
	// Always call applySaturationDecisions, even with empty decisions.
	// This function also updates VA.Status.CurrentAlloc with collected metrics
	// and emits HPA metrics, which must happen every reconciliation cycle.
	if len(allDecisions) > 0 {
		logger.Info("Applying scaling decisions",
			"totalDecisions", len(allDecisions))
	} else {
		logger.Info("No scaling decisions to apply, updating VA status with metrics")
	}
	if err := e.applySaturationDecisions(ctx, allDecisions, vaMap); err != nil {
		logger.Error(err, "Failed to apply saturation decisions")
		return err
	}

	if errorCount > 0 {
		logger.Info("Optimization completed with errors",
			"mode", "saturation-only",
			"modelsProcessed", len(modelGroups),
			"modelsFailed", errorCount,
			"decisionsApplied", len(allDecisions))
	} else {
		logger.Info("Optimization completed successfully",
			"mode", "saturation-only",
			"modelsProcessed", len(modelGroups),
			"decisionsApplied", len(allDecisions))
	}

	return nil
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) ([]interfaces.VariantReplicaState, error) {
	states := make([]interfaces.VariantReplicaState, 0, len(vas))
	logger := ctrl.LoggerFrom(ctx)

	for _, va := range vas {
		// Get current replicas from deployment using ScaleTargetRef
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy); err != nil {
			logger.Info("Failed to get deployment for VA, using status",
				"name", va.Name,
				"deployment", va.GetScaleTargetName(),
				"error", err)
			// Fallback to status if deployment fetch fails
			states = append(states, interfaces.VariantReplicaState{
				VariantName:     va.GetScaleTargetName(),
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
			VariantName:     deploy.Name,
			CurrentReplicas: currentReplicas,
			DesiredReplicas: va.Status.DesiredOptimizedAlloc.NumReplicas,
		})
	}

	return states, nil
}

// convertSaturationTargetsToDecisions converts saturation-only targets to VariantDecisions.
// Used when model-based optimizer is disabled (saturation-only mode).
func (e *Engine) convertSaturationTargetsToDecisions(
	ctx context.Context,
	saturationTargets map[string]int,
	saturationAnalysis *interfaces.ModelSaturationAnalysis,
	variantStates []interfaces.VariantReplicaState,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)
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
			logger.Info("No variant analysis found for decision (metrics may be unavailable)",
				"variant", variantName)
		}

		decisions = append(decisions, decision)
	}

	return decisions
}

// RunSaturationAnalysis performs saturation analysis for a model and returns Saturation targets.
func (e *Engine) RunSaturationAnalysis(
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

	logger := ctrl.LoggerFrom(ctx)
	namespace := modelVAs[0].Namespace // All VAs of same model are in same namespace

	// Build variant costs map, deployments map, and VAs map for metrics collection
	variantCosts := make(map[string]float64)
	deployments := make(map[string]*appsv1.Deployment)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]

		// Get the deployment for this VA using ScaleTargetRef
		var deploy appsv1.Deployment
		err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get deployment for VA",
				"variant", va.Name,
				"deployment", va.GetScaleTargetName(),
				"error", err)
			continue
		}

		// Parse variant cost
		cost := saturation.DefaultVariantCost // default
		if va.Spec.VariantCost != "" {
			if parsedCost, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
				cost = parsedCost
			}
		}

		// Use deployment name as key (not VA name) since getExistingPods uses
		// the key to build pod name regex filters for Prometheus queries
		deployments[deploy.Name] = &deploy
		variantAutoscalings[deploy.Name] = va
		variantCosts[deploy.Name] = cost
	}

	// Collect Saturation metrics using the configured collector
	replicaMetrics, err := metricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.V(logging.DEBUG).Info("Collected saturation metrics",
		"modelID", modelID,
		"namespace", namespace,
		"metricsCount", len(replicaMetrics))

	// If no metrics available, skip saturation analysis entirely
	// This prevents creating invalid decisions when pods are not ready or metrics are unavailable
	if len(replicaMetrics) == 0 {
		logger.Info("No saturation metrics available for model, skipping analysis",
			"modelID", modelID,
			"namespace", namespace)
		return nil, nil, nil, nil // Return nil to signal skip due to metrics unavailable, not error
	}

	// Analyze saturation across all variants
	saturationAnalyzer := saturation.NewAnalyzer()
	saturationAnalysis, err := saturationAnalyzer.AnalyzeModelSaturation(ctx, modelID, namespace, replicaMetrics, SaturationConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to analyze Saturation for model %s: %w", modelID, err)
	}

	logger.Info("Saturation analysis completed",
		"modelID", modelID,
		"totalReplicas", saturationAnalysis.TotalReplicas,
		"nonSaturated", saturationAnalysis.NonSaturatedCount,
		"shouldScaleUp", saturationAnalysis.ShouldScaleUp,
		"scaleDownSafe", saturationAnalysis.ScaleDownSafe)

	// Build variant states (current and desired replicas)
	variantStates, err := e.BuildVariantStates(ctx, modelVAs, k8sClient)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to build variant states for model %s: %w", modelID, err)
	}

	// Calculate saturation-based targets
	saturationTargets := saturationAnalyzer.CalculateSaturationTargets(ctx, saturationAnalysis, variantStates)

	logger.V(logging.DEBUG).Info("Saturation targets calculated",
		"modelID", modelID,
		"targets", saturationTargets)

	return saturationTargets, saturationAnalysis, variantStates, nil
}

// CollectMetricsForSaturationMode collects metrics and populates CurrentAlloc for VAs in saturation-only mode.
func (e *Engine) CollectMetricsForSaturationMode(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
	metricsCollector interfaces.MetricsCollector,
) error {
	logger := ctrl.LoggerFrom(ctx)

	for i := range modelVAs {
		va := &modelVAs[i]
		modelName := va.Spec.ModelID

		// Get accelerator name from VA labels - required field
		accName := va.Labels["inference.optimization/acceleratorName"]
		if accName == "" {
			logger.Info("Missing accelerator name label for VA, skipping",
				"variant", va.Name)
			continue
		}

		// Extract accelerator cost from VA.Spec.VariantCost - required field
		if va.Spec.VariantCost == "" {
			logger.Info("Missing variant cost for VA, skipping",
				"variant", va.Name)
			continue
		}
		cost, err := strconv.ParseFloat(va.Spec.VariantCost, 64)
		if err != nil {
			logger.Info("Invalid variant cost for VA, skipping",
				"variant", va.Name,
				"cost", va.Spec.VariantCost,
				"error", err)
			continue
		}

		// Get Deployment using ScaleTargetRef
		var deploy appsv1.Deployment
		err = utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get deployment for VA, skipping",
				"variant", va.Name,
				"deployment", va.GetScaleTargetName(),
				"error", err)
			continue // Skip VAs without deployments
		}

		// Fetch latest VA from API server (use VA name, not deployment name - they are now decoupled)
		var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		err = utils.GetVariantAutoscalingWithBackoff(ctx, k8sClient, va.Name, va.Namespace, &updateVA)
		if err != nil {
			logger.V(logging.DEBUG).Info("Unable to get VA",
				"variant", va.Name,
				"error", err)
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

			logger.Info("Metrics unavailable for VA, skipping",
				"variant", updateVA.Name,
				"reason", metricsValidation.Reason,
				"troubleshooting", metricsValidation.Message)
			continue
		}

		// Collect raw metrics from collector
		metrics, err := metricsCollector.AddMetricsToOptStatus(ctx, &updateVA, deploy, cost)
		if err != nil {
			logger.V(logging.DEBUG).Info("Unable to fetch metrics for VA",
				"variant", updateVA.Name,
				"error", err)
			continue
		}

		// Assemble Allocation struct from raw metrics
		currentAllocation, err := utils.BuildAllocationFromMetrics(metrics, &updateVA, deploy, cost)
		if err != nil {
			logger.V(logging.DEBUG).Info("Unable to build allocation for VA",
				"variant", updateVA.Name,
				"error", err)
			continue
		}

		// Update the VA in vaMap with populated CurrentAlloc
		updateVA.Status.CurrentAlloc = currentAllocation

		// Update vaMap with the VA that has CurrentAlloc populated
		// Use deployment name as key to match the initial vaMap population
		vaMap[updateVA.GetScaleTargetName()] = &updateVA

		logger.Info("Metrics collected for VA",
			"variant", updateVA.Name,
			"replicas", currentAllocation.NumReplicas,
			"accelerator", currentAllocation.Accelerator,
			"ttft", currentAllocation.TTFTAverage,
			"itl", currentAllocation.ITLAverage,
			"cost", currentAllocation.VariantCost)
	}

	return nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []interfaces.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) error {
	logger := ctrl.LoggerFrom(ctx)
	// Create a map of decisions for O(1) lookup
	decisionMap := make(map[string]interfaces.VariantDecision)
	for _, d := range decisions {
		decisionMap[d.VariantName] = d
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision {
			logger.Info("Processing decision for VA",
				"variant", vaName,
				"action", decision.Action,
				"current", decision.CurrentReplicas,
				"target", decision.TargetReplicas)
		} else {
			logger.V(logging.DEBUG).Info("No scaling decision for VA, but updating status to trigger reconcile",
				"variant", vaName)
		}

		// Fetch latest version from API server to avoid conflicts
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, e.client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Error(err, "Failed to get latest VA from API server",
				"name", va.Name)
			continue
		}

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// valid check: we only update if we have a valid current alloc from the analysis phase
		if va.Status.CurrentAlloc.Accelerator != "" {
			updateVa.Status.CurrentAlloc = va.Status.CurrentAlloc
		}

		// Copy MetricsAvailable condition from local analysis (set during metrics collection)
		// This condition was set on `va` but we fetched a fresh `updateVa` from API server
		if metricsCondition := llmdVariantAutoscalingV1alpha1.GetCondition(va, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable); metricsCondition != nil {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metricsCondition.Status,
				metricsCondition.Reason,
				metricsCondition.Message)
		}

		// Determine target replicas and accelerator
		var targetReplicas int
		var acceleratorName string
		var reason string

		if hasDecision {
			targetReplicas = decision.TargetReplicas
			acceleratorName = decision.AcceleratorName
			reason = decision.Reason
		} else {
			// No change/decision: Keep current target or default to current replicas
			// We effectively explicitly "decide" to keep things as they are if no decision was made
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = updateVa.Status.DesiredOptimizedAlloc.NumReplicas
			} else {
				targetReplicas = updateVa.Status.CurrentAlloc.NumReplicas
			}
			// Keep existing accelerator or use current
			if updateVa.Status.DesiredOptimizedAlloc.Accelerator != "" {
				acceleratorName = updateVa.Status.DesiredOptimizedAlloc.Accelerator
			} else {
				acceleratorName = updateVa.Status.CurrentAlloc.Accelerator
			}
			reason = "No scaling decision (optimization loop)"
		}

		// If we still don't have an accelerator name (e.g. new VA, no decision, no current alloc), we can't update status sensibly
		if acceleratorName == "" {
			logger.Info("Skipping status update for VA without accelerator info",
				"variant", vaName)
			continue
		}

		// Update DesiredOptimizedAlloc
		// ALWAYS update LastRunTime to trigger reconciliation in the controller
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: targetReplicas,
			Accelerator: acceleratorName,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false // Reset applied status until Actuator handles it (if needed)

		// Set condition based on decision characteristics (or lack thereof)
		if hasDecision {
			if decision.SafetyOverride {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationSafetyOverride",
					fmt.Sprintf("saturation safety override: %s", reason))
			} else if decision.SaturationOnly {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationOnlyMode",
					fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", reason, targetReplicas))
			} else {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
					fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", reason, targetReplicas))
			}
		} else {
			// No active decision (just refreshing)
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				"Optimization loop ran (no scaling change needed)")
		}

		// Emit metrics for external autoscalers (Important: Actuator emits these)
		// We should emit metrics even if no decision changed, to keep HPA alive
		act := actuator.NewActuator(e.client)
		/*
		   NOTE: emitSafetyNetMetrics handles cases where optimization FAILS.
		   Here we are in the success path (optimization ran, even if no change).
		   We should ensure metrics are emitted for the External Scaler.
		*/

		// Ensure we have a valid SAT/Model decision "SaturationOnly" flag for metric emission context if needed
		// For now we assume if no decision, it's not saturation-only forced override, just normal op.
		// isSaturationOnly := false
		// if hasDecision {
		// 	isSaturationOnly = decision.SaturationOnly
		// }

		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			logger.Error(err, "Failed to emit metrics for external autoscalers",
				"variant", updateVa.Name)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				logger.Info("Successfully emitted metrics",
					"variant", updateVa.Name,
					"target", targetReplicas,
					"accelerator", acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Update VA status in API
		if err := utils.UpdateStatusWithBackoff(ctx, e.client, &updateVa, utils.StandardBackoff, "VariantAutoscaling"); err != nil {
			logger.Error(err, "Failed to update VA status after retries",
				"name", updateVa.Name)
			continue
		}

		if hasDecision {
			logger.Info("Applied saturation decision",
				"variant", vaName,
				"action", decision.Action,
				"target", targetReplicas,
				"reason", reason)

			// Invalidate cache when scaling occurs
			if decision.Action != interfaces.ActionNoChange {
				if promCollector, ok := e.MetricsCollector.(*prometheus.PrometheusCollector); ok {
					promCollector.InvalidateCacheForVariant(decision.ModelID, decision.Namespace, decision.VariantName)
					logger.V(logging.DEBUG).Info("Invalidated metrics cache after scaling",
						"variant", decision.VariantName)
				}
			}
		}
	}

	return nil
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
func (e *Engine) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Get latest version from API server
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, e.client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Error(err, "Safety net: failed to get latest VA from API server",
				"name", va.Name)
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
				logger.Error(err, "Safety net: failed to get current replicas, using VA status",
					"variant", updateVa.Name)
				currentReplicas = int32(updateVa.Status.CurrentAlloc.NumReplicas)
			}
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Get current replicas for metric emission
		currentReplicas, err := act.GetCurrentDeploymentReplicas(ctx, &updateVa)
		if err != nil {
			logger.Error(err, "Safety net: failed to get current replicas for metrics",
				"variant", updateVa.Name)
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
			logger.Info("Safety net: skipping metric emission - no accelerator name available",
				"variant", updateVa.Name)
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
			logger.Error(err, "Safety net: failed to emit metrics",
				"variant", updateVa.Name)
			continue
		}

		logger.Info("Safety net activated: emitted fallback metrics",
			"variant", updateVa.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"accelerator", accelerator,
			"fallbackSource", fallbackSource)
	}
}

// readSaturationScalingConfig reads saturation scaling configuration from ConfigMap.
func (e *Engine) readSaturationScalingConfig(ctx context.Context, cmName, cmNamespace string) (map[string]interfaces.SaturationScalingConfig, error) {
	logger := ctrl.LoggerFrom(ctx)
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, e.client, cmName, cmNamespace, &cm)

	if err != nil {
		return nil, fmt.Errorf("failed to read ConfigMap %s/%s: %w", cmNamespace, cmName, err)
	}

	configs := make(map[string]interfaces.SaturationScalingConfig)

	// Parse all entries
	for key, yamlStr := range cm.Data {
		var config interfaces.SaturationScalingConfig
		if err := yaml.Unmarshal([]byte(yamlStr), &config); err != nil {
			logger.Error(err, "Failed to parse saturation scaling config entry, skipping",
				"key", key)
			continue
		}

		// Validate configuration
		if err := config.Validate(); err != nil {
			logger.Error(err, "Invalid saturation scaling config entry, skipping",
				"key", key)
			continue
		}

		configs[key] = config
	}

	return configs, nil
}

func (e *Engine) readOptimizationConfig(ctx context.Context) (interval string, err error) {
	cm := corev1.ConfigMap{}
	err = utils.GetConfigMapWithBackoff(ctx, e.client, getConfigMapName(), configMapNamespace, &cm)

	if err != nil {
		return "", fmt.Errorf("failed to get optimization configmap after retries: %w", err)
	}

	interval = cm.Data["GLOBAL_OPT_INTERVAL"]
	return interval, nil
}
