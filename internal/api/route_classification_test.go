package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestRouteRegistry_completeness pins the registered route set —
// adding a new mux.HandleFunc without a routeRegistry entry would
// silently bypass the per-route write-deadline middleware. The
// test compares the registry's pattern set against the "expected"
// set so any drift surfaces as a single failure with both diffs.
//
// **When adding a new route**: update both the registry in
// route_classification.go AND the `expected` set below in the
// same commit. The test is intentionally chatty (per-pattern
// comparison) so a missing-from-registry case can't slip past
// review by relying on a downstream check passing.
func TestRouteRegistry_completeness(t *testing.T) {
	expected := []string{
		"GET /v1/health",
		"GET /v1/list",
		"GET /v1/stat",
		"GET /v1/read",
		"GET /v1/download",
		"GET /v1/manifest",
		"GET /v1/artwork/{mbid}",
		"GET /v1/artist-image/{mbid}",
		"GET /v1/smart-playlist-image/{slug}",
		"GET /v1/playlist-image/{id}",
		"GET /v1/waveform",
		"GET /v1/analysis/stats",
		"POST /v1/upscale",
		"GET /v1/upscale/stats",
		"GET /v1/diagnostics",
		"POST /v1/upscale/batch",
		"GET /v1/upscale/batches",
		"DELETE /v1/upscale/batches/{id}",
		"DELETE /v1/upscale/variants",
		"GET /v1/playlists",
		"GET /v1/playlists/{id}",
		"PUT /v1/playlists/{id}",
		"DELETE /v1/playlists/{id}",
		"POST /v1/history/batch",
		"GET /v1/history",
		"GET /v1/smart-playlists",
		"GET /v1/renderers",
		"GET /v1/events",
		"GET /v1/pairing/{requestID}/events",
		"POST /v1/pairing/requests",
		"GET /v1/pairing/{requestID}",
		"DELETE /v1/pairing/{requestID}",
	}
	sort.Strings(expected)

	s := newRouteRegistryTestServer(t)
	registry := s.routeRegistry()
	var got []string
	for _, rt := range registry {
		got = append(got, rt.pattern)
	}
	sort.Strings(got)

	if strings.Join(expected, "\n") != strings.Join(got, "\n") {
		t.Errorf("route registry drift\nexpected:\n  %s\ngot:\n  %s",
			strings.Join(expected, "\n  "),
			strings.Join(got, "\n  "),
		)
	}
}

// TestRouteRegistry_streamingClassification pins the must-stream
// route set — anything in this list MUST be classified as
// `streamingRoute` because its response is either unbounded
// (multi-GB downloads, 50k-track manifest streams) or long-lived
// (SSE event streams). A future contributor who flips one of
// these to boundedRoute would cut off legitimate clients
// mid-response under the 60s write deadline.
func TestRouteRegistry_streamingClassification(t *testing.T) {
	mustStream := map[string]struct{}{
		"GET /v1/read":                       {},
		"GET /v1/download":                   {},
		"GET /v1/manifest":                   {},
		"GET /v1/events":                     {},
		"GET /v1/pairing/{requestID}/events": {},
	}
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		if _, mustBeStreaming := mustStream[rt.pattern]; mustBeStreaming {
			if rt.kind != streamingRoute {
				t.Errorf("route %q classified as boundedRoute but must be streamingRoute (response is unbounded or long-lived)", rt.pattern)
			}
		}
	}
}

// TestRouteRegistry_noUnexpectedStreamingRoutes pins the inverse:
// no route OUTSIDE the must-stream set should be streamingRoute.
// New routes default to boundedRoute (the zero value of routeKind)
// and only opt into streaming with explicit intent — this test
// catches an accidental classification flip.
func TestRouteRegistry_noUnexpectedStreamingRoutes(t *testing.T) {
	allowedStreaming := map[string]struct{}{
		"GET /v1/read":                       {},
		"GET /v1/download":                   {},
		"GET /v1/manifest":                   {},
		"GET /v1/events":                     {},
		"GET /v1/pairing/{requestID}/events": {},
	}
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		if rt.kind == streamingRoute {
			if _, ok := allowedStreaming[rt.pattern]; !ok {
				t.Errorf("route %q classified streamingRoute but isn't in the allow-list — bounded routes need a write deadline to defend against slow-reading clients", rt.pattern)
			}
		}
	}
}

// TestBoundedHandler_setsWriteDeadline pins the headline contract:
// the wrapper actually calls `SetWriteDeadline` on the response
// controller. We can't directly observe the deadline value
// (httptest's recorder doesn't expose it), but we CAN verify the
// inner handler ran (so the wrapper isn't no-op'ing on its own
// arguments) — the wrapper's `Debug` log on SetWriteDeadline error
// is the only way it could degrade without surfacing.
func TestBoundedHandler_invokesInner(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	wrapped := boundedHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	wrapped(httptest.NewRecorder(), req)
	if !called {
		t.Error("boundedHandler did not call inner handler")
	}
}

// TestBoundedRouteWriteDeadline_constantSanity pins the budget
// value — a future change that ratchets this too low would
// false-positive on legitimate slow links (Tailscale relay over
// mobile typically completes a bounded response in <1s; 60s
// leaves a generous safety margin), while too high (e.g.
// 600s = 10 min) defeats the slow-loris defence.
//
// Range pinned 30s ≤ deadline ≤ 120s — anything outside that
// is a deliberate change that should surface in review.
func TestBoundedRouteWriteDeadline_constantSanity(t *testing.T) {
	if boundedRouteWriteDeadline < 30*time.Second {
		t.Errorf("boundedRouteWriteDeadline = %v, want >= 30s (too tight for slow networks)", boundedRouteWriteDeadline)
	}
	if boundedRouteWriteDeadline > 120*time.Second {
		t.Errorf("boundedRouteWriteDeadline = %v, want <= 120s (too loose for slow-loris defence)", boundedRouteWriteDeadline)
	}
}

// newRouteRegistryTestServer is a minimal Server construction for
// the route-registry tests — no manifest provider, no upscale
// wiring, just enough for `routeRegistry()` to be enumerable.
// The existing `newTestServer` in api_test.go returns a
// `*httptest.Server` paired with a token; this helper needs the
// raw *Server to call `routeRegistry()` directly.
func newRouteRegistryTestServer(t *testing.T) *Server {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	return New(cfg, store, nil, "fp")
}

// TestRouteRegistry_writeDeadlineOverrides pins the per-route deadline
// override set: exactly the two long-op upscale routes carry the
// 15-minute budget (their synchronous server-side work scales with
// library size — folder walk / per-variant delete loop), and every
// other bounded route stays on the 60 s default. A new override is a
// deliberate decision that must surface here.
func TestRouteRegistry_writeDeadlineOverrides(t *testing.T) {
	wantLong := map[string]struct{}{
		"POST /v1/upscale":            {},
		"DELETE /v1/upscale/variants": {},
	}
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		_, long := wantLong[rt.pattern]
		switch {
		case long && rt.writeDeadline != upscaleLongOpWriteDeadline:
			t.Errorf("route %q writeDeadline = %v, want %v", rt.pattern, rt.writeDeadline, upscaleLongOpWriteDeadline)
		case !long && rt.writeDeadline != 0:
			t.Errorf("route %q carries an unexpected writeDeadline override %v", rt.pattern, rt.writeDeadline)
		}
	}
}
