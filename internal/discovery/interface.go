package discovery

import "context"

// CapacityDiscovery defines the interface for discovering accelerator capacity in the cluster.
type CapacityDiscovery interface {
	// Discover returns a map of node names to their accelerator inventory.
	// The inner map is keyed by accelerator model name (e.g. "NVIDIA-A100").
	Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error)
}

// UsageDiscovery defines the interface for discovering current GPU usage in the cluster.
type UsageDiscovery interface {
	// DiscoverUsage returns a map of accelerator type to used GPU count.
	// This is calculated by summing GPU requests from all running pods.
	DiscoverUsage(ctx context.Context) (map[string]int, error)
}

// FullDiscovery combines capacity and usage discovery for complete inventory tracking.
type FullDiscovery interface {
	CapacityDiscovery
	UsageDiscovery
}
