package layer

import (
	"testing"

	"github.com/cloudpilot-ai/hermes/pkg/common/soci"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci/ztoc/compression"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/fs/reader"
	"github.com/containerd/containerd/reference"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestResolveCacheKeyIncludesStartupProfileInputs(t *testing.T) {
	refspec, err := reference.Parse("ghcr.io/acme/example:1.0")
	if err != nil {
		t.Fatal(err)
	}
	layerDesc := ocispec.Descriptor{Digest: digest.FromString("layer")}
	sociDesc := ocispec.Descriptor{Digest: digest.FromString("ztoc")}
	oldPrefetch := &ocispec.Descriptor{
		Digest: digest.FromString("prefetch-old"),
		Annotations: map[string]string{
			soci.IndexAnnotationHermesPrefetchProfile: "startup-local-v1",
		},
	}
	newPrefetch := &ocispec.Descriptor{
		Digest: digest.FromString("prefetch-new"),
		Annotations: map[string]string{
			soci.IndexAnnotationHermesPrefetchProfile: "startup-local-v2",
		},
	}

	oldKey := resolveCacheKey(refspec, layerDesc, sociDesc, oldPrefetch, BackgroundFetchPolicy{Mode: BackgroundFetchDisabled}, true)
	newKey := resolveCacheKey(refspec, layerDesc, sociDesc, newPrefetch, BackgroundFetchPolicy{Mode: BackgroundFetchDisabled}, true)
	if oldKey == newKey {
		t.Fatalf("resolve cache key did not include prefetch/profile identity: %q", oldKey)
	}
}

func TestStartupMaterializeCandidatesPrioritizeCriticalStartupFiles(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "opt/acme/plugins/security/plugin-security.jar", Type: "reg", UncompressedOffset: 300, UncompressedSize: 10},
		{Name: "opt/acme/runtime/lib/modules", Type: "reg", UncompressedOffset: 100, UncompressedSize: 100},
		{Name: "opt/acme/config/app.yml", Type: "reg", UncompressedOffset: 200, UncompressedSize: 1},
		{Name: "opt/acme/data/nodes/0/ignored", Type: "reg", UncompressedOffset: 400, UncompressedSize: 1},
		{Name: "opt/acme/lib/app.jar", Type: "reg", UncompressedOffset: 500, UncompressedSize: 20},
		{Name: "opt/acme/modules/analysis/plugin-descriptor.properties", Type: "reg", UncompressedOffset: 600, UncompressedSize: 1},
		{Name: "opt/acme/modules/analysis", Type: "dir", UncompressedOffset: 700, UncompressedSize: 0},
	}

	patterns := []string{
		"opt/acme/config/",
		"opt/acme/lib/*.jar",
		"opt/acme/modules/*/plugin-descriptor.properties",
		"opt/acme/plugins/*/*.jar",
		"opt/acme/runtime/lib/modules",
	}
	got := startupMaterializeCandidates(files, patterns)
	names := make([]string, 0, len(got))
	for _, item := range got {
		names = append(names, item.name)
	}
	want := []string{
		"opt/acme/runtime/lib/modules",
		"opt/acme/config/app.yml",
		"opt/acme/modules/analysis/plugin-descriptor.properties",
		"opt/acme/lib/app.jar",
		"opt/acme/plugins/security/plugin-security.jar",
	}
	if len(names) != len(want) {
		t.Fatalf("candidates = %#v, want %#v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("candidates = %#v, want %#v", names, want)
		}
	}
}

func TestStartupMaterializePathMatcher(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "opt/acme/bin/server", want: true},
		{path: "opt/acme/config/app.yml", want: true},
		{path: "opt/acme/runtime/lib/server/libvm.so", want: true},
		{path: "opt/acme/runtime/lib/modules", want: true},
		{path: "opt/acme/modules/analysis/plugin-descriptor.properties", want: true},
		{path: "opt/acme/plugins/security/security.policy", want: true},
		{path: "opt/acme/plugins/security/model.bin", want: false},
		{path: "var/log/app/server.log", want: false},
	}
	for _, tt := range tests {
		if got := isStartupMaterializePath(tt.path, nil); got != tt.want {
			t.Fatalf("isStartupMaterializePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNormalizeStartupMaterializePath(t *testing.T) {
	if got := normalizeStartupMaterializePath("opt/acme/lib/app.jar"); got != "opt/acme/lib/app.jar" {
		t.Fatalf("normalized path = %q", got)
	}
	if got := normalizeStartupMaterializePath("/../opt/acme/lib/app.jar"); got != "" {
		t.Fatalf("malformed path normalized to %q", got)
	}
}

func TestStartupMaterializeCandidateDedupesNormalizedNames(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "opt/acme/lib/app.jar", Type: "reg", UncompressedOffset: compression.Offset(1), UncompressedSize: 1},
		{Name: "opt/acme/lib/app.jar", Type: "reg", UncompressedOffset: compression.Offset(2), UncompressedSize: 1},
	}
	got := startupMaterializeCandidates(files, nil)
	if len(got) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(got), got)
	}
}

func TestStartupMaterializeCandidateRejectsMalformedTarNames(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "/../opt/acme/lib/app.jar", Type: "reg", UncompressedOffset: compression.Offset(1), UncompressedSize: 1},
	}
	if got := startupMaterializeCandidates(files, nil); len(got) != 0 {
		t.Fatalf("malformed candidate was accepted: %#v", got)
	}
}

func TestShouldEnableStartupHotCacheOnlyWithoutMaterializedFiles(t *testing.T) {
	if shouldEnableStartupHotCache(false, nil) {
		t.Fatal("hot cache enabled for non-startup profile")
	}
	if !shouldEnableStartupHotCache(true, nil) {
		t.Fatal("hot cache disabled when startup profile has no materialized files")
	}
	materialized := &reader.MaterializedFileSet{
		Files: map[string]string{"opt/acme/runtime/lib/modules": "/tmp/modules"},
	}
	if shouldEnableStartupHotCache(true, materialized) {
		t.Fatal("hot cache enabled despite materialized startup files")
	}
}
