package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

type Server struct {
	cfg   Config
	store *Store
}

func NewServer(cfg Config, store *Store) *Server {
	return &Server{cfg: cfg, store: store}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/indexes/resolve", s.handleResolve)
	mux.HandleFunc("/v1/blobs/", s.handleBlob)
	mux.HandleFunc("/v1/artifacts", s.handleArtifacts)

	server := &http.Server{Addr: s.cfg.ListenAddr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	log.Printf("artifact gateway listening on %s", s.cfg.ListenAddr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	image := r.URL.Query().Get("image")
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		platform = s.cfg.Platform
	}
	if image == "" {
		writeError(w, http.StatusBadRequest, "missing image")
		return
	}
	accelerationKey := buildAccelerationForImageRef(image).Key()
	resp, err := s.store.Resolve(r.Context(), image, platform, accelerationKey)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("index not ready for image=%s platform=%s", image, platform))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	dgst := strings.TrimPrefix(r.URL.Path, "/v1/blobs/")
	if dgst == "" {
		writeError(w, http.StatusBadRequest, "missing digest")
		return
	}
	mediaType, size, content, err := s.store.GetBlob(r.Context(), dgst)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("blob %s not found", dgst))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListRecent(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
