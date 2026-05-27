package controller

import (
	"context"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRegistryAuthsFromDockerConfigJSONSecret(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("AWS:token"))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "default"},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://123456789012.dkr.ecr.us-east-1.amazonaws.com":{"auth":"` + auth + `"}}}`),
		},
	}

	auths, err := registryAuthsFromSecret(secret)
	if err != nil {
		t.Fatalf("registryAuthsFromSecret returned error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths length = %d, want 1", len(auths))
	}
	got := auths[0]
	if got.Host != "123456789012.dkr.ecr.us-east-1.amazonaws.com" || got.Username != "AWS" || got.Secret != "token" {
		t.Fatalf("auth = %#v, want normalized ECR auth", got)
	}
}

func TestRegistryAuthsFromPodImagePullSecrets(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: "apps"},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"public.ecr.aws":{"username":"AWS","password":"token"}}}`),
		},
	}
	client := fake.NewSimpleClientset(secret)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "apps"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull-secret"}},
		},
	}

	auths, err := registryAuthsFromPod(context.Background(), client, pod)
	if err != nil {
		t.Fatalf("registryAuthsFromPod returned error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auths length = %d, want 1", len(auths))
	}
	if auths[0].Host != "public.ecr.aws" || auths[0].Username != "AWS" || auths[0].Secret != "token" {
		t.Fatalf("auth = %#v, want pod pull secret auth", auths[0])
	}
}

func TestRegistryHostsMatchDockerHubAliases(t *testing.T) {
	if !registryHostsMatch("https://index.docker.io/v1/", "registry-1.docker.io") {
		t.Fatal("expected Docker Hub auth host to match registry-1.docker.io")
	}
	if registryHostsMatch("public.ecr.aws", "registry-1.docker.io") {
		t.Fatal("did not expect ECR auth host to match Docker Hub")
	}
}
