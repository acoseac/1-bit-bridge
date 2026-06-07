package dlna

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
)

// Stubs scoped to this test file — independent of the file-system-
// based fixtures the legacy filesystem tests share.

type stubRoutingLookup struct {
	m   map[string]*manifest.UPnPRouting
	err error // if non-nil, every lookup returns (nil, err) — for the transient-DB-error tests
}

func (s *stubRoutingLookup) GetUPnPRouting(_ context.Context, p string) (*manifest.UPnPRouting, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.m[p], nil
}

type stubHostResolver struct{ host string }

func (s *stubHostResolver) LiveHost(_ string) (string, bool) {
	if s.host == "" {
		return "", false
	}
	return s.host, true
}

// Test_FileHandler_UPnPRoutedTrack_ProxiesUpstreamBytes asserts the
// new fast-path: a manifest track whose RelativePath has a row in
// `upnp_track_routing` (the upstream MediaServer feature) is proxied
// through the upnpproxy package instead of being mapped to a
// filesystem path and 404'ing.
//
// This is the regression guard for the bug surfaced by the post-pair-A
// operator verification of PR #732: iOS casts a 2Go-routed track to a
// DLNA renderer via the bridge → renderer GETs
// `/dlna/file/{trackID}` → pre-fix the handler returned 404 → silent
// decline at the renderer.
// upstreamFixture spins up a stub "2Go" upstream that returns FLAC
// magic + a few bytes, mirroring how the real 2Go's MiniDLNA serves
// `/MediaItems/<id>.flac`. Pulled out as a helper so the per-method
// regression tests below each focus on one wire-axis assertion (and
// stay under the SonarCloud cognitive-complexity threshold).
//
// The handler echoes a `Range` header back in `Content-Range` so the
// 206 test can confirm the header actually flowed through the proxy.
// The returned `*string` carries the verbatim `Range` header the
// upstream observed — assertable in the test body so a proxy that
// forwarded the WRONG byte range would be caught at the upstream-
// observed side, not just at the response-header echo (CodeRabbit
// MINOR on PR #356 round-3).
func upstreamFixture(t *testing.T) (host, body string, observedRange *string, cleanup func()) {
	t.Helper()
	body = "fLaC\x00\x01\x02\x03"
	var seenRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRange = r.Header.Get("Range")
		if seenRange != "" {
			w.Header().Set("Content-Range", "bytes 0-7/8")
			w.Header().Set("Content-Type", "audio/x-flac")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "audio/x-flac")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	return strings.TrimPrefix(srv.URL, "http://"), body, &seenRange, srv.Close
}

// routedHandlerFixture wires the dlna FileHandler with a routing
// lookup + a host resolver pointing at the upstream stub from
// upstreamFixture. Returns the handler ready to invoke, the upstream
// body the handler should pass through unchanged, and the
// upstream-observed Range header pointer so test bodies can assert
// the exact byte range forwarded.
func routedHandlerFixture(t *testing.T) (handler http.HandlerFunc, upstreamBody string, observedRange *string, cleanup func()) {
	t.Helper()
	host, body, seenRange, closeFn := upstreamFixture(t)
	const relPath = "2go/Music/Test Artist/Test Album/Test Track.flac"
	lib := newTestLib(TrackInfo{
		TrackID:       "abc123",
		RelativePath:  relPath,
		AbsolutePath:  "/non/existent/path", // would 404 if the fast-path didn't take
		FileExtension: ".flac",
		Size:          int64(len(body)),
	})
	routing := &stubRoutingLookup{m: map[string]*manifest.UPnPRouting{
		relPath: {
			SourcePath: relPath,
			ServerUDN:  "uuid:test-2go",
			ObjectID:   "64$0$0",
			ResURL:     "/MediaItems/5.flac",
		},
	}}
	proxy := upnpproxy.New(&stubHostResolver{host: host}, nil)
	return FileHandler(lib, routing, proxy), body, seenRange, closeFn
}

// Test_FileHandler_UPnPRoutedTrack_GET — bit-exact upstream bytes
// surface on a vanilla GET (no Range header). Replaces the prior
// `t.Run("GET returns upstream bytes bit-exact", ...)` sub-test of a
// monolithic `Test_FileHandler_UPnPRoutedTrack_ProxiesUpstreamBytes`
// to keep cognitive complexity below SonarCloud's S3776 threshold —
// each per-method axis is now its own top-level test.
func Test_FileHandler_UPnPRoutedTrack_GET(t *testing.T) {
	h, body, _, cleanup := routedHandlerFixture(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/abc123", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q; want %q", got, body)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/x-flac" {
		t.Errorf("Content-Type = %q; want audio/x-flac", got)
	}
}

// Test_FileHandler_UPnPRoutedTrack_RangeHeader — Range header flows
// through to the upstream MediaServer verbatim; the response carries
// the upstream's 206 + Content-Range. The upstream-observed Range
// assertion is what makes this test actually catch a proxy that
// forwarded the WRONG byte range (CodeRabbit MINOR on PR #356
// round-3 — the original `Content-Range = "bytes 0-7/8"` check
// passed even when the fixture echoed a fixed Content-Range
// regardless of the byte range it received). Without this, the
// renderer's readRange (DSF mid-file seek; FLAC stream start) would
// fall back to a full-body GET and the bit-exact contract over a
// marginal LAN would suffer.
func Test_FileHandler_UPnPRoutedTrack_RangeHeader(t *testing.T) {
	h, _, observedRange, cleanup := routedHandlerFixture(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/abc123", nil)
	req.Header.Set("Range", "bytes=0-7")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d; want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 0-7/8" {
		t.Errorf("Content-Range = %q; want bytes 0-7/8", got)
	}
	if *observedRange != "bytes=0-7" {
		t.Errorf("upstream-observed Range = %q; want bytes=0-7 — bit-exact contract requires verbatim byte-range passthrough, NOT just a non-empty Range that defaults to a hardcoded Content-Range echo",
			*observedRange)
	}
}

// Test_FileHandler_UPnPRoutedTrack_HEAD — HEAD returns the upstream's
// 200 + headers with an empty body. Renderers frequently HEAD a track
// to probe size + DLNA flags before issuing the actual GET; the proxy
// must not stream bytes back on HEAD.
func Test_FileHandler_UPnPRoutedTrack_HEAD(t *testing.T) {
	h, _, _, cleanup := routedHandlerFixture(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodHead, "/dlna/file/abc123", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response should have empty body, got %d bytes", rec.Body.Len())
	}
}

// Test_FileHandler_UpstreamOffline_503 — when SSDP hasn't discovered
// the upstream yet (or it went offline), the proxy returns
// PreStreamError(503, "upnp_server_offline") and the dlna handler
// surfaces that as plain-text HTTP 503 (renderer sees a real error
// status, not a silent decline OR a 404 misclassification).
func Test_FileHandler_UPnPRoutedTrack_UpstreamOffline_Returns503(t *testing.T) {
	const relPath = "2go/x.flac"
	lib := newTestLib(TrackInfo{
		TrackID: "abc", RelativePath: relPath, AbsolutePath: "/nope",
		FileExtension: ".flac", Size: 1,
	})
	routing := &stubRoutingLookup{m: map[string]*manifest.UPnPRouting{
		relPath: {SourcePath: relPath, ServerUDN: "uuid:offline", ResURL: "/x.flac"},
	}}
	// Empty host → resolver returns ("", false) → 503.
	proxy := upnpproxy.New(&stubHostResolver{host: ""}, nil)
	h := FileHandler(lib, routing, proxy)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/abc", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; want text/plain (renderers expect plain-text errors)", ct)
	}
}

// Test_FileHandler_NoRoutingLookup_FallsThroughToFilesystem confirms
// the legacy filesystem path still works when the proxy isn't wired
// (mirrors how a pre-feature bridge behaves; `FileHandler(lib, nil, nil)`
// is the documented zero-config call from `internal/dlna/server.go`).
func Test_FileHandler_NoRoutingLookup_FallsThroughToFilesystem(t *testing.T) {
	path := createTempFile(t, ".flac", "fLaC local content")
	lib := newTestLib(TrackInfo{
		TrackID: "local", RelativePath: "Music/local.flac",
		AbsolutePath: path, FileExtension: ".flac", Size: 18,
	})
	h := FileHandler(lib, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/local", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "fLaC local content" {
		t.Errorf("body = %q; want %q", got, "fLaC local content")
	}
}

// Test_FileHandler_RoutingWithNilMatch_FallsThroughToFilesystem — the
// routing lookup is wired but returns (nil, nil) for the requested
// path (it's a filesystem track, not an upstream-routed one). The
// handler must serve from the local filesystem AbsolutePath.
func Test_FileHandler_RoutingMissForFilesystemTrack_StillServesLocally(t *testing.T) {
	path := createTempFile(t, ".flac", "local-fs")
	lib := newTestLib(TrackInfo{
		TrackID: "fs", RelativePath: "Music/fs.flac",
		AbsolutePath: path, FileExtension: ".flac", Size: 8,
	})
	// Routing lookup is wired but the path isn't mapped → (nil, nil).
	routing := &stubRoutingLookup{m: map[string]*manifest.UPnPRouting{}}
	proxy := upnpproxy.New(&stubHostResolver{host: "127.0.0.1:9"}, nil)
	h := FileHandler(lib, routing, proxy)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/fs", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "local-fs" {
		t.Errorf("body = %q; want %q", got, "local-fs")
	}
}

// Test_FileHandler_RoutingLookupError_OnRoutedTrack_Returns500 — the
// load-bearing CodeRabbit MAJOR fix from PR #356 round-4: for a track
// that was persisted by the manifest as routed (AbsolutePath == ""
// sentinel from rebuild's routing-fast-path), a transient
// routing-lookup error MUST NOT fall through to `os.Open("")` (which
// would surface as a false 404 → iOS caches as `lastErrorRescanShareID`
// → surfaces a "rescan share?" affordance the user didn't actually
// need). Instead the handler returns 500 so iOS retries on the next
// play tap.
func Test_FileHandler_RoutingLookupError_OnRoutedTrack_Returns500(t *testing.T) {
	const relPath = "2go/Music/x.flac"
	// Routed track per the manifest convention: AbsolutePath empty,
	// RelativePath populated → the file-handler's pre-fix fallback
	// would do `os.Open("")` and 404.
	lib := newTestLib(TrackInfo{
		TrackID:       "routed-err",
		RelativePath:  relPath,
		AbsolutePath:  "",
		FileExtension: ".flac",
		Size:          1,
	})
	// Routing lookup returns (nil, err) — simulates transient SQLite
	// failure, connection reset, etc.
	routing := &stubRoutingLookup{err: errors.New("simulated transient DB error")}
	proxy := upnpproxy.New(&stubHostResolver{host: "127.0.0.1:1"}, nil)
	h := FileHandler(lib, routing, proxy)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/routed-err", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 — routed sentinel + transient DB error MUST surface as 5xx so iOS retries, NOT fall through to os.Open(\"\") which 404s and triggers a false rescan affordance",
			rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; want text/plain (renderers expect plain-text errors)", ct)
	}
}

// Test_FileHandler_RoutingLookupError_OnFilesystemTrack_FallsThrough —
// the orthogonal half of the round-4 fix: for a track with a real
// filesystem AbsolutePath, the lookup is purely informational, and a
// transient DB error must NOT block the legitimate filesystem serve.
// The handler falls through and the file's bytes are served as
// normal. Without this branch's surgical use of `info.AbsolutePath ==
// ""` to differentiate, the round-4 fix would regress every
// filesystem track on a transient routing DB error.
func Test_FileHandler_RoutingLookupError_OnFilesystemTrack_FallsThrough(t *testing.T) {
	path := createTempFile(t, ".flac", "local-fs-bytes")
	const relPath = "Music/local.flac"
	lib := newTestLib(TrackInfo{
		TrackID:       "fs-err",
		RelativePath:  relPath,
		AbsolutePath:  path, // ← filesystem track: non-empty absPath
		FileExtension: ".flac",
		Size:          int64(len("local-fs-bytes")),
	})
	routing := &stubRoutingLookup{err: errors.New("simulated transient DB error")}
	proxy := upnpproxy.New(&stubHostResolver{host: "127.0.0.1:1"}, nil)
	h := FileHandler(lib, routing, proxy)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/fs-err", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — filesystem track + transient DB error must fall through to os.Open(realPath) and serve normally; body=%s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "local-fs-bytes" {
		t.Errorf("body = %q; want %q", got, "local-fs-bytes")
	}
}

// Test_FileHandler_VariantTrailingSegment_BypassesProxy — a request
// to `/dlna/file/{trackID}/variant-{id}{ext}` MUST NOT be proxied to
// the upstream even when the track has a routing row, because
// variants are bridge-minted sidecars by definition. The legacy
// variant resolution path takes over.
func Test_FileHandler_VariantSegment_BypassesUPnPProxy(t *testing.T) {
	variantPath := createTempFile(t, ".flac", "variant-bytes")
	const relPath = "2go/Music/x.flac"
	lib := newTestLib(TrackInfo{
		TrackID: "vbypass", RelativePath: relPath,
		AbsolutePath: "/source-wont-be-opened", FileExtension: ".flac", Size: 0,
		Variants: []VariantInfo{
			{VariantID: "v1", AbsolutePath: variantPath, FileExtension: ".flac"},
		},
	})
	// Routing IS wired for the source path — but the request is for
	// the variant trailing segment, so the proxy must NOT engage.
	routing := &stubRoutingLookup{m: map[string]*manifest.UPnPRouting{
		relPath: {SourcePath: relPath, ServerUDN: "u", ResURL: "/x.flac"},
	}}
	proxy := upnpproxy.New(&stubHostResolver{host: "127.0.0.1:1"}, nil) // would 502 if hit
	h := FileHandler(lib, routing, proxy)

	req := httptest.NewRequest(http.MethodGet, "/dlna/file/vbypass/variant-v1.flac", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "variant-bytes" {
		t.Errorf("body = %q; want %q", got, "variant-bytes")
	}
}
