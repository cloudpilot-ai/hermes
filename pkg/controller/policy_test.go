package controller

import (
	"testing"

	hermesv1 "github.com/cloudpilot-ai/hermes/pkg/apis/v1alpha1"
	sociapi "github.com/cloudpilot-ai/hermes/pkg/common/soci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	if targets[0].Acceleration.PrefetchProfile() != buildProfileStartupLocal {
		t.Fatalf("vllm acceleration profile = %q, want %q", targets[0].Acceleration.PrefetchProfile(), buildProfileStartupLocal)
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

func TestHermesPolicyStoreMatchImageUsesInternalStartupLocalProfile(t *testing.T) {
	store := NewHermesPolicyStore()
	store.Upsert(&hermesv1.HermesPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a"},
		Spec: hermesv1.HermesPolicySpec{
			ImageSelectors: []hermesv1.HermesImageSelector{{ImageRegex: ".*example.*"}},
		},
	})
	store.Upsert(&hermesv1.HermesPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "app-b"},
		Spec: hermesv1.HermesPolicySpec{
			ImageSelectors: []hermesv1.HermesImageSelector{{ImageRegex: ".*example.*"}},
		},
	})

	targets := store.MatchImage("ghcr.io/acme/example:1.0", "linux/amd64")
	if len(targets) != 1 {
		t.Fatalf("targets length = %d, want 1: %#v", len(targets), targets)
	}
	got := targets[0].Acceleration
	if got.PrefetchProfile() != buildProfileStartupLocal {
		t.Fatalf("prefetch profile = %q, want %q", got.PrefetchProfile(), buildProfileStartupLocal)
	}
}

func TestBuildAccelerationDefaultIsNoop(t *testing.T) {
	var zero BuildAcceleration
	if key := zero.Key(); key != "" {
		t.Fatalf("default key = %q, want empty", key)
	}
	if annotations := zero.IndexAnnotations(); annotations != nil {
		t.Fatalf("default annotations = %#v, want nil", annotations)
	}
	if got := zero.MinLayerSize(10 << 20); got != 10<<20 {
		t.Fatalf("default MinLayerSize = %d, want base", got)
	}
}

func TestStartupLocalProfileAnnotationsAndKey(t *testing.T) {
	acceleration := BuildAcceleration{
		Profile:              buildProfileStartupLocal,
		PrefetchPathPatterns: []string{"opt/app/bin/server", "opt/app/config/"},
	}
	annotations := acceleration.IndexAnnotations()
	if got := annotations[sociapi.IndexAnnotationHermesBackgroundFetch]; got != sociapi.IndexAnnotationHermesBackgroundFetchDisabled {
		t.Fatalf("background fetch annotation = %q, want disabled", got)
	}
	if got := annotations[sociapi.IndexAnnotationHermesPrefetchProfile]; got != buildProfileStartupLocal {
		t.Fatalf("prefetch profile annotation = %q, want %q", got, buildProfileStartupLocal)
	}
	if got := annotations[sociapi.IndexAnnotationHermesSkipFileVerification]; got != "true" {
		t.Fatalf("skip verification annotation = %q, want true", got)
	}
	if key := acceleration.Key(); key == "" {
		t.Fatalf("startup-local profile key is empty")
	}
	startKey := acceleration.Key()
	otherPaths := BuildAcceleration{
		Profile:              buildProfileStartupLocal,
		PrefetchPathPatterns: []string{"opt/other/bin/server"},
	}
	if changedKey := otherPaths.Key(); changedKey != startKey {
		t.Fatalf("startup-local key changed with image-specific prefetch paths: got %s, want %s", changedKey, startKey)
	}
}

func TestStartupLocalProfileDerivesImageSpecificPrefetchPaths(t *testing.T) {
	acceleration := buildAccelerationForImageRef("ghcr.io/acme/example:1.0").WithImageConfig(ocispec.ImageConfig{
		Entrypoint: []string{"/opt/acme/bin/server"},
		WorkingDir: "/opt/acme",
		Env:        []string{"PATH=/custom/bin:/usr/bin"},
	})
	paths := map[string]struct{}{}
	for _, item := range acceleration.PrefetchPaths() {
		paths[item] = struct{}{}
	}
	for _, want := range []string{
		"opt/acme/bin/server",
		"opt/acme/config/",
		"opt/acme/lib/*.jar",
		"opt/acme/plugins/*/*.jar",
	} {
		if _, ok := paths[want]; !ok {
			t.Fatalf("derived prefetch paths missing %q from %#v", want, acceleration.PrefetchPaths())
		}
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

func TestTaskKeySeparatesAccelerationProfiles(t *testing.T) {
	base := taskKey("ghcr.io/acme/example:1.0", "linux/amd64")
	accelerated := taskKey("ghcr.io/acme/example:1.0", "linux/amd64", "startup-local-v1:abc123")
	if base == accelerated {
		t.Fatalf("taskKey did not include acceleration key: %q", base)
	}
	if got := taskKey("ghcr.io/acme/example:1.0", "linux/amd64", ""); got != base {
		t.Fatalf("empty acceleration key changed task key: %q, want %q", got, base)
	}
}
