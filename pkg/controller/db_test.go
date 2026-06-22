package controller

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestStoreKeepsAccelerationArtifactsSeparate(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	image := "ghcr.io/acme/example@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	platform := "linux/amd64"
	accelerationKey := BuildAcceleration{Profile: buildProfileStartupLocal}.Key()

	defaultIndex := putReadyArtifactForTest(t, ctx, store, image, platform, "", "default-index")
	acceleratedIndex := putReadyArtifactForTest(t, ctx, store, image, platform, accelerationKey, "accelerated-index")

	defaultResp, err := store.Resolve(ctx, image, platform, "")
	if err != nil {
		t.Fatal(err)
	}
	acceleratedResp, err := store.Resolve(ctx, image, platform, accelerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if defaultResp.SOCIIndex.Digest != defaultIndex {
		t.Fatalf("default resolve digest = %s, want %s", defaultResp.SOCIIndex.Digest, defaultIndex)
	}
	if acceleratedResp.SOCIIndex.Digest != acceleratedIndex {
		t.Fatalf("accelerated resolve digest = %s, want %s", acceleratedResp.SOCIIndex.Digest, acceleratedIndex)
	}
	if acceleratedResp.SOCIIndex.Digest == defaultResp.SOCIIndex.Digest {
		t.Fatalf("default and accelerated profiles resolved to the same index")
	}
	if ready, err := store.HasReady(ctx, image, platform, ""); err != nil || !ready {
		t.Fatalf("default ready = %v, err = %v", ready, err)
	}
	if ready, err := store.HasReady(ctx, image, platform, accelerationKey); err != nil || !ready {
		t.Fatalf("accelerated ready = %v, err = %v", ready, err)
	}
}

func putReadyArtifactForTest(t *testing.T, ctx context.Context, store *Store, image, platform, accelerationKey, indexPayload string) digest.Digest {
	t.Helper()

	indexBytes := []byte(indexPayload)
	indexDesc := ocispec.Descriptor{
		MediaType: "application/vnd.test.index",
		Digest:    digest.FromBytes(indexBytes),
		Size:      int64(len(indexBytes)),
	}
	ztocBytes := []byte(indexPayload + "-ztoc")
	ztocDesc := ocispec.Descriptor{
		MediaType: "application/vnd.test.ztoc",
		Digest:    digest.FromBytes(ztocBytes),
		Size:      int64(len(ztocBytes)),
	}
	artifact := Artifact{
		SourceImageRef:      image,
		ImageDigestRef:      image,
		ImageManifestDigest: "sha256:manifest",
		ImageConfigDigest:   digest.FromString(indexPayload + "-config").String(),
		Platform:            platform,
	}
	layers := []LayerArtifact{{
		LayerDigest: digest.FromString(indexPayload + "-layer").String(),
		ZtocDigest:  ztocDesc.Digest.String(),
		ZtocSize:    ztocDesc.Size,
	}}
	if err := store.PutReady(ctx, artifact, indexDesc, indexBytes, []ocispec.Descriptor{ztocDesc}, map[string][]byte{ztocDesc.Digest.String(): ztocBytes}, layers, Config{SpanSize: 1, MinLayerSize: 1}, accelerationKey); err != nil {
		t.Fatal(err)
	}
	return indexDesc.Digest
}
