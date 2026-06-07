package dlna

import (
	"context"
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
	m map[string]*manifest.UPnPRouting
}

func (s *stubRoutingLookup) GetUPnPRouting(_ context.Context, p string) (*manifest.UPnPRouting, error) {
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
func Test_FileHandler_UPnPRoutedTrack_ProxiesUpstreamBytes(t *testing.T) {
	// Spin up a stub "2Go" upstream that returns FLAC magic + a few bytes.
	upstreamBody := "fLaC\x00\x01\x02\x03"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the Range header back in Content-Range so the test can
		// confirm the header flowed through.
		if rng := r.Header.Get("Range"); rng != "" {
			w.Header().Set("Content-Range", "bytes 0-7/8")
			w.Header().Set("Content-Type", "audio/x-flac")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(upstreamBody))
			return
		}
		w.Header().Set("Content-Type", "audio/x-flac")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	// Library has the track with RelativePath populated — the routing
	// lookup keys on it.
	const relPath = "2go/Music/Test Artist/Test Album/Test Track.flac"
	lib := newTestLib(TrackInfo{
		TrackID:       "abc123",
		RelativePath:  relPath,
		AbsolutePath:  "/non/existent/path", // would 404 if the fast-path didn't take
		FileExtension: ".flac",
		Size:          int64(len(upstreamBody)),
	})

	// Routing lookup says this track lives on the upstream + we know
	// the live host.
	routing := &stubRoutingLookup{m: map[string]*manifest.UPnPRouting{
		relPath: {
			SourcePath: relPath,
			ServerUDN:  "uuid:test-2go",
			ObjectID:   "64$0$0",
			ResURL:     "/MediaItems/5.flac",
		},
	}}
	proxy := upnpproxy.New(&stubHostResolver{host: upstreamHost}, nil)
	h := FileHandler(lib, routing, proxy)

	t.Run("GET returns upstream bytes bit-exact", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/dlna/file/abc123", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rec.Code)
		}
		if got := rec.Body.String(); got != upstreamBody {
			t.Errorf("body = %q; want %q", got, upstreamBody)
		}
		if got := rec.Header().Get("Content-Type"); got != "audio/x-flac" {
			t.Errorf("Content-Type = %q; want audio/x-flac", got)
		}
	})

	t.Run("Range header flows through to upstream", func(t *testing.T) {
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
	})

	t.Run("HEAD skips body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/dlna/file/abc123", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD response should have empty body, got %d bytes", rec.Body.Len())
		}
	})
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
