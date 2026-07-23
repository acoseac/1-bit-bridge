// Admin Library Inspector search endpoint (v1.4 PR B — FTS5).
//
// One surface: GET /api/library/search?q=&limit=
//   - Min 2 chars after trim (matches the JS-side gate).
//   - `limit` default 50, capped at 200.
//   - Returns `{folders: [...], tracks: [...], truncated: bool}`.
//
// Loopback-only (enforced upstream at the listener layer). Path
// validation lives in the search query sanitiser
// (`manifest.buildFTSMatchExpr`) — no traversal axis here.
//
// Failure modes:
//   - FTS5 not compiled in (rare; only on minimal modernc.org/sqlite
//     builds) → 503 with the typed `search-disabled` error code.
//   - Empty `q` after sanitisation → 200 with empty arrays. We don't
//     promote this to a 400 because the JS-side debounce can fire a
//     fetch while the user is still typing punctuation-only input;
//     surfacing it as an error would flicker the dropdown's status
//     line on every keystroke.

package admin

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// --- response shapes ---

type searchTrackHit struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ParentPath string `json:"parentPath"`
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Album      string `json:"album,omitempty"`
}

type searchFolderHit struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	ParentPath string `json:"parentPath"`
	HitCount   int    `json:"hitCount"`
}

type librarySearchResponse struct {
	Folders   []searchFolderHit `json:"folders"`
	Tracks    []searchTrackHit  `json:"tracks"`
	Truncated bool              `json:"truncated"`
}

// --- handler ---

// apiLibrarySearch handles GET /api/library/search?q=&limit=.
//
// Per-PR-B-plan: server-side fan-out hits the FTS5 virtual table
// the v7 manifest migration installs. The UI calls this only when
// the current-folder client-side filter produced zero matches AND
// the query is ≥ 2 chars — so the server-side cost is bounded to
// the actually-novel-query case.
func (s *Server) apiLibrarySearch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "no-manifest",
			"manifest store is not configured")
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	// Trim THEN min-2 check. Pure-whitespace input would otherwise
	// hit the FTS5 sanitiser and surface as a no-result 200 — same
	// outcome but pointlessly burns a SQL probe.
	//
	// Count RUNES, not bytes: the limit exists to keep one-character
	// queries off the FTS index, and len() would let a single CJK or
	// accented character (2-4 bytes in UTF-8) straight through while
	// rejecting a genuine 1-char ASCII query. Byte length also makes the
	// user-facing "at least 2 characters" message untrue for those
	// alphabets.
	if utf8.RuneCountInString(raw) < 2 {
		writeError(w, http.StatusBadRequest, "query-too-short",
			"query must be at least 2 characters")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}

	tracks, err := s.deps.Manifest.SearchTracks(r.Context(), raw, limit)
	if err != nil {
		if errors.Is(err, manifest.ErrSearchUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "search-disabled",
				"library search is not available on this bridge (FTS5 not compiled in)")
			return
		}
		writeError(w, http.StatusInternalServerError, "search-tracks", err.Error())
		return
	}
	// Folder rollup runs against the SAME query but with the same
	// limit — the underlying SearchFolders impl widens the inner
	// search internally to give the GROUP BY enough rows to produce
	// a full limit-sized parent set.
	folders, err := s.deps.Manifest.SearchFolders(r.Context(), raw, limit)
	if err != nil {
		if errors.Is(err, manifest.ErrSearchUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "search-disabled",
				"library search is not available on this bridge (FTS5 not compiled in)")
			return
		}
		writeError(w, http.StatusInternalServerError, "search-folders", err.Error())
		return
	}

	resp := librarySearchResponse{
		Folders:   make([]searchFolderHit, 0, len(folders)),
		Tracks:    make([]searchTrackHit, 0, len(tracks)),
		Truncated: len(tracks) >= limit, // best-effort hint; over-counts on exact-limit boundary
	}
	for _, f := range folders {
		name := f.Name
		if name == "" || name == "." {
			name = "Library root"
		}
		resp.Folders = append(resp.Folders, searchFolderHit{
			Name:       name,
			Path:       f.Path,
			ParentPath: parentPath(f.Path),
			HitCount:   f.HitCount,
		})
	}
	for _, t := range tracks {
		resp.Tracks = append(resp.Tracks, searchTrackHit{
			Name:       path.Base(t.Path),
			Path:       t.Path,
			ParentPath: t.ParentPath,
			Title:      t.Title,
			Artist:     t.Artist,
			Album:      t.Album,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// parentPath returns the directory component of a library-relative
// path, mapping the root case (`.`) back to the empty string the
// inspector navigation uses as its "library root" sentinel.
func parentPath(p string) string {
	if p == "" {
		return ""
	}
	parent := path.Dir(p)
	if parent == "." {
		return ""
	}
	return parent
}
