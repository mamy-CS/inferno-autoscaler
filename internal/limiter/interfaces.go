// Package limiter provides interfaces for resource limiting algorithms.
//
// # Design Rationale
//
// The limiter package separates two orthogonal concerns:
//
//  1. Inventory (Granularity): How resources are tracked and what constraints apply.
//     Examples: cluster-wide pool, per-accelerator-type limits, node-level with scheduling.
//
//  2. AllocationAlgorithm (Strategy): How resources are distributed across proposals.
//     Examples: greedy by saturation, round-robin, priority-based, weighted fair share.
//
// This separation enables:
//   - Algorithms work with ANY inventory - they don't care if resources are tracked
//     at cluster, type, or node level
//   - Inventories work with ANY algorithm - the distribution strategy is independent
//   - Easy testing: algorithms can be tested with mock allocators
//   - Runtime flexibility: swap algorithms via configuration without code changes
//
// # Interface Hierarchy
//
//	Limiter (public API)
//	   │
//	   ├── Inventory (resource granularity)
//	   │      └── creates ResourceAllocator
//	   │
//	   └── AllocationAlgorithm (distribution strategy)
//	          └── uses ResourceAllocator
//
// Users typically only interact with Limiter. The internal interfaces (Inventory,
// AllocationAlgorithm, ResourceAllocator) are exposed for extensibility - custom
// implementations can be plugged in without modifying the package.
//
// # Relationship to Other Types
//
// This package defines ScalingProposal (input) and ScalingDecision (output) rather
// than reusing VariantDecision from the interfaces package because:
//   - VariantDecision is saturation-analyzer specific (SaturationBased, SafetyOverride fields)
//   - ScalingProposal is a generic optimizer output with clear input/output boundaries
//   - This decoupling allows the limiter to work with any optimizer, not just saturation
//
// The Inventory interface here is separate from collector's inventory because:
//   - Collector inventory: knows how to collect metrics, track staleness, emit events
//   - Limiter inventory: knows resource availability for allocation decisions
//   - Different responsibilities, different lifecycles
package limiter

import (
	"context"
)

// Limiter constrains scaling decisions based on resource availability.
//
// This is the primary interface users interact with. It combines an Inventory
// (resource granularity) with an AllocationAlgorithm (distribution strategy)
// to produce constrained scaling decisions.
//
// Example usage:
//
//	limiter := NewLimiter("prod", nodeInventory, greedyAlgorithm)
//	decisions, err := limiter.Limit(ctx, proposals)
type Limiter interface {
	// Name returns limiter identifier for logging/metrics.
	Name() string

	// Limit applies resource constraints to proposed scaling decisions.
	// Returns decisions that may have adjusted TargetReplicas based on availability.
	Limit(ctx context.Context, proposals []ScalingProposal) ([]ScalingDecision, error)
}

// AllocationAlgorithm defines how to distribute limited resources across proposals.
//
// Algorithms are independent of resource granularity - they work with any Inventory
// through the ResourceAllocator abstraction. This enables mixing any algorithm
// with any inventory type.
//
// Built-in algorithms include:
//   - GreedyBySaturation: allocates to most saturated (lowest spare capacity) first
//   - RoundRobin: distributes evenly, one replica at a time
//   - PriorityBased: allocates to highest priority first
//   - WeightedFairShare: allocates proportionally based on weights
//   - BinPacking: consolidates on fewer nodes (requires NodeAwareAllocator)
type AllocationAlgorithm interface {
	// Name returns algorithm identifier for logging/metrics.
	Name() string

	// Allocate distributes available resources across proposals.
	//
	// The allocator parameter abstracts resource reservation - the algorithm
	// doesn't need to know if resources are cluster-wide, per-type, or per-node.
	Allocate(
		ctx context.Context,
		proposals []ScalingProposal,
		allocator ResourceAllocator,
	) ([]AllocationResult, error)
}

// ResourceAllocator abstracts resource reservation at different granularities.
//
// Created by Inventory to handle granularity-specific allocation logic.
// Algorithms use this interface without knowing the underlying inventory type.
//
// For example:
//   - ClusterInventory creates an allocator that tracks total GPUs
//   - TypeInventory creates an allocator that tracks GPUs per accelerator type
//   - NodeInventory creates an allocator that tracks GPUs per node with scheduling checks
type ResourceAllocator interface {
	// TryAllocate attempts to allocate GPUs for a proposal.
	// Returns actual GPUs allocated (may be less than requested if constrained).
	//
	// The proposal parameter provides context (AcceleratorType, GPUsPerReplica)
	// that some allocators need for type-aware or node-aware allocation.
	TryAllocate(proposal ScalingProposal, gpusRequested int) (gpusAllocated int, err error)

	// Remaining returns total remaining allocatable GPUs across all resources.
	Remaining() int
}

// Inventory provides resource availability and creates allocators.
//
// Implementations define the granularity of resource tracking:
//   - ClusterInventory: single pool of all GPUs
//   - TypeInventory: separate pools per accelerator type (H100, A100, etc.)
//   - NodeInventory: per-node tracking with scheduling constraint awareness
//
// The Inventory is responsible for:
//   - Refreshing availability data from the cluster
//   - Creating ResourceAllocator instances that handle granularity-specific logic
//
// Note: This is separate from collector.Inventory which handles metrics collection.
// The limiter Inventory focuses solely on resource availability for allocation.
type Inventory interface {
	// Name returns inventory identifier for logging/metrics.
	Name() string

	// Refresh updates inventory from the cluster.
	// Should be called before CreateAllocator to ensure fresh data.
	Refresh(ctx context.Context) error

	// CreateAllocator returns a ResourceAllocator for this inventory.
	// The allocator encapsulates granularity-specific allocation logic.
	CreateAllocator(ctx context.Context) ResourceAllocator

	// TotalAvailable returns total available GPUs for metrics/logging.
	TotalAvailable() int
}
