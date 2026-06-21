package fs

import (
	"testing"

	"github.com/cloudpilot-ai/hermes/pkg/common/soci"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/fs/layer"
)

func TestSociContextBackgroundFetchPolicy(t *testing.T) {
	tests := []struct {
		name           string
		annotations    map[string]string
		wantMode       layer.BackgroundFetchMode
		wantPreResolve bool
		wantPause      bool
		wantSkipVerify bool
	}{
		{
			name:           "legacy index uses default background fetch behavior",
			annotations:    nil,
			wantMode:       layer.BackgroundFetchEnabled,
			wantPreResolve: true,
			wantPause:      true,
		},
		{
			name: "startup first",
			annotations: map[string]string{
				soci.IndexAnnotationHermesBackgroundFetch:      soci.IndexAnnotationHermesBackgroundFetchDisabled,
				soci.IndexAnnotationHermesSkipFileVerification: "true",
				soci.IndexAnnotationHermesPrefetchProfile:      "opensearch-jvm-v1",
			},
			wantMode:       layer.BackgroundFetchDisabled,
			wantPreResolve: false,
			wantPause:      true,
			wantSkipVerify: true,
		},
		{
			name: "warm cache",
			annotations: map[string]string{
				soci.IndexAnnotationHermesBackgroundFetch: soci.IndexAnnotationHermesBackgroundFetchEnabled,
			},
			wantMode:       layer.BackgroundFetchEnabled,
			wantPreResolve: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &sociContext{sociIndex: &soci.Index{Annotations: tt.annotations}}
			policy := c.backgroundFetchPolicy()
			if policy.Mode != tt.wantMode {
				t.Fatalf("mode = %q, want %q", policy.Mode, tt.wantMode)
			}
			if got := c.shouldPreResolveNeighboringLayers(); got != tt.wantPreResolve {
				t.Fatalf("preResolve = %v, want %v", got, tt.wantPreResolve)
			}
			if got := c.shouldPauseBackgroundFetchOnMount(); got != tt.wantPause {
				t.Fatalf("pause = %v, want %v", got, tt.wantPause)
			}
			if got := c.skipFileVerification(); got != tt.wantSkipVerify {
				t.Fatalf("skipFileVerification = %v, want %v", got, tt.wantSkipVerify)
			}
		})
	}
}

func TestSociContextCacheKeyIncludesIndexDigest(t *testing.T) {
	imageDigest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	keyA := sociContextCacheKey(imageDigest, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	keyB := sociContextCacheKey(imageDigest, "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if keyA == keyB {
		t.Fatalf("cache key did not include index digest: %q", keyA)
	}
}
