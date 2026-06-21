package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodImageInputsChanged(t *testing.T) {
	oldPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:       []corev1.Container{{Name: "app", Image: "nginx:1"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}},
		},
	}
	statusOnly := oldPod.DeepCopy()
	statusOnly.Status.Phase = corev1.PodRunning
	if podImageInputsChanged(oldPod, statusOnly) {
		t.Fatalf("status-only pod update changed image inputs")
	}

	imageChanged := oldPod.DeepCopy()
	imageChanged.Spec.Containers[0].Image = "nginx:2"
	if !podImageInputsChanged(oldPod, imageChanged) {
		t.Fatalf("image update did not change image inputs")
	}

	secretChanged := oldPod.DeepCopy()
	secretChanged.Spec.ImagePullSecrets[0].Name = "other"
	if !podImageInputsChanged(oldPod, secretChanged) {
		t.Fatalf("imagePullSecrets update did not change image inputs")
	}
}
