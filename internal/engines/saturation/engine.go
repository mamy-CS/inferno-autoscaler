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
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d-incubation/workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/prometheus"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/executor"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

const (
	configMapName = "workload-variant-autoscaler-variantautoscaling-config"
	// Environment variable to enable experimental hybrid-based optimization
	// When "off" or unset, runs saturation analyzer only (default, reactive mode)

	saturationConfigMapName = "saturation-scaling-config"
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

type Engine struct {
	client   client.Client
	scheme   *runtime.Scheme
	executor executor.Executor

	Recorder         record.EventRecorder
	MetricsCollector interfaces.MetricsCollector

	// Saturation scaling config cache (thread-safe, updated on ConfigMap changes)
	saturationConfigCache      map[string]interfaces.SaturationScalingConfig
	saturationConfigCacheMutex sync.RWMutex
	saturationConfigLoaded     bool // Track if initial load succeeded
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

	interval, err := e.readOptimizationConfig(ctx)
	if err != nil {
		logger.Log.Errorf("Unable to read optimization config: %v", err)
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

	//TODO simplify Saturation loading configmap
	if err := e.InitializeSaturationConfigCache(context.Background()); err != nil {
		logger.Log.Warn("Failed to load initial saturation scaling config, will use defaults", err)
	} else {
		logger.Log.Info("saturation scaling configuration loaded successfully")
	}

	if strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true") {
		logger.Log.Info("Scaling to zero is enabled!")
	}

	activeVAs, err := utils.ActiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		logger.Log.Errorf("unable to get active variant autoscalings: %v", err)
		return err
	}

	if len(activeVAs) == 0 {
		logger.Log.Infof("No active VariantAutoscalings found, skipping optimization")
		return nil
	}

	// Get saturation scaling configuration (atomic check-and-get prevents race condition)
	saturationConfigMap, configLoaded := e.getSaturationConfigSafe()
	if !configLoaded {
		logger.Log.Warnf("Saturation scaling config not loaded yet, using defaults")
	}

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	logger.Log.Infof("Grouped VAs by model: modelCount=%d, totalVAs=%d", len(modelGroups), len(activeVAs))

	// Process each model independently
	allDecisions := make([]interfaces.VariantDecision, 0)
	// Track error count for final reconciliation summary
	errorCount := 0
	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Copy slice elements to local variable to ensure stable pointers
	// Use simple name as key since decision.VariantName is just the name (not full name with namespace)
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		va := activeVAs[i] // Copy to local variable to ensure stable pointer
		vaMap[va.Name] = &va
	}

	for modelID, modelVAs := range modelGroups {
		logger.Log.Infof("Processing model: modelID=%s, variantCount=%d", modelID, len(modelVAs))

		// Collect metrics and populate CurrentAlloc for saturation-only mode
		// This validates metrics availability and populates the VariantAutoscalings with CurrentAlloc
		if err := e.CollectMetricsForSaturationMode(ctx, modelVAs, vaMap, e.client, e.MetricsCollector); err != nil {
			logger.Log.Errorf("Failed to collect metrics for saturation mode: modelID=%s, error=%v", modelID, err)
			// Metrics collection error - individual VAs are skipped
		}

		// Get saturation config for this model (with fallback to default)
		saturationConfig := interfaces.DefaultSaturationScalingConfig()
		if len(modelVAs) > 0 {
			modelConfig := e.getSaturationScalingConfigForVariant(saturationConfigMap, modelID, modelVAs[0].Namespace)
			saturationConfig.Merge(modelConfig)
		}

		saturationTargets, saturationAnalysis, variantStates, err := e.RunSaturationAnalysis(ctx, modelID, modelVAs, saturationConfig, e.client, e.MetricsCollector)
		if err != nil {
			logger.Log.Errorf("saturation analysis failed for modelID=%s: %v", modelID, err)
			errorCount++
			// Activate safety net to ensure HPA doesn't scale to zero on partial failure
			e.emitSafetyNetMetrics(ctx, modelVAs)
			continue
		}

		var finalDecisions []interfaces.VariantDecision
		if saturationAnalysis != nil {
			finalDecisions = e.convertSaturationTargetsToDecisions(saturationTargets, saturationAnalysis, variantStates)
			logger.Log.Infof("saturation-only decisions made for model: %s - decision count: %d",
				modelID,
				len(finalDecisions))
			allDecisions = append(allDecisions, finalDecisions...)
		} else {
			// If saturationAnalysis is nil (e.g. no metrics), we just skip this model
			logger.Log.Debugf("Skipping decision application for model %s: saturation analysis is nil (likely no metrics)", modelID)
		}
	}

	// STEP 3: Apply all decisions
	if len(allDecisions) > 0 {
		logger.Log.Infof("Applying scaling decisions: totalDecisions=%d", len(allDecisions))
		if err := e.applySaturationDecisions(ctx, allDecisions, vaMap); err != nil {
			logger.Log.Errorf("Failed to apply Saturation decisions: %v", err)
			return err
		}
	} else {
		logger.Log.Info("No scaling decisions to apply")
	}

	if errorCount > 0 {
		logger.Log.Warnf("Optimization completed with errors: mode=%s, modelsProcessed=%d, modelsFailed=%d, decisionsApplied=%d",
			"saturation-only",
			len(modelGroups),
			errorCount,
			len(allDecisions))
	} else {
		logger.Log.Infof("Optimization completed successfully: mode=%s, modelsProcessed=%d, decisionsApplied=%d",
			"saturation-only",
			len(modelGroups),
			len(allDecisions))
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
func (e *Engine) convertSaturationTargetsToDecisions(
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
	variantStates, err := e.BuildVariantStates(ctx, modelVAs, k8sClient)
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
func (e *Engine) CollectMetricsForSaturationMode(
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
		currentAllocation, err := utils.BuildAllocationFromMetrics(metrics, &updateVA, deploy, cost)
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
// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []interfaces.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) error {
	// Create a map of decisions for O(1) lookup
	decisionMap := make(map[string]interfaces.VariantDecision)
	for _, d := range decisions {
		decisionMap[d.VariantName] = d
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision {
			logger.Log.Infof("Processing decision for VA: variant=%s, action=%s, current=%d→target=%d",
				vaName, decision.Action, decision.CurrentReplicas, decision.TargetReplicas)
		} else {
			logger.Log.Debugf("No scaling decision for VA, but updating status to trigger reconcile: variant=%s", vaName)
		}

		// Fetch latest version from API server to avoid conflicts
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, e.client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Log.Errorf("failed to get latest VA from API server: name=%s, error=%v", va.Name, err)
			continue
		}

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// valid check: we only update if we have a valid current alloc from the analysis phase
		if va.Status.CurrentAlloc.Accelerator != "" {
			updateVa.Status.CurrentAlloc = va.Status.CurrentAlloc
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
			logger.Log.Warnf("Skipping status update for VA without accelerator info: variant=%s", vaName)
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
			logger.Log.Errorf("failed to emit metrics for external autoscalers: variant=%s, error=%v", updateVa.Name, err)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				logger.Log.Infof("Successfully emitted metrics: variant=%s, target=%d, accelerator=%s",
					updateVa.Name, targetReplicas, acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Update VA status in API
		if err := utils.UpdateStatusWithBackoff(ctx, e.client, &updateVa, utils.StandardBackoff, "VariantAutoscaling"); err != nil {
			logger.Log.Errorf("failed to update VA status after retries: name=%s, error=%v", updateVa.Name, err)
			continue
		}

		if hasDecision {
			logger.Log.Infof("Applied Saturation decision: variant=%s, action=%s, target=%d, reason=%s",
				vaName, decision.Action, targetReplicas, reason)

			// Invalidate cache when scaling occurs
			if decision.Action != interfaces.ActionNoChange {
				if promCollector, ok := e.MetricsCollector.(*prometheus.PrometheusCollector); ok {
					promCollector.InvalidateCacheForVariant(decision.ModelID, decision.Namespace, decision.VariantName)
					logger.Log.Debugf("Invalidated metrics cache after scaling: variant=%s", decision.VariantName)
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
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Get latest version from API server
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, e.client, va.Name, va.Namespace, &updateVa); err != nil {
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

// getsaturationConfigFromCache retrieves cached config (thread-safe read).
func (e *Engine) getsaturationConfigFromCache() map[string]interfaces.SaturationScalingConfig {
	e.saturationConfigCacheMutex.RLock()
	defer e.saturationConfigCacheMutex.RUnlock()

	configCopy := make(map[string]interfaces.SaturationScalingConfig, len(e.saturationConfigCache))
	for k, v := range e.saturationConfigCache {
		configCopy[k] = v
	}
	return configCopy
}

// getSaturationConfigSafe atomically retrieves cached config and loaded status (thread-safe).
func (e *Engine) getSaturationConfigSafe() (map[string]interfaces.SaturationScalingConfig, bool) {
	e.saturationConfigCacheMutex.RLock()
	defer e.saturationConfigCacheMutex.RUnlock()

	configCopy := make(map[string]interfaces.SaturationScalingConfig, len(e.saturationConfigCache))
	for k, v := range e.saturationConfigCache {
		configCopy[k] = v
	}
	return configCopy, e.saturationConfigLoaded
}

// updateSaturationConfigCache updates the cache (thread-safe write).
func (e *Engine) updateSaturationConfigCache(ctx context.Context) error {
	configs, err := e.readSaturationScalingConfig(ctx, saturationConfigMapName, configMapNamespace)
	if err != nil {
		return err
	}

	e.saturationConfigCacheMutex.Lock()
	defer e.saturationConfigCacheMutex.Unlock()

	e.saturationConfigCache = configs
	e.saturationConfigLoaded = true

	logger.Log.Infof("saturation scaling config cache updated: entries=%d, has_default=%t",
		len(configs),
		configs["default"] != (interfaces.SaturationScalingConfig{}))

	return nil
}

// InitializeSaturationConfigCache performs initial load of saturation scaling config cache.
func (e *Engine) InitializeSaturationConfigCache(ctx context.Context) error {
	return e.updateSaturationConfigCache(ctx)
}

// readSaturationScalingConfig reads saturation scaling configuration from ConfigMap.
func (e *Engine) readSaturationScalingConfig(ctx context.Context, cmName, cmNamespace string) (map[string]interfaces.SaturationScalingConfig, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, e.client, cmName, cmNamespace, &cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Log.Warnf("saturation scaling ConfigMap not found, using hardcoded defaults: configmap=%s, namespace=%s",
				cmName, cmNamespace)
			// Return default config only
			return map[string]interfaces.SaturationScalingConfig{
				"default": interfaces.DefaultSaturationScalingConfig(),
			}, nil
		}
		return nil, fmt.Errorf("failed to read ConfigMap %s/%s: %w", cmNamespace, cmName, err)
	}

	configs := make(map[string]interfaces.SaturationScalingConfig)

	// Parse all entries
	for key, yamlStr := range cm.Data {
		var config interfaces.SaturationScalingConfig
		if err := yaml.Unmarshal([]byte(yamlStr), &config); err != nil {
			logger.Log.Warnf("Failed to parse saturation scaling config entry, skipping: key=%s, error=%v",
				key, err)
			continue
		}

		// Validate configuration
		if err := config.Validate(); err != nil {
			logger.Log.Warnf("Invalid saturation scaling config entry, skipping: key=%s, error=%v",
				key, err)
			continue
		}

		configs[key] = config
	}

	// Ensure default exists
	if _, ok := configs["default"]; !ok {
		logger.Log.Warn("No 'default' entry in saturation scaling ConfigMap, using hardcoded defaults")
		configs["default"] = interfaces.DefaultSaturationScalingConfig()
	}

	return configs, nil
}

// getSaturationScalingConfigForVariant retrieves config for specific model/namespace with fallback to default.
func (e *Engine) getSaturationScalingConfigForVariant(
	configs map[string]interfaces.SaturationScalingConfig,
	modelID, namespace string,
) interfaces.SaturationScalingConfig {
	// Start with default
	config := configs["default"]

	// Search for matching override
	for key, override := range configs {
		if key == "default" {
			continue
		}

		// Check if this override matches our model_id and namespace
		if override.ModelID == modelID && override.Namespace == namespace {
			config.Merge(override)
			logger.Log.Debugf("Applied saturation scaling override: key=%s, modelID=%s, namespace=%s, config=%v",
				key, modelID, namespace, config)
			break
		}
	}

	return config
}

func (e *Engine) readOptimizationConfig(ctx context.Context) (interval string, err error) {
	cm := corev1.ConfigMap{}
	err = utils.GetConfigMapWithBackoff(ctx, e.client, configMapName, configMapNamespace, &cm)

	if err != nil {
		return "", fmt.Errorf("failed to get optimization configmap after retries: %w", err)
	}

	interval = cm.Data["GLOBAL_OPT_INTERVAL"]
	return interval, nil
}
