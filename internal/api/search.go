package api

// GET /v1/search — server-side library search.
//
// The FTS5 index has existed since migration 7 and had three consumers —
// the admin player, admin library search, and the DLNA ContentDirectory
// adapter — but none on /v1, so a client could only search what it had
// already synced. This puts it on the wire.
//
// **It serves the SERVED set.** `tracks_fts` is trigger-populated from
// `tracks`, so it contains duplicate-suppressed rows that /v1/manifest
// deliberately withholds; the store's SearchServedTracks joins them out.
// Serving the raw index here would contradict every count beside it.
//
// The response deliberately carries PATHS plus display context, not full
// Track objects: the client already holds the manifest, `path` is the
// join key, and duplicating the manifest's schema on a second surface
// would mean two places to keep a wire contract in step.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// searchMinQueryRunes is the shortest query accepted. Counted in RUNES,
// not bytes: the limit exists to keep one-character queries off the FTS
// index, and a byte length would let a single CJK or accented character
// through while rejecting a genuine one-character ASCII query — and would
// make the error message untrue for those alphabets. Same reasoning as
// the admin handler's.
const searchMinQueryRunes = 2

// searchDefaultLimit / searchMaxLimit bound the page. Lower than the
// admin surface's 200 because this one is called per keystroke over a
// possibly-relayed link; the store's own searchHardCap is the backstop.
const (
	searchDefaultLimit = 25
	searchMaxLimit     = 100
)

// ServedSearcher is the optional capability /v1/search needs. Declared as
// a narrow interface the ManifestProvider may also satisfy, rather than
// widened into ManifestProvider itself, so the several test doubles that
// implement that interface do not all have to grow a search method they
// do not use — and so a provider genuinely without search degrades to a
// clean 503 rather than failing to compile.
type ServedSearcher interface {
	SearchServedTracks(ctx context.Context, query string, limit int) ([]manifest.TrackHit, error)
}

// searchTrackHit is the wire DTO. A DTO rather than manifest.TrackHit
// straight from the handler, per the wire-type discipline: a future
// column rename in the store must not silently reshape the protocol.
type searchTrackHit struct {
	Path   string `json:"path"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
}

// searchResponse is the /v1/search body.
type searchResponse struct {
	Tracks []searchTrackHit `json:"tracks"`
	// Truncated is a best-effort hint that more hits exist. It
	// over-reports on an exact-limit boundary, which is the honest cost
	// of not running a second COUNT query per keystroke.
	Truncated bool `json:"truncated"`
}

// search handles GET /v1/search?q=&limit=.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if s.manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "search_unavailable",
			"this bridge has no manifest store")
		return
	}
	searcher, ok := s.manifest.(ServedSearcher)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "search_unavailable",
			"this bridge's manifest provider does not support search")
		return
	}

	q := safeQuery(r).Get("q")
	if utf8.RuneCountInString(q) < searchMinQueryRunes {
		writeError(w, http.StatusBadRequest, "query_too_short",
			"q must be at least 2 characters")
		return
	}

	limit := searchDefaultLimit
	if v := safeQuery(r).Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer")
			return
		}
		if n > searchMaxLimit {
			n = searchMaxLimit
		}
		limit = n
	}

	hits, err := searcher.SearchServedTracks(r.Context(), q, limit)
	if err != nil {
		// A CANCELLED request is the normal case here, not a fault. This
		// endpoint is called per keystroke, so a client that keeps typing
		// abandons the request in flight — reporting that as 500 would
		// bury real failures under noise in the logs and in the error
		// metrics, and the client is gone anyway so nothing reads the
		// body. Return silently. (Gemini HIGH.)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, manifest.ErrSearchUnavailable) {
			// Terminal for this bridge, not transient: the FTS5 module is
			// compiled in or it is not, for the process lifetime. The
			// `search` feature flag is gated on the same fact, so a
			// client that reads the flag never gets here.
			writeError(w, http.StatusServiceUnavailable, "search_unavailable",
				"library search is not available on this bridge (FTS5 not compiled in)")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := searchResponse{
		Tracks:    make([]searchTrackHit, 0, len(hits)),
		Truncated: len(hits) >= limit,
	}
	for _, h := range hits {
		resp.Tracks = append(resp.Tracks, searchTrackHit{
			Path: h.Path, Title: h.Title, Artist: h.Artist, Album: h.Album,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// searchAvailable reports whether GET /v1/search can serve. It is a
// RUNTIME fact — the FTS5 module is compiled into the SQLite driver or it
// is not, for the process lifetime — so it is probed through the store
// rather than read from config, and the health flag and the endpoint gate
// on the SAME answer so the two cannot disagree.
func (s *Server) searchAvailable() bool {
	if s.manifest == nil {
		return false
	}
	probe, ok := s.manifest.(interface {
		SearchAvailable(ctx context.Context) (bool, error)
	})
	if !ok {
		return false
	}
	avail, err := probe.SearchAvailable(context.Background())
	return err == nil && avail
}
