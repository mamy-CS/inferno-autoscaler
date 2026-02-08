package interfaces

import (
	"context"
	"time"
)

// Analyzer is the common interface for all scaling analyzers.
// Each analyzer observes workload metrics and produces capacity signals
// (required_capacity, spare_capacity) that the engine combines into
// scaling decisions. Analyzers do NOT build scaling plans — the engine does.
//
// Saturation Analyzer V2 is the first implementation of this interface.
// Future analyzers (throughput, SLO) will implement the same interface.
type Analyzer interface {
	// Name returns the analyzer's identifier (e.g., "saturation", "throughput", "slo").
	Name() string

	// Analyze computes capacity signals for a model across all its variants.
	// Returns per-variant capacity breakdown and model-level scaling signals.
	Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// AnalyzerInput is the common input provided to all analyzers.
type AnalyzerInput struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []ReplicaMetrics
	VariantStates  []VariantReplicaState
	Config         SaturationScalingConfig
}

// AnalyzerResult is the common output produced by all analyzers.
// The engine consumes these results to build scaling plans.
type AnalyzerResult struct {
	// AnalyzerName identifies which analyzer produced this result.
	AnalyzerName string

	ModelID    string
	Namespace  string
	AnalyzedAt time.Time

	// Per-variant capacity breakdown (in analyzer-specific units).
	VariantCapacities []VariantCapacity

	// Model-level aggregates (in analyzer-specific units).
	TotalSupply float64 // Sum of all variant TotalCapacity
	TotalDemand float64 // Sum of all variant TotalDemand
	Utilization float64 // TotalDemand / TotalSupply (0.0-1.0)

	// Scaling signals — the core output consumed by the engine.
	// These are the primary inputs for the engine's signal combination logic.
	RequiredCapacity float64 // >0 means scale-up needed (demand/threshold - supply)
	SpareCapacity    float64 // >0 means scale-down possible (supply - demand/boundary)
}

// VariantCapacity holds per-variant capacity data in analyzer-specific units.
// For saturation: units are tokens. For throughput: tokens/sec. For SLO: latency-constrained capacity.
type VariantCapacity struct {
	VariantName     string
	AcceleratorName string
	Cost            float64

	ReplicaCount    int
	PendingReplicas int

	// PerReplicaCapacity is the representative capacity per replica.
	// For saturation V2: median(effectiveCapacity) in tokens across ready replicas.
	PerReplicaCapacity float64

	// TotalCapacity is ReplicaCount × PerReplicaCapacity.
	TotalCapacity float64

	// TotalDemand is the aggregate demand on this variant.
	TotalDemand float64

	// Utilization is TotalDemand / TotalCapacity (0.0-1.0).
	Utilization float64
}
