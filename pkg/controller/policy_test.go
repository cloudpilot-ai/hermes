package controller

import (
	"testing"

	hermesv1 "github.com/cloudpilot-ai/hermes/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHermesPolicyStoreMatchImage(t *testing.T) {
	store := NewHermesPolicyStore()
	store.Upsert(&hermesv1.HermesPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm"},
		Spec: hermesv1.HermesPolicySpec{
			ImageSelectors: []hermesv1.HermesImageSelector{{ImageRegex: ".*vllm.*"}},
			Platforms:      []string{"linux/amd64", "linux/arm64"},
		},
	})
	store.Upsert(&hermesv1.HermesPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "paused"},
		Spec: hermesv1.HermesPolicySpec{
			Paused:         true,
			ImageSelectors: []hermesv1.HermesImageSelector{{ImageRegex: ".*vllm.*"}},
			Platforms:      []string{"linux/s390x"},
		},
	})
	store.Upsert(&hermesv1.HermesPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
		Spec: hermesv1.HermesPolicySpec{
			ImageSelectors: []hermesv1.HermesImageSelector{{ImageRegex: ".*nginx.*"}},
		},
	})

	targets := store.MatchImage("ecr.com/prod/vllm:v11.0.0", "linux/amd64")
	if len(targets) != 2 {
		t.Fatalf("targets length = %d, want 2: %#v", len(targets), targets)
	}
	if targets[0].Platform != "linux/amd64" || len(targets[0].PolicyNames) != 1 || targets[0].PolicyNames[0] != "vllm" {
		t.Fatalf("unexpected amd64 target: %#v", targets[0])
	}
	if targets[1].Platform != "linux/arm64" || len(targets[1].PolicyNames) != 1 || targets[1].PolicyNames[0] != "vllm" {
		t.Fatalf("unexpected arm64 target: %#v", targets[1])
	}

	targets = store.MatchImage("docker.io/library/nginx:latest", "linux/amd64")
	if len(targets) != 1 {
		t.Fatalf("targets length = %d, want 1: %#v", len(targets), targets)
	}
	if targets[0].Platform != "linux/amd64" || len(targets[0].PolicyNames) != 1 || targets[0].PolicyNames[0] != "nginx" {
		t.Fatalf("unexpected nginx target: %#v", targets[0])
	}

	if targets := store.MatchImage("docker.io/library/redis:latest", "linux/amd64"); len(targets) != 0 {
		t.Fatalf("redis matched unexpectedly: %#v", targets)
	}
}

func TestSetHermesImageStatus(t *testing.T) {
	status := &hermesv1.HermesPolicyStatus{}
	setHermesImageStatus(status, hermesv1.HermesImageStatus{
		ImageDigestRef: "ecr.com/prod/vllm@sha256:amd64",
		Platform:       "linux/amd64",
		Phase:          hermesv1.HermesImagePhaseBuilding,
	})
	setHermesImageStatus(status, hermesv1.HermesImageStatus{
		ImageDigestRef: "ecr.com/prod/vllm@sha256:amd64",
		Platform:       "linux/amd64",
		Phase:          hermesv1.HermesImagePhaseReady,
	})
	setHermesImageStatus(status, hermesv1.HermesImageStatus{
		ImageDigestRef: "ecr.com/prod/vllm@sha256:arm64",
		Platform:       "linux/arm64",
		Phase:          hermesv1.HermesImagePhaseFailed,
		Error:          "boom",
	})

	if len(status.Images) != 2 {
		t.Fatalf("status images length = %d, want 2", len(status.Images))
	}
	if status.Ready != 1 || status.Failed != 1 {
		t.Fatalf("counts ready=%d failed=%d, want 1/1", status.Ready, status.Failed)
	}
}

func TestBuilderPolicyNamesForStatusIncludesRawAndDigestPolicies(t *testing.T) {
	builder := NewBuilder(Config{Platform: "linux/amd64", QueueSize: 1}, nil)
	rawKey := taskKey("ecr.com/prod/vllm:v1", "linux/amd64")
	digestKey := taskKey("ecr.com/prod/vllm@sha256:abcd", "linux/amd64")

	builder.rememberRawPolicies(rawKey, []string{"policy-a"})
	builder.rememberDigestPolicies(digestKey, []string{"policy-b", "policy-a"})

	names := builder.policyNamesForStatus(BuildTask{
		SourceImageRef: "ecr.com/prod/vllm:v1",
		Platform:       "linux/amd64",
		PolicyNames:    []string{"policy-c"},
	}, "ecr.com/prod/vllm@sha256:abcd", "linux/amd64")

	want := []string{"policy-a", "policy-b", "policy-c"}
	if len(names) != len(want) {
		t.Fatalf("policy names = %#v, want %#v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("policy names = %#v, want %#v", names, want)
		}
	}
}
