package pipeline

import (
	autoscalingv1 "k8s.io/api/autoscaling/v1"
)

// ScalingAction represents the type of scaling action.
type ScalingAction string

const (
	ScaleUp   ScalingAction = "ScaleUp"
	ScaleDown ScalingAction = "ScaleDown"
	NoChange  ScalingAction = "NoChange"
)

// ScalingProposal represents a proposed scaling action from any optimizer.
//
// This type is intentionally generic - it captures WHAT resources are needed,
// not implementation-specific details like saturation metrics. This allows the
// limiter to work with any optimizer (saturation-based, model-based, hybrid).
//
// Compare to interfaces.VariantDecision which is saturation-analyzer specific
// and includes fields like SaturationBased, SafetyOverride, SaturationOnly.
// ScalingProposal is the clean input boundary for the limiter.
type ScalingProposal struct {
	// Variant identification
	ModelID     string
	VariantName string
	Namespace   string

	// Resource requirements - WHAT it needs (not WHERE it can run)
	AcceleratorType string // GPU type (e.g., "H100", "A100")
	GPUsPerReplica  int    // GPUs required per replica

	// Current state
	CurrentReplicas int32

	// Proposed action from optimizer
	DesiredReplicas int32
	Action          ScalingAction

	// Algorithm hints - used by some algorithms for ordering/weighting
	// These are optional and algorithm-specific:
	SpareCapacity float64 // 0.0 = saturated, 1.0 = idle (for GreedyBySaturation)
	Priority      int     // Higher = more important (for PriorityBased)
	Weight        float64 // Relative weight (for WeightedFairShare)

	// Cost information for cost-aware algorithms
	Cost float64

	// Reference to scale target - used by NodeInventory to load scheduling constraints
	// (nodeSelector, affinity, tolerations) from the actual Deployment/StatefulSet
	ScaleTargetRef *autoscalingv1.CrossVersionObjectReference
}

// AllocationResult captures the outcome for a single proposal from an algorithm.
//
// This is an internal type used between AllocationAlgorithm and Limiter.
// It captures what the algorithm decided before the Limiter converts it
// to a ScalingDecision for external consumption.
type AllocationResult struct {
	Proposal      ScalingProposal
	GPUsAllocated int
	ReplicasAdded int
	Partial       bool   // True if less than requested was allocated
	Reason        string // Explanation for partial/zero allocation
}

// ScalingDecision represents the final scaling decision after limiting.
//
// This is the OUTPUT of the limiter - it embeds the original proposal and adds
// the constrained TargetReplicas along with metadata about what limiting occurred.
//
// The separation of ScalingProposal (input) and ScalingDecision (output) provides
// a clear contract: proposals go in, decisions come out, and the decision shows
// how the proposal was modified by resource constraints.
type ScalingDecision struct {
	ScalingProposal

	// Adjusted target (may differ from DesiredReplicas due to limits)
	TargetReplicas int32

	// Allocation details for observability
	GPUsAllocated int

	// Limiting metadata - explains what happened during limiting
	LimitedBy      string // Name of limiter that constrained this decision
	LimitReason    string // Human-readable reason for limiting
	WasConstrained bool   // True if original proposal was modified
}
