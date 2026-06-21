package controller

import (
	"context"
	"database/sql"
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

	image := "docker.io/opensearchproject/opensearch@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	platform := "linux/amd64"
	accelerationKey := BuildAcceleration{Profile: buildProfileOpenSearchJVM}.Key()

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

func TestStoreMigratesOldAccelerationIdentity(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "artifacts.db")
	image := "docker.io/library/busybox@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	platform := "linux/amd64"
	oldIndex := digest.FromBytes([]byte("old-index"))
	oldZtoc := digest.FromBytes([]byte("old-ztoc"))

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE image_artifacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_image_ref TEXT NOT NULL,
			image_digest_ref TEXT NOT NULL,
			image_manifest_digest TEXT NOT NULL,
			image_config_digest TEXT NOT NULL DEFAULT '',
			platform TEXT NOT NULL,
			index_digest TEXT NOT NULL DEFAULT '',
			index_media_type TEXT NOT NULL DEFAULT '',
			index_size INTEGER NOT NULL DEFAULT 0,
			soci_version TEXT NOT NULL DEFAULT 'v1',
			span_size INTEGER NOT NULL DEFAULT 0,
			min_layer_size INTEGER NOT NULL DEFAULT 0,
			build_status TEXT NOT NULL,
			build_started_at TEXT,
			build_finished_at TEXT,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(image_digest_ref, platform)
		)`,
		artifactLayersTableSQL,
		`INSERT INTO image_artifacts (
			id, source_image_ref, image_digest_ref, image_manifest_digest, platform,
			index_digest, index_media_type, index_size, build_status
		) VALUES (7, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`INSERT INTO artifact_layers(image_artifact_id, layer_digest, ztoc_digest, ztoc_size)
			VALUES (7, ?, ?, ?)`,
	} {
		var execErr error
		switch stmt {
		case `INSERT INTO image_artifacts (
			id, source_image_ref, image_digest_ref, image_manifest_digest, platform,
			index_digest, index_media_type, index_size, build_status
		) VALUES (7, ?, ?, ?, ?, ?, ?, ?, ?)`:
			_, execErr = db.ExecContext(ctx, stmt, image, image, "sha256:manifest", platform, oldIndex.String(), "application/vnd.test.index", int64(len("old-index")), statusReady)
		case `INSERT INTO artifact_layers(image_artifact_id, layer_digest, ztoc_digest, ztoc_size)
			VALUES (7, ?, ?, ?)`:
			_, execErr = db.ExecContext(ctx, stmt, digest.FromString("old-layer").String(), oldZtoc.String(), int64(len("old-ztoc")))
		default:
			_, execErr = db.ExecContext(ctx, stmt)
		}
		if execErr != nil {
			_ = db.Close()
			t.Fatal(execErr)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	resp, err := store.Resolve(ctx, image, platform, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.SOCIIndex.Digest != oldIndex {
		t.Fatalf("migrated resolve digest = %s, want %s", resp.SOCIIndex.Digest, oldIndex)
	}
	if len(resp.Ztocs) != 1 || resp.Ztocs[0].ZtocDigest != oldZtoc.String() {
		t.Fatalf("migrated ztocs = %#v", resp.Ztocs)
	}

	accelerationKey := BuildAcceleration{Profile: buildProfileOpenSearchJVM}.Key()
	newIndex := putReadyArtifactForTest(t, ctx, store, image, platform, accelerationKey, "new-index")
	acceleratedResp, err := store.Resolve(ctx, image, platform, accelerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if acceleratedResp.SOCIIndex.Digest != newIndex {
		t.Fatalf("accelerated resolve digest = %s, want %s", acceleratedResp.SOCIIndex.Digest, newIndex)
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
