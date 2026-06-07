package saturation

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// runV2AnalysisOnly runs the V2 saturation analyzer and returns the raw AnalyzerResult
// without building targets or converting to V1 types. The optimizer will handle
// target building across all models.
func (e *Engine) runV2AnalysisOnly(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// 1. Pre-populate capacity store with scale target-derived params
	for _, va := range variantAutoscalings {
		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTarget := scaleTargets[key]
		if scaleTarget == nil {
			logger.V(logging.DEBUG).Info("No scale target found for VA, skipping capacity store pre-population",
				"variant", va.Name, "scaleTargetKey", key)
			continue
		}
		// Get accelerator name from scale target nodeSelector/nodeAffinity or VA label
		accelerator := utils.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
		gpuCount := scaleTarget.GetTotalGPUsPerReplica()
		e.capacityStore.LoadFromScaleTarget(namespace, modelID, va.Name, accelerator, gpuCount, scaleTarget)
		logger.V(logging.DEBUG).Info("Pre-populated capacity store from scale target",
			"variant", va.Name, "accelerator", accelerator, "gpuCount", gpuCount)
	}

	// 2. Build AnalyzerInput
	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		// TODO: populate SchedulerQueue when flow control metrics are collected
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	logger.Info("V2 saturation analysis completed",
		"modelID", modelID,
		"totalSupply", result.TotalSupply,
		"totalDemand", result.TotalDemand,
		"utilization", result.Utilization,
		"requiredCapacity", result.RequiredCapacity,
		"spareCapacity", result.SpareCapacity)

	return result, nil
}

// runAnalyzersAndScore runs the V2 saturation analyzer, invokes every other
// registered analyzer once per cycle to exercise the multi-analyzer pipeline,
// and computes the weighted composite score from saturation's signal and the
// model's priority.
//
// On this branch only saturation drives scaling decisions: non-saturation
// analyzer results are intentionally discarded. Combine and per-analyzer
// threshold consumption land in follow-up PRs (multi-analyzer-optimizer,
// multi-analyzer-threshold).
func (e *Engine) runAnalyzersAndScore(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Resolve per-analyzer threshold overrides before running the analyzer.
	// The saturation analyzer reads thresholds from the config, so we apply
	// per-analyzer overrides to the config's top-level fields. Only saturation
	// overrides are honored today; per-analyzer overrides for other analyzers
	// are tracked on the multi-analyzer-threshold branch.
	for _, aw := range config.Analyzers {
		if aw.Name == interfaces.SaturationAnalyzerName && (aw.Enabled == nil || *aw.Enabled) {
			if aw.ScaleUpThreshold != nil {
				config.ScaleUpThreshold = *aw.ScaleUpThreshold
			}
			if aw.ScaleDownBoundary != nil {
				config.ScaleDownBoundary = *aw.ScaleDownBoundary
			}
			break
		}
	}

	// Run saturation analyzer (always needed for PerReplicaCapacity).
	baseResult, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings)
	if err != nil {
		return nil, err
	}

	// Build AnalyzerInput once; shared by all non-saturation analyzers.
	// Note: &config has had saturation's per-entry threshold overrides applied
	// (the loop above). Non-saturation analyzers therefore receive the
	// saturation-adjusted config rather than the original. This is harmless
	// on this branch (their results are discarded), and the clean fix —
	// engine applies thresholds universally after each analyzer runs —
	// is tracked on multi-analyzer-threshold (PR #1228).
	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		// SchedulerQueue: nil — wired in a later PR
	}

	// Iterate every registered analyzer in registration order. Saturation has
	// already run above (with full args); the loop calls Analyze on every
	// other registered analyzer to exercise the multi-analyzer pipeline.
	// Their results are intentionally discarded on this branch — combine /
	// per-analyzer-result consumption lands in follow-up PRs.
	e.runRegisteredAnalyzers(ctx, logger, modelID, input)

	// Compute weighted score from enabled analyzers (saturation only on this
	// branch — non-saturation analyzer results are not consumed yet).
	totalWeighted := 0.0
	for _, aw := range config.Analyzers {
		if aw.Enabled != nil && !*aw.Enabled {
			continue
		}
		if aw.Name == interfaces.SaturationAnalyzerName {
			totalWeighted += baseResult.RequiredCapacity * aw.Score
			// future: add "throughput", "slo" cases
		}
	}

	// Score = priority * weighted sum
	baseResult.Score = config.Priority * totalWeighted
	return baseResult, nil
}

// runRegisteredAnalyzers invokes Analyze on every registered non-saturation
// analyzer in registration order, reading from the frozen analyzersSnapshot
// built by StartOptimizeLoop. The saturation entry is skipped because the
// engine runs saturation separately via runV2AnalysisOnly with full args.
// Each call is isolated: errors are logged and discarded, and panics are
// recovered so a faulty analyzer cannot take down the optimize goroutine.
// Results are intentionally discarded on this branch — combine logic lands
// in follow-up PRs.
func (e *Engine) runRegisteredAnalyzers(
	ctx context.Context,
	logger logr.Logger,
	modelID string,
	input interfaces.AnalyzerInput,
) {
	for _, entry := range e.analyzersSnapshot {
		if entry.name == interfaces.SaturationAnalyzerName {
			continue
		}
		runRegisteredAnalyzer(ctx, logger, entry, modelID, input)
	}
}

// runRegisteredAnalyzer invokes a single non-saturation analyzer's Analyze
// method, isolating the call from the rest of the cycle. Errors are logged
// and discarded; panics are recovered and logged. The result is intentionally
// discarded on this branch — combine logic lands in follow-up PRs.
func runRegisteredAnalyzer(
	ctx context.Context,
	logger logr.Logger,
	entry analyzerEntry,
	modelID string,
	input interfaces.AnalyzerInput,
) {
	defer func() {
		if r := recover(); r != nil {
			// Plugin failure is non-fatal; log at debug to avoid spamming
			// operator logs every optimize cycle.
			logger.V(logging.DEBUG).Info("registered analyzer panicked; result discarded",
				"name", entry.name, "modelID", modelID, "panic", fmt.Sprintf("%v", r))
		}
	}()
	if _, err := entry.analyzer.Analyze(ctx, input); err != nil {
		// Plugin failure is non-fatal; log at debug to avoid spamming
		// operator logs every optimize cycle.
		logger.V(logging.DEBUG).Info("registered analyzer failed; result discarded",
			"name", entry.name, "modelID", modelID, "error", err)
	}
}

// computeCurrentGPUUsage iterates over model scaling requests to compute the
// current GPU usage per accelerator type. Used to provide current usage to
// the ConstraintProvider when building GPU constraints for the optimizer.
func computeCurrentGPUUsage(requests []pipeline.ModelScalingRequest) map[string]int {
	usage := make(map[string]int)
	for _, req := range requests {
		if req.Result == nil {
			continue
		}
		stateMap := make(map[string]interfaces.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range req.Result.VariantCapacities {
			state := stateMap[vc.VariantName]
			gpusPerReplica := state.GPUsPerReplica
			if gpusPerReplica <= 0 {
				gpusPerReplica = 1
			}
			usage[vc.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
		}
	}
	return usage
}

// collectV2ModelRequest performs V2 analysis for a single model and returns
// a ModelScalingRequest for the optimizer, or nil if analysis should be skipped.
func (e *Engine) collectV2ModelRequest(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config config.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*pipeline.ModelScalingRequest, error) {
	result, err := e.runAnalyzersAndScore(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, scaleTargets, variantAutoscalings)
	if err != nil {
		return nil, fmt.Errorf("collecting V2 model request for %s/%s: %w", namespace, modelID, err)
	}

	// Detect P/D disaggregation: true when any variant has role != interfaces.RoleBoth
	disaggregated := false
	for _, vs := range variantStates {
		if vs.Role != "" && vs.Role != interfaces.RoleBoth {
			disaggregated = true
			break
		}
	}

	return &pipeline.ModelScalingRequest{
		ModelID:       modelID,
		Namespace:     namespace,
		Result:        result,
		VariantStates: variantStates,
		Priority:      config.Priority,
		Disaggregated: disaggregated,
	}, nil
}
