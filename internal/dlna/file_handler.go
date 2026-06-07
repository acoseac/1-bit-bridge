package dlna

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
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
//  3. **UPnP-routed fast-path**: if `routing` is wired and the
//     track's manifest path has a routing row, proxy the upstream's
//     bytes bit-exact via `proxy.Serve`. The DLNA renderer never
//     learns the bytes live elsewhere — to it this is just a normal
//     bridge file fetch.
//  4. Otherwise: open the resolved `AbsolutePath`. Open failure → 500.
//  5. Set DLNA-required response headers (Content-Type via
//     PreferredMIMEFor, transferMode.dlna.org: Streaming,
//     contentFeatures.dlna.org, Accept-Ranges: bytes).
//  6. Wrap the ResponseWriter in `AdaptiveResponseWriter` (chunk
//     size adapts to RTT/jitter/loss — placeholder until task #11
//     wires live network telemetry from the listener).
//  7. `http.ServeContent` owns Range / 206 Partial Content handling.
//
// `routing` + `proxy` are OPTIONAL. Pass `nil` for both to disable
// the UPnP-routed fast-path entirely (legacy behaviour — every
// request goes through the filesystem resolver). When `routing` is
// wired but `proxy` is nil (shouldn't reach production — the
// wiring layer ensures pairing), the fast-path silently falls
// through to the filesystem so the worst case is the pre-fix 404
// rather than a panic.
//
// **Auth bypass is by design.** The DLNA listener binds LAN-only
// (per `IsLANEligibleInterface` in PR 1 task #11); DLNA renderers
// can't speak the bridge's bearer-token scheme. The LAN-only bind
// IS the gate, not authentication.
//
// **HEAD requests are supported** via http.ServeContent's native
// behavior — useful for libavformat / mConnect probes that test
// reachability before issuing the full GET. The UPnP proxy's
// `Serve` also honours HEAD by skipping the body copy.
func FileHandler(lib LibrarySource, routing upnpproxy.RoutingLookup, proxy *upnpproxy.Proxy) http.HandlerFunc {
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

		// UPnP-routed fast-path: track lives on an upstream
		// MediaServer (e.g. a Chord 2Go's microSD card), not the local
		// filesystem. Proxy bytes bit-exact via the upnpproxy package
		// — same logic the `/v1/download` path uses (PR-pending,
		// post-pair-A operator verification of PR #732).
		//
		// Variant query (`/variant-{id}{ext}` trailing segment) is
		// NEVER routed: variants are bridge-minted sidecars, an
		// upstream track has no variants today. So we only consult
		// the routing table when the resolved trailing segment is
		// the SOURCE path, not a variant.
		if routing != nil && proxy != nil && info.RelativePath != "" && extractVariantID(r.URL.Path) == "" {
			rt, lookupErr := routing.GetUPnPRouting(r.Context(), info.RelativePath)
			if lookupErr == nil && rt != nil {
				perr := proxy.Serve(r.Context(), w, r.Method, r.Header, rt)
				if perr != nil {
					// DLNA renderers expect plain-text HTTP errors,
					// not structured JSON (cf. api's writeError
					// envelope). `http.Error` emits text/plain.
					http.Error(w, perr.Message, perr.Status)
				}
				return
			}
			// (nil, nil) → not routed → fall through to filesystem.
			// (nil, err) → log + fall through (worst case 404; this
			// is the pre-fix legacy behaviour for routed tracks AND
			// the correct behaviour for filesystem tracks — same
			// shape as the api `serveFile`'s loud-log-then-fall-
			// through approach).
		}

		// Resolve which file to serve: the source, or an offline variant
		// addressed via the trailing `/variant-{id}{ext}` path segment.
		servePath, ext, isVariant, known := resolveServeTarget(info, r.URL.Path)
		if !known {
			// A variant segment was present but the ID isn't one this
			// track carries — to the renderer this is "no such object".
			http.NotFound(w, r)
			return
		}

		f, err := os.Open(servePath)
		if err != nil {
			if isVariant {
				// The DB row pointed at a sidecar that's no longer on
				// disk (GC'd, manually deleted). Mirror the api
				// /v1/download?variant= contract: 410 Gone, distinct from
				// a 404 "unknown object".
				http.Error(w, "variant sidecar missing", http.StatusGone)
				return
			}
			// Source file vanished between scan and serve, or permissions
			// changed. Return 404 — indistinguishable to the renderer from
			// "track doesn't exist".
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
		if ext == "" {
			// Defensive — derive from path if the adapter didn't pre-fill.
			ext = strings.ToLower(filepath.Ext(servePath))
		}

		w.Header().Set("Content-Type", PreferredMIMEFor(ua, ext))
		w.Header().Set("transferMode.dlna.org", "Streaming")
		w.Header().Set("contentFeatures.dlna.org", ContentFeaturesHeaderValue(ua, ext))
		w.Header().Set("Accept-Ranges", "bytes")

		// Phase 0 diagnostic tracing (OFF unless BRIDGE_DLNA_TRACE is set).
		// The trace wrapper sits BELOW the adaptive writer so it times the
		// real flush-to-socket calls (the ones that block under renderer
		// back-pressure) and watches the request Context for peer-close —
		// distinguishing a STALLED UPnP pause from a closed socket. Zero-cost
		// passthrough when disabled. See file_trace.go.
		dst, finishTrace := newFileTrace(w, r, trackID)
		defer finishTrace()

		// Wrap in AdaptiveResponseWriter. Default chunk size today;
		// task #11 wires per-connection RTT/jitter telemetry into
		// `ChunkSizeFor` so the wrapper adapts to observed network
		// conditions. The wrap is structural even at the default
		// chunk size — defer-Flush drains trailing bytes that
		// `http.ServeContent`'s 32KB internal loop would otherwise
		// leave in our buffer.
		chunkSize := ChunkSizeFor(0, 0, 0) // (rtt, jitter, loss) — placeholder until task #11
		aw := NewAdaptiveResponseWriter(dst, chunkSize)
		defer aw.Flush()

		// http.ServeContent handles Range + 206 + If-Modified-Since
		// + Content-Length. The wrapper passes status / headers through
		// to the underlying ResponseWriter; ServeContent's writes go
		// through the wrapper's buffer.
		http.ServeContent(aw, r, filepath.Base(servePath), stat.ModTime(), f)
	}
}

// resolveServeTarget decides which file a /dlna/file/ request addresses:
// the source, or an offline variant named by the trailing
// `/variant-{id}{ext}` segment. Returns the path to serve, the extension
// to drive MIME / contentFeatures, whether the target is a variant (so
// the caller maps a missing file to 410 Gone rather than 404), and
// `known` — false ONLY when a variant segment was present but its ID
// isn't one this track carries (caller → 404).
//
// `ext` is assigned the variant's FileExtension UNCONDITIONALLY: if a
// variant somehow records an empty extension, leaving it as the source's
// (.dsf) would mis-describe a .flac sidecar; an empty value instead falls
// through to the caller's derive-from-path fallback. Per
// gemini-code-assist (HIGH) on PR #330.
func resolveServeTarget(info TrackInfo, urlPath string) (servePath, ext string, isVariant, known bool) {
	servePath = info.AbsolutePath
	ext = info.FileExtension
	variantID := extractVariantID(urlPath)
	if variantID == "" {
		return servePath, ext, false, true
	}
	v, found := findVariant(info.Variants, variantID)
	if !found {
		return "", "", true, false
	}
	return v.AbsolutePath, v.FileExtension, true, true
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

// variantPathPrefix is the marker that introduces the variant segment in
// a `/dlna/file/{trackID}/variant-{variantID}{ext}` URL.
const variantPathPrefix = "variant-"

// extractVariantID recovers the variant ID from a variant file URL of
// the form `/dlna/file/{trackID}/variant-{variantID}{ext}`. Returns ""
// when the path has no second segment (a plain source request) or the
// second segment isn't a `variant-` segment.
//
// The variant ID itself never contains a '.', so the trailing file
// extension is stripped at the LAST dot. Pure helper — tested in
// isolation.
func extractVariantID(urlPath string) string {
	if !strings.HasPrefix(urlPath, FilePathPrefix) {
		return ""
	}
	rest := urlPath[len(FilePathPrefix):]
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return "" // no second segment → source request
	}
	seg := rest[idx+1:]
	// Ignore any further segments after the variant one.
	if j := strings.IndexByte(seg, '/'); j >= 0 {
		seg = seg[:j]
	}
	if !strings.HasPrefix(seg, variantPathPrefix) {
		return ""
	}
	seg = seg[len(variantPathPrefix):]
	// Strip the trailing file extension (variant IDs carry no '.').
	if dot := strings.LastIndexByte(seg, '.'); dot >= 0 {
		seg = seg[:dot]
	}
	return seg
}

// Avoid `path` import being flagged if a future refactor uses
// `filepath` exclusively. `path.Clean` is the right tool for
// URL-path manipulation (vs filepath which uses OS-specific
// separators), so keep the import live via a token reference.
var _ = path.Base
