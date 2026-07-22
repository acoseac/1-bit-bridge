package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// GET /v1/booklet/{mbid} — the PDF album booklet for a release (keyed by
// the release MusicBrainz GID that tracks carry as musicBrainzAlbumID; the
// manifest advertises availability per-track via `bookletTag`). Serves the
// locally-cached file the atlasharvest booklet loop downloaded; the bridge
// never proxies the fetch inline (same offline-first posture as artwork).
// Additive endpoint — ProtocolVersion stays at 1; iOS gates on the
// "booklets" health feature flag. No provenance is exposed anywhere.

// BookletStore is the manifest surface the handler needs ("is this MBID a
// known booklet, and has it been fetched"). *manifest.Store satisfies it.
type BookletStore interface {
	GetBooklet(ctx context.Context, mbid string) (*manifest.BookletRow, error)
}

// WithBooklets wires the booklet-serving surface: the availability rows,
// the on-disk PDF cache dir, and an optional fetch-priority nudge (the 202
// path calls it so a booklet the user just tapped jumps the background
// download queue). Also flips the "booklets" health feature flag.
func (s *Server) WithBooklets(store BookletStore, dir string, nudge func(mbid string)) *Server {
	s.bookletStore = store
	s.bookletDir = dir
	s.bookletNudge = nudge
	return s
}

// BookletPath returns the canonical on-disk cache path for a release's
// booklet. Shared with cmd/bridge's disk adapter so the writer and the
// server can't disagree about the layout. The caller MUST have validated
// mbid (strict UUID) — the [a-f0-9-] alphabet makes traversal impossible.
func BookletPath(dir, mbid string) string {
	return filepath.Join(dir, mbid+".pdf")
}

// IsValidBookletMBID reports whether mbid is a strict MusicBrainz UUID,
// using the same anchored pattern the GET handler enforces. Exported so the
// WRITE side (cmd/bridge's disk adapter) can enforce the same rule.
//
// Load-bearing because `mbid` is the LEADING component of BookletPath's
// filepath.Join, and the writer's atomicwrite.WriteBytes does
// os.MkdirAll(filepath.Dir(path)) — so a traversing value would CREATE its
// own parent directories rather than failing. The read handler validated;
// the write path did not, and both this file's docblock and the adapter's
// asserted a validation that was never implemented anywhere in the harvest
// chain (2026-07-20 review, F29).
func IsValidBookletMBID(mbid string) bool { return mbidPattern.MatchString(mbid) }

// booklet handles GET /v1/booklet/{mbid}.
//
// 200 → application/pdf via http.ServeContent (Range supported — PDFKit
// benefits); 202 + Retry-After → known + available but the background
// fetch hasn't landed it yet (mirrors the /v1/artwork pending contract);
// 404 → unknown release or no booklet exists for it.
func (s *Server) booklet(w http.ResponseWriter, r *http.Request) {
	if s.bookletStore == nil || s.bookletDir == "" {
		writeError(w, http.StatusNotFound, "not_found", "booklets not enabled on this bridge")
		return
	}
	mbid := r.PathValue("mbid")
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request", "mbid must be a MusicBrainz UUID")
		return
	}
	row, err := s.bookletStore.GetBooklet(r.Context(), mbid)
	if err != nil {
		logger.Error("booklet lookup", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	if row == nil || !row.Available {
		writeError(w, http.StatusNotFound, "not_found", "no booklet for release")
		return
	}
	f, err := os.Open(BookletPath(s.bookletDir, mbid))
	if err != nil {
		if os.IsNotExist(err) {
			// Known + available but not downloaded yet: nudge the fetch
			// sweep so the tapped album jumps the queue, and tell iOS to
			// retry — same 202 semantics as pending artwork.
			if s.bookletNudge != nil {
				s.bookletNudge(mbid)
			}
			w.Header().Set("Retry-After", strconv.Itoa(artworkPendingRetryAfterSeconds))
			writeError(w, http.StatusAccepted, "pending",
				"booklet download pending; retry after the Retry-After window")
			return
		}
		logger.Error("open booklet", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat booklet", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="booklet-`+mbid+`.pdf"`)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
