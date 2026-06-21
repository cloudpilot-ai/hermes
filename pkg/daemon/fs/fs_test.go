package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cloudpilot-ai/hermes/pkg/common/config"
	"github.com/cloudpilot-ai/hermes/pkg/common/soci"
	socistore "github.com/cloudpilot-ai/hermes/pkg/common/soci/store"
	"github.com/cloudpilot-ai/hermes/pkg/daemon/fs/layer"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
				soci.IndexAnnotationHermesPrefetchProfile:      "startup-local-v1",
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

func TestGetSociContextFallsBackWhenHermesFetchFails(t *testing.T) {
	ctx := context.Background()
	localStore, err := socistore.NewSociStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	fallbackIndex := soci.NewIndex(soci.V1, nil, nil, map[string]string{"source": "registry-fallback"})
	fallbackBytes, err := soci.MarshalIndex(fallbackIndex)
	if err != nil {
		t.Fatal(err)
	}
	fallbackDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(fallbackBytes),
		Size:      int64(len(fallbackBytes)),
	}
	if err := localStore.Push(ctx, fallbackDesc, bytes.NewReader(fallbackBytes)); err != nil {
		t.Fatal(err)
	}

	var hermesBlobRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/indexes/resolve":
			_ = json.NewEncoder(w).Encode(hermesResolveResponse{
				Image:     r.URL.Query().Get("image"),
				Platform:  "linux/amd64",
				SOCIIndex: fallbackDesc,
			})
		case "/v1/blobs/" + fallbackDesc.Digest.String():
			hermesBlobRequests.Add(1)
			http.Error(w, "Hermes blob unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fs := &filesystem{
		ctx:          fsCtx,
		contentStore: localStore,
		externalArtifactStore: config.ExternalArtifactStoreConfig{
			Enable:             true,
			Endpoint:           server.URL,
			TimeoutSec:         1,
			Platform:           "linux/amd64",
			FallbackToRegistry: true,
		},
	}

	imageManifestDigest := digest.FromString("image-manifest").String()
	c, err := fs.getSociContext(ctx, "example.com/acme/app:latest", fallbackDesc.Digest.String(), imageManifestDigest, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.sociIndex.Annotations["source"]; got != "registry-fallback" {
		t.Fatalf("soci index source = %q, want registry fallback", got)
	}
	if hermesBlobRequests.Load() == 0 {
		t.Fatalf("Hermes blob endpoint was not exercised")
	}
}
