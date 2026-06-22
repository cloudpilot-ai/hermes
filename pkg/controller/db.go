package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const imageArtifactsTableSQL = `CREATE TABLE image_artifacts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_image_ref TEXT NOT NULL,
	image_digest_ref TEXT NOT NULL,
	image_manifest_digest TEXT NOT NULL,
	image_config_digest TEXT NOT NULL DEFAULT '',
	platform TEXT NOT NULL,
	acceleration_key TEXT NOT NULL DEFAULT '',
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
	UNIQUE(image_digest_ref, platform, acceleration_key)
)`

const artifactLayersTableSQL = `CREATE TABLE artifact_layers (
	image_artifact_id INTEGER NOT NULL,
	layer_digest TEXT NOT NULL,
	ztoc_digest TEXT NOT NULL,
	ztoc_size INTEGER NOT NULL,
	PRIMARY KEY(image_artifact_id, layer_digest),
	FOREIGN KEY(image_artifact_id) REFERENCES image_artifacts(id) ON DELETE CASCADE
)`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`PRAGMA foreign_keys = ON;`,
		`CREATE TABLE IF NOT EXISTS ` + strings.TrimPrefix(imageArtifactsTableSQL, "CREATE TABLE "),
		`CREATE TABLE IF NOT EXISTS artifact_blobs (
			digest TEXT PRIMARY KEY,
			media_type TEXT NOT NULL,
			size INTEGER NOT NULL,
			content BLOB NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS ` + strings.TrimPrefix(artifactLayersTableSQL, "CREATE TABLE "),
		`CREATE INDEX IF NOT EXISTS image_artifacts_status_idx ON image_artifacts(build_status);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MarkBuilding(ctx context.Context, task BuildTask, imageDigestRef, manifestDigest string, cfg Config, accelerationKey string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `INSERT INTO image_artifacts (
		source_image_ref, image_digest_ref, image_manifest_digest, platform, acceleration_key,
		span_size, min_layer_size, build_status, build_started_at, error, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?)
		ON CONFLICT(image_digest_ref, platform, acceleration_key) DO UPDATE SET
			source_image_ref=excluded.source_image_ref,
			image_manifest_digest=excluded.image_manifest_digest,
			span_size=excluded.span_size,
			min_layer_size=excluded.min_layer_size,
			build_status=excluded.build_status,
			build_started_at=excluded.build_started_at,
			error='',
			updated_at=excluded.updated_at
		WHERE image_artifacts.build_status != ?`,
		task.SourceImageRef, imageDigestRef, manifestDigest, task.Platform,
		accelerationKey, cfg.SpanSize, cfg.MinLayerSize, statusBuilding, now, now, statusReady)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return true, nil
	}
	return rows > 0, nil
}

func (s *Store) MarkFailed(ctx context.Context, sourceImageRef, imageDigestRef, platform, accelerationKey string, buildErr error) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	errText := ""
	if buildErr != nil {
		errText = buildErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO image_artifacts (
		source_image_ref, image_digest_ref, image_manifest_digest, platform, acceleration_key,
		build_status, build_finished_at, error, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(image_digest_ref, platform, acceleration_key) DO UPDATE SET
		source_image_ref=excluded.source_image_ref,
		build_status=excluded.build_status,
		build_finished_at=excluded.build_finished_at,
		error=excluded.error,
		updated_at=excluded.updated_at`,
		sourceImageRef, imageDigestRef, "", platform, accelerationKey, statusFailed, now, errText, now)
	return err
}

func (s *Store) PutReady(ctx context.Context, artifact Artifact, indexDesc ocispec.Descriptor, indexBytes []byte, ztocs []ocispec.Descriptor, ztocBytes map[string][]byte, layers []LayerArtifact, cfg Config, accelerationKey string) error {
	if err := verifyBlob(indexDesc.Digest, indexBytes); err != nil {
		return fmt.Errorf("index digest verification failed: %w", err)
	}
	for _, z := range ztocs {
		if err := verifyBlob(z.Digest, ztocBytes[z.Digest.String()]); err != nil {
			return fmt.Errorf("ztoc %s digest verification failed: %w", z.Digest, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `INSERT INTO image_artifacts (
		source_image_ref, image_digest_ref, image_manifest_digest, image_config_digest,
		platform, acceleration_key, index_digest, index_media_type, index_size, soci_version,
		span_size, min_layer_size, build_status, build_finished_at, error, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'v1', ?, ?, ?, ?, '', ?)
	ON CONFLICT(image_digest_ref, platform, acceleration_key) DO UPDATE SET
		source_image_ref=excluded.source_image_ref,
		image_manifest_digest=excluded.image_manifest_digest,
		image_config_digest=excluded.image_config_digest,
		index_digest=excluded.index_digest,
		index_media_type=excluded.index_media_type,
		index_size=excluded.index_size,
		soci_version=excluded.soci_version,
		span_size=excluded.span_size,
		min_layer_size=excluded.min_layer_size,
		build_status=excluded.build_status,
		build_finished_at=excluded.build_finished_at,
		error='',
		updated_at=excluded.updated_at`,
		artifact.SourceImageRef, artifact.ImageDigestRef, artifact.ImageManifestDigest, artifact.ImageConfigDigest,
		artifact.Platform, accelerationKey, indexDesc.Digest.String(), indexDesc.MediaType, indexDesc.Size,
		cfg.SpanSize, cfg.MinLayerSize, statusReady, now, now)
	if err != nil {
		return err
	}

	_ = res

	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM image_artifacts WHERE image_digest_ref = ? AND platform = ? AND acceleration_key = ?`,
		artifact.ImageDigestRef, artifact.Platform, accelerationKey).Scan(&id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO artifact_blobs(digest, media_type, size, content, created_at) VALUES (?, ?, ?, ?, ?)`,
		indexDesc.Digest.String(), indexDesc.MediaType, int64(len(indexBytes)), indexBytes, now); err != nil {
		return err
	}
	for _, z := range ztocs {
		b := ztocBytes[z.Digest.String()]
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO artifact_blobs(digest, media_type, size, content, created_at) VALUES (?, ?, ?, ?, ?)`,
			z.Digest.String(), z.MediaType, int64(len(b)), b, now); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_layers WHERE image_artifact_id = ?`, id); err != nil {
		return err
	}
	for _, l := range layers {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO artifact_layers(image_artifact_id, layer_digest, ztoc_digest, ztoc_size) VALUES (?, ?, ?, ?)`,
			id, l.LayerDigest, l.ZtocDigest, l.ZtocSize); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) Resolve(ctx context.Context, imageDigestRef, platform, accelerationKey string) (*ResolveResponse, error) {
	var a Artifact
	err := s.db.QueryRowContext(ctx, `SELECT id, source_image_ref, image_digest_ref, image_manifest_digest, image_config_digest,
		platform, acceleration_key, index_digest, index_media_type, index_size, build_status, error
		FROM image_artifacts
		WHERE image_digest_ref = ? AND platform = ? AND build_status = ? AND acceleration_key = ?`,
		imageDigestRef, platform, statusReady, accelerationKey).Scan(
		&a.ID, &a.SourceImageRef, &a.ImageDigestRef, &a.ImageManifestDigest, &a.ImageConfigDigest,
		&a.Platform, &a.AccelerationKey, &a.IndexDigest, &a.IndexMediaType, &a.IndexSize, &a.Status, &a.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT layer_digest, ztoc_digest, ztoc_size FROM artifact_layers WHERE image_artifact_id = ? ORDER BY layer_digest`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ztocs []ZtocResponse
	for rows.Next() {
		var z ZtocResponse
		if err := rows.Scan(&z.LayerDigest, &z.ZtocDigest, &z.Size); err != nil {
			return nil, err
		}
		ztocs = append(ztocs, z)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	d, err := digest.Parse(a.IndexDigest)
	if err != nil {
		return nil, err
	}

	return &ResolveResponse{
		Image:    a.ImageDigestRef,
		Platform: a.Platform,
		SOCIIndex: ocispec.Descriptor{
			MediaType: a.IndexMediaType,
			Digest:    d,
			Size:      a.IndexSize,
		},
		Ztocs: ztocs,
	}, nil
}

func (s *Store) HasReady(ctx context.Context, imageDigestRef, platform, accelerationKey string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1
		FROM image_artifacts
		WHERE image_digest_ref = ? AND platform = ? AND build_status = ? AND acceleration_key = ?
		LIMIT 1`, imageDigestRef, platform, statusReady, accelerationKey).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) GetBlob(ctx context.Context, dgst string) (mediaType string, size int64, content []byte, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT media_type, size, content FROM artifact_blobs WHERE digest = ?`, dgst).Scan(&mediaType, &size, &content)
	return mediaType, size, content, err
}

func (s *Store) ListRecent(ctx context.Context, limit int) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_image_ref, image_digest_ref, image_manifest_digest, image_config_digest,
		platform, acceleration_key, index_digest, index_media_type, index_size, build_status, error
		FROM image_artifacts ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Artifact, 0)
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.SourceImageRef, &a.ImageDigestRef, &a.ImageManifestDigest, &a.ImageConfigDigest,
			&a.Platform, &a.AccelerationKey, &a.IndexDigest, &a.IndexMediaType, &a.IndexSize, &a.Status, &a.Error); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func verifyBlob(dgst digest.Digest, b []byte) error {
	if len(b) == 0 {
		return io.ErrUnexpectedEOF
	}
	if got := digest.FromBytes(b); got != dgst {
		return fmt.Errorf("got %s, want %s", got, dgst)
	}
	return nil
}
