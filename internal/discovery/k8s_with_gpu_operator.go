package discovery

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vendors list for GPU vendors
var vendors = []string{
	"nvidia.com",
	"amd.com",
	"intel.com",
}

// K8sWithGpuOperator implements CapacityDiscovery for Kubernetes clusters with GPU Operator
type K8sWithGpuOperator struct {
	Client client.Client
}

// NewK8sWithGpuOperator creates a new K8sWithGpuOperator instance.
func NewK8sWithGpuOperator(client client.Client) *K8sWithGpuOperator {
	return &K8sWithGpuOperator{
		Client: client,
	}
}

// Discover discovers GPU capacity by iterating over nodes and checking GFD labels.
func (d *K8sWithGpuOperator) Discover(ctx context.Context) (map[string]map[string]AcceleratorModelInfo, error) {
	// Optimization: Filter nodes server-side using LabelSelector
	// We only care about nodes that have the NVIDIA GPU product label for now.
	// This prevents listing thousands of CPU-only nodes in large clusters.
	// TODO: To support AMD/Intel in the future, we need to iterate over vendors and perform multiple list calls
	// because K8s LabelSelectors do not support OR logic across different keys (e.g. nvidia OR amd).
	req, err := labels.NewRequirement("nvidia.com/gpu.product", selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create label requirement: %w", err)
	}
	selector := labels.NewSelector().Add(*req)

	// Check for WVA_NODE_SELECTOR environment variable for sharding
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, err := labels.Parse(selectorStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", err)
		}
		// Merge user requirements into the existing selector
		requirements, _ := userSelector.Requirements()
		for _, req := range requirements {
			selector = selector.Add(req)
		}
	}

	var nodeList corev1.NodeList
	if err := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	inv := make(map[string]map[string]AcceleratorModelInfo)

	for _, node := range nodeList.Items {
		nodeName := node.Name

		for _, vendor := range vendors {
			prodKey := vendor + "/gpu.product"
			memKey := vendor + "/gpu.memory"

			if model, ok := node.Labels[prodKey]; ok {
				mem := node.Labels[memKey]
				count := 0
				if cap, ok := node.Status.Allocatable[corev1.ResourceName(vendor+"/gpu")]; ok {
					count = int(cap.Value())
				}

				if inv[nodeName] == nil {
					inv[nodeName] = make(map[string]AcceleratorModelInfo)
				}

				inv[nodeName][model] = AcceleratorModelInfo{
					Count:  count,
					Memory: mem,
				}
			}
		}
	}

	return inv, nil
}

// DiscoverUsage calculates current GPU usage by summing GPU requests from running pods.
// Returns a map of accelerator type to used GPU count.
func (d *K8sWithGpuOperator) DiscoverUsage(ctx context.Context) (map[string]int, error) {
	// First, build a map of node name -> GPU type
	nodeGPUType, err := d.discoverNodeGPUTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover node GPU types: %w", err)
	}

	// List all pods (running or pending on a node)
	var podList corev1.PodList
	if err := d.Client.List(ctx, &podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Aggregate GPU requests by accelerator type
	usageByType := make(map[string]int)

	for _, pod := range podList.Items {
		// Skip pods that aren't scheduled or are completed/failed
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		// Get the GPU type for this node
		gpuType, ok := nodeGPUType[pod.Spec.NodeName]
		if !ok {
			// Node doesn't have GPUs, skip
			continue
		}

		// Sum GPU requests from all containers
		gpuCount := getPodGPURequests(&pod)
		if gpuCount > 0 {
			usageByType[gpuType] += gpuCount
		}
	}

	return usageByType, nil
}

// discoverNodeGPUTypes returns a map of node name to GPU type (model name).
func (d *K8sWithGpuOperator) discoverNodeGPUTypes(ctx context.Context) (map[string]string, error) {
	// Use the same filtering as Discover()
	req, err := labels.NewRequirement("nvidia.com/gpu.product", selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create label requirement: %w", err)
	}
	selector := labels.NewSelector().Add(*req)

	// Check for WVA_NODE_SELECTOR environment variable for sharding
	if selectorStr := os.Getenv("WVA_NODE_SELECTOR"); selectorStr != "" {
		userSelector, err := labels.Parse(selectorStr)
		if err != nil {
			return nil, fmt.Errorf("invalid WVA_NODE_SELECTOR: %w", err)
		}
		requirements, _ := userSelector.Requirements()
		for _, req := range requirements {
			selector = selector.Add(req)
		}
	}

	var nodeList corev1.NodeList
	if err := d.Client.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	nodeGPUType := make(map[string]string)
	for _, node := range nodeList.Items {
		for _, vendor := range vendors {
			prodKey := vendor + "/gpu.product"
			if model, ok := node.Labels[prodKey]; ok {
				nodeGPUType[node.Name] = model
				break // Only one GPU type per node expected
			}
		}
	}

	return nodeGPUType, nil
}

// getPodGPURequests returns the total GPU requests for a pod across all containers.
// For regular containers, GPUs are summed (they run concurrently).
// For init containers, we take the max (they run sequentially).
// The final result is max(initContainerMax, regularContainerSum) since init containers
// complete before regular containers start.
func getPodGPURequests(pod *corev1.Pod) int {
	// Sum GPU requests from regular containers (run concurrently)
	regularTotal := 0
	for _, container := range pod.Spec.Containers {
		for _, vendor := range vendors {
			resName := corev1.ResourceName(vendor + "/gpu")
			if qty, ok := container.Resources.Requests[resName]; ok {
				regularTotal += int(qty.Value())
			}
		}
	}

	// Find max GPU request from init containers (run sequentially)
	initMax := 0
	for _, container := range pod.Spec.InitContainers {
		containerGPUs := 0
		for _, vendor := range vendors {
			resName := corev1.ResourceName(vendor + "/gpu")
			if qty, ok := container.Resources.Requests[resName]; ok {
				containerGPUs += int(qty.Value())
			}
		}
		if containerGPUs > initMax {
			initMax = containerGPUs
		}
	}

	// Return max of init containers and regular containers
	// (init containers finish before regular containers start)
	if initMax > regularTotal {
		return initMax
	}
	return regularTotal
}

// Ensure K8sWithGpuOperator implements FullDiscovery
var _ FullDiscovery = (*K8sWithGpuOperator)(nil)
