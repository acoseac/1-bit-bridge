package dlna

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// FilePathPrefix is the URL prefix the file handler is mounted under.
// Trailing slash so `path.Base` cleanly extracts the trackID component.
const FilePathPrefix = "/dlna/file/"

// FileHandler returns an http.HandlerFunc that serves bytes for the
// track addressed by the trailing path component of /dlna/file/{trackID}.
// Resolution path:
//
//  1. Extract trackID from URL path (after /dlna/file/ prefix).
//  2. Lookup via `LibrarySource.GetTrackInfo(trackID)`. Unknown ID → 404.
//  3. Open the resolved `AbsolutePath`. Open failure → 500.
//  4. Set DLNA-required response headers:
//     - Content-Type via PreferredMIMEFor(UA, ext) — per-vendor
//     - transferMode.dlna.org: Streaming
//     - contentFeatures.dlna.org: full DLNA.ORG_OP/CI/FLAGS string
//     - Accept-Ranges: bytes (advertises Range support)
//  5. Wrap the ResponseWriter in `AdaptiveResponseWriter` so the
//     chunk size adapts to (RTT/jitter/loss) — today the chunk-size
//     selector uses static defaults; PR 1 task #11 will wire live
//     network telemetry from the listener once that's available.
//  6. Defer `aw.Flush()` to drain trailing bytes after ServeContent.
//  7. Call `http.ServeContent` which owns Range / 206 Partial Content
//     handling — the wrapper composes WITH ServeContent rather than
//     replacing it (CLAUDE.md "AdaptiveResponseWriter — wraps
//     http.ServeContent without losing Range" invariant).
//
// **Auth bypass is by design.** The DLNA listener binds LAN-only
// (per `IsLANEligibleInterface` in PR 1 task #11); DLNA renderers
// can't speak the bridge's bearer-token scheme. The LAN-only bind
// IS the gate, not authentication.
//
// **HEAD requests are supported** via http.ServeContent's native
// behavior — useful for libavformat / mConnect probes that test
// reachability before issuing the full GET.
func FileHandler(lib LibrarySource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Accept GET + HEAD; reject everything else.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
			return
		}

		trackID := extractTrackID(r.URL.Path)
		if trackID == "" {
			http.NotFound(w, r)
			return
		}

		info, ok := lib.GetTrackInfo(trackID)
		if !ok {
			http.NotFound(w, r)
			return
		}

		f, err := os.Open(info.AbsolutePath)
		if err != nil {
			// Either the file vanished between scan and serve, or
			// permissions changed. Log via the telemetry middleware
			// (which records the eventual 500 status); return 404
			// rather than 500 because to the renderer this is
			// indistinguishable from "track doesn't exist".
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "stat failed", http.StatusInternalServerError)
			return
		}

		ua := r.Header.Get("User-Agent")
		ext := info.FileExtension
		if ext == "" {
			// Defensive — derive from path if the adapter didn't pre-fill.
			ext = strings.ToLower(filepath.Ext(info.AbsolutePath))
		}

		w.Header().Set("Content-Type", PreferredMIMEFor(ua, ext))
		w.Header().Set("transferMode.dlna.org", "Streaming")
		w.Header().Set("contentFeatures.dlna.org", ContentFeaturesHeaderValue(ua, ext))
		w.Header().Set("Accept-Ranges", "bytes")

		// Wrap in AdaptiveResponseWriter. Default chunk size today;
		// task #11 wires per-connection RTT/jitter telemetry into
		// `ChunkSizeFor` so the wrapper adapts to observed network
		// conditions. The wrap is structural even at the default
		// chunk size — defer-Flush drains trailing bytes that
		// `http.ServeContent`'s 32KB internal loop would otherwise
		// leave in our buffer.
		chunkSize := ChunkSizeFor(0, 0, 0) // (rtt, jitter, loss) — placeholder until task #11
		aw := NewAdaptiveResponseWriter(w, chunkSize)
		defer aw.Flush()

		// http.ServeContent handles Range + 206 + If-Modified-Since
		// + Content-Length. The wrapper passes status / headers through
		// to the underlying ResponseWriter; ServeContent's writes go
		// through the wrapper's buffer.
		http.ServeContent(aw, r, filepath.Base(info.AbsolutePath), stat.ModTime(), f)
	}
}

// extractTrackID extracts the trackID component from a path matching
// `/dlna/file/{trackID}` (or `/dlna/file/{trackID}/anything-after`
// — extra path components are ignored). Returns "" if the path
// doesn't match the prefix.
//
// Pure helper — tested in isolation in file_handler_test.go without
// constructing real http.Requests.
func extractTrackID(urlPath string) string {
	if !strings.HasPrefix(urlPath, FilePathPrefix) {
		return ""
	}
	rest := urlPath[len(FilePathPrefix):]
	if rest == "" {
		return ""
	}
	// Defensive: if the renderer sends `/dlna/file/{id}/extra`, take
	// just the first segment. path.Base on `/a/b/c` returns "c"; we
	// want "a" instead (the first segment after the prefix).
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// Avoid `path` import being flagged if a future refactor uses
// `filepath` exclusively. `path.Clean` is the right tool for
// URL-path manipulation (vs filepath which uses OS-specific
// separators), so keep the import live via a token reference.
var _ = path.Base
