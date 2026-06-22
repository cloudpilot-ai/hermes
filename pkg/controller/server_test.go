package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
)

func TestResolveFallsBackToDefaultArtifact(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	image := "ghcr.io/acme/example@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	platform := "linux/amd64"
	putReadyArtifactForTest(t, ctx, store, image, platform, "", "default-index")

	server := NewServer(Config{Platform: platform}, store)
	req := httptest.NewRequest(http.MethodGet, "/v1/indexes/resolve?image="+url.QueryEscape(image), nil)
	rec := httptest.NewRecorder()

	server.handleResolve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
