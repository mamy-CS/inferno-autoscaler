package fixtures

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// CreateModelService creates a model service deployment. Fails if the deployment already exists.
func CreateModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int) error {
	deployment := buildModelServiceDeployment(namespace, name, poolName, modelID, useSimulator, maxNumSeqs)
	_, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	return err
}

// DeleteModelService deletes the model service deployment. Idempotent; ignores NotFound.
func DeleteModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name string) error {
	deploymentName := name + "-decode"
	err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete model service deployment %s: %w", deploymentName, err)
	}
	return nil
}

// EnsureModelService creates or replaces the model service deployment (idempotent for test setup).
func EnsureModelService(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int) error {
	appLabel := name + "-decode"
	deploymentName := appLabel

	existingDeployment, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err == nil {
		if existingDeployment.Status.ReadyReplicas > 0 {
			return nil
		}
		propagationPolicy := metav1.DeletePropagationForeground
		deleteErr := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			if !errors.IsConflict(deleteErr) {
				return fmt.Errorf("delete existing deployment %s: %w", deploymentName, deleteErr)
			}
		}
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		for {
			_, checkErr := k8sClient.AppsV1().Deployments(namespace).Get(waitCtx, deploymentName, metav1.GetOptions{})
			if errors.IsNotFound(checkErr) {
				break
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for deployment %s to be deleted", deploymentName)
			}
			time.Sleep(2 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing deployment %s: %w", deploymentName, err)
	}

	deployment := buildModelServiceDeployment(namespace, name, poolName, modelID, useSimulator, maxNumSeqs)
	_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		propagationPolicy := metav1.DeletePropagationForeground
		_ = k8sClient.AppsV1().Deployments(namespace).Delete(ctx, deploymentName, metav1.DeleteOptions{
			PropagationPolicy: &propagationPolicy,
		})
		time.Sleep(2 * time.Second)
		_, err = k8sClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	}
	return err
}

func buildModelServiceDeployment(namespace, name, poolName, modelID string, useSimulator bool, maxNumSeqs int) *appsv1.Deployment {
	appLabel := name + "-decode"
	image := "ghcr.io/llm-d/llm-d-inference-sim:v0.6.1"
	if !useSimulator {
		image = "vllm/vllm-openai:latest"
	}
	args := buildModelServerArgs(modelID, useSimulator, maxNumSeqs)
	labels := map[string]string{
		"app":                       appLabel,
		"llm-d.ai/inferenceServing": "true",
		"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
		"llm-d.ai/model-pool":       poolName,
		"test-resource":             "true",
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appLabel,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                       appLabel,
					"llm-d.ai/inferenceServing": "true",
					"llm-d.ai/model":            "ms-sim-llm-d-modelservice",
					"llm-d.ai/model-pool":       poolName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            appLabel,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            args,
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP},
							},
							Env: []corev1.EnvVar{
								{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
								{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}},
								{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("4Gi"),
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func buildModelServerArgs(modelID string, useSimulator bool, maxNumSeqs int) []string {
	if useSimulator {
		return []string{
			"--model", modelID,
			"--port", "8000",
			fmt.Sprintf("--time-to-first-token=%d", 100),
			fmt.Sprintf("--inter-token-latency=%d", 20),
			"--mode=random",
			"--enable-kvcache",
			"--kv-cache-size=1024",
			"--block-size=16",
			"--tokenizers-cache-dir=/tmp",
			"--max-num-seqs", fmt.Sprintf("%d", maxNumSeqs),
			"--max-model-len", "1024",
		}
	}
	return []string{
		"--model", modelID,
		"--max-num-seqs", fmt.Sprintf("%d", maxNumSeqs),
		"--max-model-len", "1024",
		"--served-model-name", modelID,
		"--disable-log-requests",
	}
}
