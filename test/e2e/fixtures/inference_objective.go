package fixtures

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var inferenceObjectiveGVR = schema.GroupVersionResource{
	Group:    "inference.networking.x-k8s.io",
	Version:  "v1alpha2",
	Resource: "inferenceobjectives",
}

// EnsureInferenceObjective creates the e2e-default InferenceObjective for GIE flow-control
// queuing when the CRD exists. poolName must match the InferencePool metadata.name (typically the
// EPP Service name with an "-epp" suffix removed, e.g. gaie-sim-epp → gaie-sim).
//
// Returns applied=true if the object exists or was created. If the InferenceObjective API is not
// available on the cluster, returns (false, nil).
func EnsureInferenceObjective(ctx context.Context, dc dynamic.Interface, namespace, poolName string) (applied bool, err error) {
	obj := buildInferenceObjective(namespace, poolName)
	ri := dc.Resource(inferenceObjectiveGVR).Namespace(namespace)

	if _, cErr := ri.Create(ctx, obj, metav1.CreateOptions{}); cErr != nil {
		if apierrors.IsAlreadyExists(cErr) {
			return true, nil
		}
		if inferenceObjectiveAPIMissing(cErr) {
			return false, nil
		}
		return false, fmt.Errorf("create InferenceObjective e2e-default: %w", cErr)
	}
	return true, nil
}

// DeleteInferenceObjective removes e2e-default InferenceObjective if present.
func DeleteInferenceObjective(ctx context.Context, dc dynamic.Interface, namespace string) error {
	err := dc.Resource(inferenceObjectiveGVR).Namespace(namespace).Delete(ctx, "e2e-default", metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) || inferenceObjectiveAPIMissing(err) {
		return nil
	}
	return err
}

func buildInferenceObjective(namespace, poolName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "inference.networking.x-k8s.io/v1alpha2",
			"kind":       "InferenceObjective",
			"metadata": map[string]interface{}{
				"name":      "e2e-default",
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"priority": int64(0),
				"poolRef": map[string]interface{}{
					"name":  poolName,
					"kind":  "InferencePool",
					"group": "inference.networking.k8s.io",
				},
			},
		},
	}
}

func inferenceObjectiveAPIMissing(err error) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	// Discovery / dynamic client sometimes returns ResourceNotFound with Reason NotFound
	return apierrors.ReasonForError(err) == metav1.StatusReasonNotFound
}
