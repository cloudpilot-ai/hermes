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
	refspec, err := reference.Parse("docker.io/opensearchproject/opensearch:2.19.1")
	if err != nil {
		t.Fatal(err)
	}
	layerDesc := ocispec.Descriptor{Digest: digest.FromString("layer")}
	sociDesc := ocispec.Descriptor{Digest: digest.FromString("ztoc")}
	oldPrefetch := &ocispec.Descriptor{
		Digest: digest.FromString("prefetch-old"),
		Annotations: map[string]string{
			soci.IndexAnnotationHermesPrefetchProfile: "opensearch-jvm-v1",
		},
	}
	newPrefetch := &ocispec.Descriptor{
		Digest: digest.FromString("prefetch-new"),
		Annotations: map[string]string{
			soci.IndexAnnotationHermesPrefetchProfile: "opensearch-jvm-v2",
		},
	}

	oldKey := resolveCacheKey(refspec, layerDesc, sociDesc, oldPrefetch, BackgroundFetchPolicy{Mode: BackgroundFetchDisabled}, true)
	newKey := resolveCacheKey(refspec, layerDesc, sociDesc, newPrefetch, BackgroundFetchPolicy{Mode: BackgroundFetchDisabled}, true)
	if oldKey == newKey {
		t.Fatalf("resolve cache key did not include prefetch/profile identity: %q", oldKey)
	}
}

func TestStartupMaterializeCandidatesPrioritizeCriticalOpenSearchFiles(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "usr/share/opensearch/plugins/security/plugin-security.jar", Type: "reg", UncompressedOffset: 300, UncompressedSize: 10},
		{Name: "usr/share/opensearch/jdk/lib/modules", Type: "reg", UncompressedOffset: 100, UncompressedSize: 100},
		{Name: "usr/share/opensearch/config/opensearch.yml", Type: "reg", UncompressedOffset: 200, UncompressedSize: 1},
		{Name: "usr/share/opensearch/data/nodes/0/ignored", Type: "reg", UncompressedOffset: 400, UncompressedSize: 1},
		{Name: "usr/share/opensearch/lib/opensearch.jar", Type: "reg", UncompressedOffset: 500, UncompressedSize: 20},
		{Name: "usr/share/opensearch/modules/analysis-common/plugin-descriptor.properties", Type: "reg", UncompressedOffset: 600, UncompressedSize: 1},
		{Name: "usr/share/opensearch/modules/analysis-common", Type: "dir", UncompressedOffset: 700, UncompressedSize: 0},
	}

	got := startupMaterializeCandidates(files)
	names := make([]string, 0, len(got))
	for _, item := range got {
		names = append(names, item.name)
	}
	want := []string{
		"usr/share/opensearch/jdk/lib/modules",
		"usr/share/opensearch/config/opensearch.yml",
		"usr/share/opensearch/lib/opensearch.jar",
		"usr/share/opensearch/modules/analysis-common/plugin-descriptor.properties",
		"usr/share/opensearch/plugins/security/plugin-security.jar",
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
		{path: "usr/share/opensearch/bin/opensearch", want: true},
		{path: "usr/share/opensearch/config/jvm.options", want: true},
		{path: "usr/share/opensearch/jdk/lib/server/libjvm.so", want: true},
		{path: "usr/share/opensearch/jdk/lib/modules", want: true},
		{path: "usr/share/opensearch/modules/analysis-common/plugin-descriptor.properties", want: true},
		{path: "usr/share/opensearch/plugins/security/security.policy", want: true},
		{path: "usr/share/opensearch/plugins/security/model.bin", want: false},
		{path: "var/log/opensearch/server.log", want: false},
	}
	for _, tt := range tests {
		if got := isStartupMaterializePath(tt.path); got != tt.want {
			t.Fatalf("isStartupMaterializePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestNormalizeStartupMaterializePath(t *testing.T) {
	if got := normalizeStartupMaterializePath("usr/share/opensearch/lib/opensearch.jar"); got != "usr/share/opensearch/lib/opensearch.jar" {
		t.Fatalf("normalized path = %q", got)
	}
	if got := normalizeStartupMaterializePath("/../usr/share/opensearch/lib/opensearch.jar"); got != "" {
		t.Fatalf("malformed path normalized to %q", got)
	}
}

func TestStartupMaterializeCandidateDedupesNormalizedNames(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "usr/share/opensearch/lib/opensearch.jar", Type: "reg", UncompressedOffset: compression.Offset(1), UncompressedSize: 1},
		{Name: "usr/share/opensearch/lib/opensearch.jar", Type: "reg", UncompressedOffset: compression.Offset(2), UncompressedSize: 1},
	}
	got := startupMaterializeCandidates(files)
	if len(got) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(got), got)
	}
}

func TestStartupMaterializeCandidateRejectsMalformedTarNames(t *testing.T) {
	files := []ztoc.FileMetadata{
		{Name: "/../usr/share/opensearch/lib/opensearch.jar", Type: "reg", UncompressedOffset: compression.Offset(1), UncompressedSize: 1},
	}
	if got := startupMaterializeCandidates(files); len(got) != 0 {
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
		Files: map[string]string{"usr/share/opensearch/jdk/lib/modules": "/tmp/modules"},
	}
	if shouldEnableStartupHotCache(true, materialized) {
		t.Fatal("hot cache enabled despite materialized startup files")
	}
}
