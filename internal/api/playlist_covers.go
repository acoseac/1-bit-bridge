package api

import (
	"context"
	"net/http"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// PlaylistCoverStore resolves operator-uploaded cover hashes for the smart-mix
// + playlist wire DTOs (the `imageHash` advertisement). Nil-safe — when
// unwired, no imageHash is advertised and iOS uses the auto-mosaic.
// *manifest.Store satisfies it.
type PlaylistCoverStore interface {
	PlaylistCoversByScope(ctx context.Context, scope string) (map[string]manifest.PlaylistCover, error)
	DeletePlaylistCover(ctx context.Context, scope, key string) (hash, ext string, ok bool, err error)
}

// WithPlaylistCoverStore wires custom-cover resolution. Returns the receiver.
func (s *Server) WithPlaylistCoverStore(st PlaylistCoverStore) *Server {
	s.coverStore = st
	return s
}

// coverHashesForScope returns key→imageHash for a scope. Best-effort: a nil
// store or a lookup error yields nil (a DTO build must never fail on cover
// advertisement — the cover is decorative, the mosaic is the fallback).
func (s *Server) coverHashesForScope(ctx context.Context, scope string) map[string]string {
	if s.coverStore == nil {
		return nil
	}
	m, err := s.coverStore.PlaylistCoversByScope(ctx, scope)
	if err != nil {
		logger.Warn("cover hashes lookup", "scope", scope, "err", err)
		return nil
	}
	out := make(map[string]string, len(m))
	for k, c := range m {
		out[k] = c.ImageHash
	}
	return out
}

// pruneCover removes a cover mapping + its on-disk JPEG (orphan cleanup on
// playlist delete). Best-effort — never fails the caller's operation.
func (s *Server) pruneCover(ctx context.Context, scope, key string) {
	if s.coverStore == nil {
		return
	}
	_, ext, ok, err := s.coverStore.DeletePlaylistCover(ctx, scope, key)
	if err != nil {
		logger.Warn("prune cover mapping", "scope", scope, "err", err)
		return
	}
	if ok {
		if cfg := s.cfgHolder.Load(); cfg != nil {
			if rmErr := os.Remove(manifest.PlaylistCoverPath(cfg.DataDir, scope, key, ext)); rmErr != nil && !os.IsNotExist(rmErr) {
				logger.Warn("prune cover unlink", "scope", scope, "err", rmErr)
			}
		}
	}
}

// smartMixCover handles GET /v1/smart-playlist-image/{slug}.
func (s *Server) smartMixCover(w http.ResponseWriter, r *http.Request) {
	s.serveCover(w, r, manifest.CoverScopeSmartMix, r.PathValue("slug"))
}

// playlistCover handles GET /v1/playlist-image/{id}.
func (s *Server) playlistCover(w http.ResponseWriter, r *http.Request) {
	s.serveCover(w, r, manifest.CoverScopePlaylist, r.PathValue("id"))
}

// serveCover serves an operator-uploaded cover JPEG, or 404 when none exists.
// An optional `?h=<hash>` query param is a client cache-buster — a re-upload
// changes the advertised imageHash, so iOS fetches a new URL and gets a clean
// cache miss; the handler serves the current file regardless. Covers are
// always JPEG (uploads are normalized on the admin side). Bearer-authed via
// the route wrapper, mirroring /v1/artwork.
func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, scope, key string) {
	if key == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing key")
		return
	}
	cfg := s.cfgHolder.Load()
	if cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress", "cover service not ready")
		return
	}
	path := manifest.PlaylistCoverPath(cfg.DataDir, scope, key, "jpg")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "no custom cover for this item")
			return
		}
		logger.Error("open playlist cover", "scope", scope, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat playlist cover", "scope", scope, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
