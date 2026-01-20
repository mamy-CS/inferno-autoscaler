package utils

import (
	"context"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// IsHPAScaleToZeroEnabled checks if the HPAScaleToZero feature gate is enabled on the cluster.
// On OpenShift, this checks the FeatureGate custom resource.
// On vanilla Kubernetes, this checks the kube-controller-manager pod args.
func IsHPAScaleToZeroEnabled(ctx context.Context, k8sClient *kubernetes.Clientset, w io.Writer) bool {
	// Use discovery to check if this is an OpenShift cluster
	_, resources, err := k8sClient.Discovery().ServerGroupsAndResources()
	if err != nil {
		_, _ = fmt.Fprintf(w, "Warning: Could not discover API resources: %v\n", err)
		return false
	}

	isOpenShift := false
	for _, resourceList := range resources {
		for _, resource := range resourceList.APIResources {
			if resource.Name == "featuregates" && resourceList.GroupVersion == "config.openshift.io/v1" {
				isOpenShift = true
				break
			}
		}
		if isOpenShift {
			break
		}
	}

	if isOpenShift {
		// On OpenShift, check the FeatureGate CR
		// We use raw REST client to avoid importing OpenShift types
		result, err := k8sClient.RESTClient().
			Get().
			AbsPath("/apis/config.openshift.io/v1/featuregates/cluster").
			DoRaw(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "Warning: Could not get FeatureGate CR: %v\n", err)
			return false
		}

		// Check if HPAScaleToZero is in the enabled features
		resultStr := string(result)
		if strings.Contains(resultStr, "HPAScaleToZero") {
			_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is enabled on OpenShift\n")
			return true
		}
		_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is NOT enabled on OpenShift\n")
		return false
	}

	// On vanilla Kubernetes, check kube-controller-manager pod args
	pods, err := k8sClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "component=kube-controller-manager",
	})
	if err != nil || len(pods.Items) == 0 {
		_, _ = fmt.Fprintf(w, "Warning: Could not check kube-controller-manager: %v\n", err)
		return false
	}

	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			for _, arg := range container.Args {
				if strings.Contains(arg, "HPAScaleToZero=true") {
					_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is enabled\n")
					return true
				}
			}
		}
	}

	_, _ = fmt.Fprintf(w, "HPAScaleToZero feature gate is NOT enabled\n")
	return false
}
