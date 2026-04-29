package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
)

// decodeOptionalJSONBody decodes a JSON body into `dst` when one
// is present, treats an empty body as "no fields supplied", and
// surfaces a typed BadRequest on malformed JSON. Replaces the
// `r.ContentLength > 0` guard Gemini flagged on PR #45 — chunked
// requests and transports without a Content-Length header report
// 0 or -1 even when a body is genuinely present, so the old guard
// could silently drop intended fields.
func decodeOptionalJSONBody(w http.ResponseWriter, r *http.Request, dst any) (ok bool) {
	if r.Body == nil {
		return true
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	writeError(w, http.StatusBadRequest, "bad-json", err.Error())
	return false
}

// --- POST /api/tokens/{id}/rotate ---
//
// Replaces the raw bytes of an existing token. ID, Name, CreatedAt
// stay; Hash + RotatedAt change; previous raw stops validating
// immediately. Returns the new raw token + a fresh pair URL/QR
// suitable for the device-holder to re-scan.
//
// The shape mirrors `apiTokensMint` so the admin JS can reuse the
// post-mint UI flow (display raw, render QR, wait for the user to
// confirm they pasted/scanned). Rotation is idempotent in terms of
// row identity but produces a fresh secret on every call.
func (s *Server) apiTokensRotate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Optional body for a re-pair URL override — mirrors apiTokensMint.
	// Empty / absent body falls back to the operator-default URL the
	// listener advertises.
	var req struct {
		URL string `json:"url"`
	}
	if !decodeOptionalJSONBody(w, r, &req) {
		return
	}
	if req.URL == "" {
		req.URL = defaultBridgeURL(s.deps.Cfg.ListenAddress)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rawToken, tok, err := s.deps.Auth.Rotate(id)
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown-token", id)
			return
		}
		writeError(w, http.StatusInternalServerError, "rotate", err.Error())
		return
	}

	alternates := pairAlternates(req.URL, s.deps.Cfg.ListenAddress)
	pairURL := buildPairURL(req.URL, rawToken, s.deps.Fingerprint, s.deps.Cfg.LibraryName, alternates)
	qrData, err := qrDataURL(pairURL)
	if err != nil {
		// QR render failures don't block the rotate — operator can
		// still hand the raw token over manually.
		qrData = ""
	}
	writeJSON(w, http.StatusOK, pairResult{
		RawToken:    rawToken,
		ID:          tok.ID,
		Name:        tok.Name,
		Fingerprint: s.deps.Fingerprint,
		URL:         req.URL,
		PairURL:     pairURL,
		Alternates:  alternates,
		QRDataURL:   qrData,
	})
}

// --- PATCH /api/tokens/{id} ---
//
// Updates lifecycle fields on a token without invalidating its raw
// bytes. Currently the only mutable field is `expiresAt`:
//
//	{ "expiresAt": "2026-12-31T23:59:00Z" }   // set
//	{ "expiresAt": null }                      // clear
//
// Other fields are reserved for future lifecycle work (e.g. a
// human-editable Name). A request with no recognized field is a
// no-op and returns 200 with the unchanged row — surfaces a clean
// "request acknowledged" rather than treating it as an error.
//
// Mutations go through the admin Server's mutex so a concurrent
// mint / revoke can't interleave a half-applied edit.
func (s *Server) apiTokensSetLifecycle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// `*string` for expiresAt because the JSON null vs absent
	// distinction matters: null = clear; absent = no change. A
	// pointer to the parsed time would lose the absent-vs-null
	// signal post-decode.
	var req struct {
		ExpiresAt json.RawMessage `json:"expiresAt"`
	}
	if !decodeOptionalJSONBody(w, r, &req) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(req.ExpiresAt) > 0 {
		var exp *time.Time
		if string(req.ExpiresAt) != "null" {
			var t time.Time
			if err := json.Unmarshal(req.ExpiresAt, &t); err != nil {
				writeError(w, http.StatusBadRequest, "bad-expires-at",
					"expiresAt must be RFC3339 timestamp or null: "+err.Error())
				return
			}
			exp = &t
		}
		tok, err := s.deps.Auth.SetExpiry(id, exp)
		if err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				writeError(w, http.StatusNotFound, "unknown-token", id)
				return
			}
			writeError(w, http.StatusInternalServerError, "set-expiry", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tokenRow{
			ID:         tok.ID,
			Name:       tok.Name,
			CreatedAt:  tok.CreatedAt,
			LastUsedAt: tok.LastUsedAt,
			RotatedAt:  tok.RotatedAt,
			ExpiresAt:  tok.ExpiresAt,
		})
		return
	}

	// No recognized fields — no-op; return current row state.
	// `Get` is an O(N) lookup like `List`+filter, but doesn't
	// allocate the slice copy for every other token in the store.
	t, err := s.deps.Auth.Get(id)
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown-token", id)
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokenRow{
		ID:         t.ID,
		Name:       t.Name,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RotatedAt:  t.RotatedAt,
		ExpiresAt:  t.ExpiresAt,
	})
}
