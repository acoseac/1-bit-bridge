package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// Entry is the JSON shape of a single directory entry returned by /v1/list.
// The path field is library-root-relative, not the server's absolute path.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// StatResponse is the JSON shape returned by /v1/stat.
type StatResponse struct {
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// list handles GET /v1/list?path=<rel>. Returns the entries of the resolved
// directory. Entries are sorted by name (case-insensitive) for stable client
// rendering. In multi-root mode an empty path returns synthetic top-level
// entries — one per configured root, keyed by basename — so iOS can
// enumerate roots the same way SMB enumerates shares.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	clientPath := r.URL.Query().Get("path")
	roots := s.resolver.Roots()
	if len(roots) > 1 && (clientPath == "" || clientPath == "/") {
		entries := make([]Entry, 0, len(roots))
		for _, root := range roots {
			base := filepath.Base(root)
			info, err := os.Stat(root)
			if err != nil {
				// A root that's currently unreachable stays visible as a
				// directory entry so iOS can render it (and surface a
				// meaningful error if the user tries to descend).
				entries = append(entries, Entry{
					Name:    base,
					Path:    base,
					IsDir:   true,
					ModTime: time.Time{},
				})
				continue
			}
			entries = append(entries, Entry{
				Name:    base,
				Path:    base,
				IsDir:   true,
				Size:    0,
				ModTime: info.ModTime().UTC(),
			})
		}
		sortEntriesByName(entries)
		writeJSON(w, http.StatusOK, entries)
		return
	}
	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a file, not a directory")
		return
	}

	dir, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer dir.Close()
	raw, err := dir.Readdir(-1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	entries := make([]Entry, 0, len(raw))
	for _, ri := range raw {
		// Skip hidden files — macOS/Windows both drop noise into every
		// directory, none of it is music.
		if len(ri.Name()) > 0 && ri.Name()[0] == '.' {
			continue
		}
		entries = append(entries, Entry{
			Name:    ri.Name(),
			Path:    childPath(clientPath, ri.Name()),
			IsDir:   ri.IsDir(),
			Size:    ri.Size(),
			ModTime: ri.ModTime().UTC(),
		})
	}
	sortEntriesByName(entries)
	writeJSON(w, http.StatusOK, entries)
}

// stat handles GET /v1/stat?path=<rel>. Returns a single-entry StatResponse.
// Works on both files and directories.
func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	clientPath := r.URL.Query().Get("path")
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	writeJSON(w, http.StatusOK, StatResponse{
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
	})
}

// read handles GET /v1/read?path=<rel>. Range header is REQUIRED per
// PROTOCOL.md — unranged reads are rejected with 400 (RFC 7233 reserves
// 416 for satisfiable-range errors, not missing-header ones). This
// endpoint is intended for tag-header windows (64–128 KB) and similar
// sub-file queries from the iOS scanner fast-path fallback. Whole-file
// reads should use /v1/download.
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") == "" {
		writeError(w, http.StatusBadRequest,
			"range_required",
			"use /v1/download for unranged reads; /v1/read requires a Range header")
		return
	}
	s.serveFile(w, r)
}

// download handles GET /v1/download?path=<rel>. Supports Range; returns the
// whole file when unranged.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r)
}

// serveFile is the shared file-body path for /v1/read and /v1/download.
//
// Wraps the entire response in the SessionTracker's Begin/End so the
// updater's install path can refuse to swap-and-restart while a
// download is in flight (Hugo 2 / XMOS DAC DoP-lock loss is the
// invariant we're protecting — see internal/updater/sessions.go for
// the rationale). Tracker is nil-safe; pre-Phase-B bridges that
// don't wire one continue to work unchanged.
//
// **Variant routing** (v1.2): if the request carries `?variant=<id>`
// in addition to `?path=<rel>`, the resolver looks up the (path,
// variantID) pair in the variant store and serves the on-disk
// sidecar instead of the original. Path validation still runs on
// the source path (so `..`-escapes can't sneak through via the
// variant route). When the variant store is unwired (feature
// disabled) OR the row is missing, we 404 — iOS's typed
// BridgeError.http(404) maps to a clean fallback to the original
// on the next playback. A row that exists but whose sidecar is
// stale (source drift) yields 410 Gone, which iOS handles
// identically to 404 modulo the error message.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if s.sessions != nil {
		s.sessions.Begin()
		defer s.sessions.End()
	}

	clientPath := r.URL.Query().Get("path")
	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, err); ok {
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return
	}

	// Variant branch: take over before the source-file open if the
	// caller asked for a variant. The source-path resolve above is
	// load-bearing — we run it first so a malformed `?path=` is
	// rejected with the standard 400 family BEFORE any variant
	// lookup happens. We pass the validated `info` through so the
	// freshness check uses the resolver's canonical stat instead
	// of any string-concatenation re-resolution downstream
	// (CodeQL alert: "uncontrolled data used in path expression"
	// + Gemini single-root regression — both go away when the
	// stat lives here, not in the manifest provider).
	if variantID := r.URL.Query().Get("variant"); variantID != "" {
		// Path normalization (collapse `//`, `.`, `..`) lives in the
		// manifest store's normalizePathForLookup so /v1/download?variant=,
		// /v1/upscale, and any other LookupTrack/LookupVariant caller
		// share one fix (Gemini on PR #147). The API layer hands over
		// the raw clientPath; the store applies path.Clean inside its
		// case-folded fallback.
		s.serveVariant(w, r, clientPath, info, variantID)
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()

	// Pre-set content-type: we treat every library file as opaque bytes —
	// the iOS side already knows what format it asked for (the manifest
	// told it). Standard binary content type keeps intermediaries from
	// transcoding anything.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	// http.ServeContent handles Range, If-Modified-Since, and the 206
	// partial-content bookkeeping for us. It also skips the body on HEAD
	// requests automatically.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// serveVariant resolves (clientPath, variantID) → on-disk sidecar
// path and streams the bytes via http.ServeContent. The
// freshness check happens here (not in the variant store) so the
// canonical `os.FileInfo` from the resolver is the source of
// truth — no duplicate path resolution, no string-concatenation
// stat. The sidecar lives under the bridge's own data dir (not
// user-controlled).
//
// `sourceInfo` MUST come from `s.resolver.ResolveChecked` upstream
// in serveFile — that's how the path-traversal guard composes
// with the variant lookup. Don't call this with a user-supplied
// FileInfo from somewhere else.
func (s *Server) serveVariant(w http.ResponseWriter, r *http.Request, sourcePath string, sourceInfo os.FileInfo, variantID string) {
	if s.variantStore == nil {
		writeError(w, http.StatusNotFound, "variant_not_found", "upscaling is not enabled on this bridge")
		return
	}
	rec, err := s.variantStore.LookupVariant(sourcePath, variantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, "variant_not_found", "no such variant")
		return
	}
	// Freshness gate. Mtime + size deltas indicate the source
	// has drifted since the sidecar was minted — operator's call
	// whether to re-convert (via `bridge upscale --force`) so we
	// never auto-delete a stale row here. iOS sees 410 Gone and
	// falls back to the original.
	//
	// Mtime comparison tolerates filesystem rounding granularities:
	// ext4 stores nanoseconds, NFS exports can truncate to
	// microseconds, and SMB / FAT32 mounts carry 2-second
	// granularity. A bare `!=` (or a tolerance narrower than the
	// FS's rounding step) would false-stale every variant on those
	// filesystems on the next bridge restart even when the source
	// was untouched. 2 s exactly covers FAT32 — the previous 1 ms
	// constant was three orders of magnitude too tight and produced
	// constant 410 Gone responses for libraries hosted on a NAS.
	// Real edits jump mtime by far more than 2 s (audacity save,
	// metadata rewrite, file replacement) so this tolerance still
	// reliably trips the gate when the source actually drifts.
	const mtimeToleranceNS int64 = 2_000_000_000
	mtimeDelta := rec.SourceMTimeNS - sourceInfo.ModTime().UnixNano()
	if mtimeDelta < 0 {
		mtimeDelta = -mtimeDelta
	}
	if mtimeDelta > mtimeToleranceNS || rec.SourceSize != sourceInfo.Size() {
		writeError(w, http.StatusGone, "variant_stale", "variant is out of date relative to source; falling back to original is recommended")
		return
	}
	f, err := os.Open(rec.SidecarPath)
	if err != nil {
		// Distinguish the "file genuinely gone" case (410 Gone,
		// iOS falls back to original, --gc reconciles) from
		// permission errors / I/O faults (5xx, operator must
		// see the real cause — silently mapping these to 410
		// would hide a permissions misconfig as if the variant
		// were permanently missing). CodeRabbit second-pass on
		// PR #108.
		if os.IsNotExist(err) {
			writeError(w, http.StatusGone, "variant_missing_on_disk", "sidecar file missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "open sidecar: "+err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// childPath returns the library-relative path of a child given its parent's
// library-relative path and its own name. Uses forward slashes regardless
// of the server's OS.
//
// `path.Join` (not `filepath.Join`) normalises consecutive slashes, strips
// trailing slashes from `parent`, and otherwise canonicalises forward-
// slash paths uniformly across OSes. Pre-fix used manual concatenation
// (`filepath.ToSlash(filepath.Clean(parent)) + "/" + name`); the result
// is functionally identical on every input today (since `name` comes from
// `os.ReadDir` which strips path separators), but the manual path is a
// foot-gun for future refactors that might pass a less-disciplined `name`
// — `path.Join` collapses any double-slash that creeps in (gemini-style
// review bias).
func childPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return name
	}
	return path.Join(filepath.ToSlash(parent), name)
}

// writeResolveError maps an fs resolver error to the right JSON error
// response. Returns true if an error was written (caller should bail).
func writeResolveError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, bridgefs.ErrBadPath):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, bridgefs.ErrUnknownRoot):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, bridgefs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
	return true
}

// lessCaseFold compares two strings case-insensitively across the full
// Unicode range. Matches the alphabetical-but-CI ordering iOS users
// expect in a file browser — "Ébène" sorts near "e", not after "z" as
// the older ASCII-only byte fold produced.
//
// **Use sortEntriesByName for sorting**, not this directly inside
// `sort.Slice` — `strings.ToLower` allocates two strings per call,
// and `sort.Slice` calls the comparator O(N log N) times. The
// dedicated sort helper computes each lowercased key once. lessCaseFold
// stays as the canonical case-fold predicate (package-internal) for
// one-shot comparisons and to keep `TestLessCaseFoldUnicode` honest.
func lessCaseFold(a, b string) bool {
	return strings.ToLower(a) < strings.ToLower(b)
}

// sortEntriesByName sorts entries in-place by case-folded Name. Each
// element's lowercased key is computed once into a parallel slice and
// reused by the comparator — vs. the previous `sort.Slice` +
// `strings.ToLower` per comparison, which allocated 2 × O(N log N)
// strings per request. For a 2000-file directory the prior shape did
// ~44 000 string allocations per /v1/list call (review item).
func sortEntriesByName(entries []Entry) {
	keys := make([]string, len(entries))
	for i := range entries {
		keys[i] = strings.ToLower(entries[i].Name)
	}
	sort.Sort(entriesByCaseFold{entries: entries, keys: keys})
}

// entriesByCaseFold is the sort.Interface implementation paired with
// sortEntriesByName. Swap moves both the entry and its precomputed key
// so subsequent comparisons keep referencing the right key.
//
// Less uses the original Name as a tie-break when folded keys match
// — without it, fold-equal entries ("Apple"/"apple") permute
// arbitrarily under sort.Sort and any UI that depends on a stable
// listing across requests sees flicker (CodeRabbit on PR #71).
type entriesByCaseFold struct {
	entries []Entry
	keys    []string
}

func (s entriesByCaseFold) Len() int { return len(s.entries) }
func (s entriesByCaseFold) Less(i, j int) bool {
	if s.keys[i] != s.keys[j] {
		return s.keys[i] < s.keys[j]
	}
	return s.entries[i].Name < s.entries[j].Name
}
func (s entriesByCaseFold) Swap(i, j int) {
	s.entries[i], s.entries[j] = s.entries[j], s.entries[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}

// Copy is exposed for test shims; wraps io.Copy so tests can swap it out.
// (Currently unused — kept as a seam for benchmarks.)
var Copy = io.Copy
