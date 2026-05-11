package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func TestManifestRateLimiter_BurstThenBlock(t *testing.T) {
	// 60 rpm / burst=1: first Reserve has zero delay, second is deferred.
	rl := newManifestRateLimiter(60, 1)
	lim := rl.limiterFor("a")

	if d := lim.Reserve().Delay(); d != 0 {
		t.Errorf("first slot: delay = %v, want 0", d)
	}
	if d := lim.Reserve().Delay(); d <= 0 {
		t.Errorf("second slot: delay = %v, want > 0", d)
	}
}

func TestManifestRateLimiter_PerTokenIsolation(t *testing.T) {
	rl := newManifestRateLimiter(60, 1)
	a := rl.limiterFor("token-a")
	b := rl.limiterFor("token-b")

	// Drain A's burst.
	a.Reserve()
	// B's first slot must remain unaffected.
	if d := b.Reserve().Delay(); d != 0 {
		t.Errorf("B's first slot delay = %v, want 0 (A's exhaustion must not bleed)", d)
	}
}

func TestManifestRateLimiter_DisabledWhenRPMZero(t *testing.T) {
	if !newManifestRateLimiter(0, 3).disabled() {
		t.Error("rpm=0 must report disabled")
	}
	if newManifestRateLimiter(6, 3).disabled() {
		t.Error("rpm>0 must not report disabled")
	}
}

func TestManifestRateLimiter_ReaperDropsIdle(t *testing.T) {
	rl := newManifestRateLimiter(6, 3)
	rl.limiterFor("stale")
	rl.limiterFor("fresh")
	rl.mu.Lock()
	rl.entries["stale"].lastSeen = time.Now().Add(-2 * manifestLimiterIdleTimeout)
	dropped := rl.reapIdle(time.Now())
	rl.mu.Unlock()
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, ok := rl.entries["stale"]; ok {
		t.Error("stale should be reaped")
	}
	if _, ok := rl.entries["fresh"]; !ok {
		t.Error("fresh should survive")
	}
}

func TestTokenIDFromContext_RoundTrip(t *testing.T) {
	if got := tokenIDFromContext(withTokenID(context.Background(), "abc")); got != "abc" {
		t.Errorf("round-trip = %q, want abc", got)
	}
	if got := tokenIDFromContext(context.Background()); got != "" {
		t.Errorf("bare ctx = %q, want empty", got)
	}
}

// TestRateLimitManifestMiddleware_FallsOpenWithoutAuthContext: direct
// middleware call without authed() upstream lets the request through.
// Covers the test-harness contract and any future public route that
// gets wrapped incorrectly.
func TestRateLimitManifestMiddleware_FallsOpenWithoutAuthContext(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	cfg.Limits.Manifest.RequestsPerMinute = 60
	cfg.Limits.Manifest.Burst = 1
	srv := New(cfg, nil, nil, "fp")

	called := false
	h := srv.rateLimitManifest(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	h(rr, req)

	if !called {
		t.Error("downstream handler should run with no token in context")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestRateLimitManifestMiddleware_429WithRetryAfter: with a real authed
// chain and burst=1, the second back-to-back request must come back as
// 429 + Retry-After + the `rate_limited` typed error code.
func TestRateLimitManifestMiddleware_429WithRetryAfter(t *testing.T) {
	srv, raw := newRateLimitedTestServer(t, 60, 1)
	defer srv.Close()

	// First call passes through to the manifest handler. Provider is
	// nil so the handler returns 503 — what we care about is that
	// it's NOT a rate-limit response.
	resp := manifestRequest(t, srv, raw)
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("first call should not be rate-limited")
	}
	resp.Body.Close()

	// Second call lands inside the deferred window → 429.
	resp = manifestRequest(t, srv, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Error("Retry-After header missing on 429")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want positive integer seconds", ra)
	}
	var er ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if er.Error != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", er.Error)
	}
}

// TestRateLimitManifestMiddleware_PerClientIsolation: tokens A and B
// share the bridge — burning A's burst MUST NOT block B.
func TestRateLimitManifestMiddleware_PerClientIsolation(t *testing.T) {
	srv, raw := newRateLimitedTestServer(t, 60, 1)
	defer srv.Close()

	// Mint a second token against the same auth store. Reuse the same
	// httptest server (the store is shared with the server's New call,
	// via the closure inside newRateLimitedTestServer).
	rawB := mintSecondToken(t, srv)

	// Exhaust A.
	r := manifestRequest(t, srv, raw)
	r.Body.Close()
	r = manifestRequest(t, srv, raw)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("A's 2nd call: status = %d, want 429", r.StatusCode)
	}
	r.Body.Close()

	// B's first call must not be 429.
	r = manifestRequest(t, srv, rawB)
	defer r.Body.Close()
	if r.StatusCode == http.StatusTooManyRequests {
		t.Errorf("B inherited A's 429: status = %d", r.StatusCode)
	}
}

// TestRateLimitManifestMiddleware_DisabledFallsOpen: rpm=0 in config
// disables the limiter and every call passes through.
func TestRateLimitManifestMiddleware_DisabledFallsOpen(t *testing.T) {
	srv, raw := newRateLimitedTestServer(t, 0, 0)
	defer srv.Close()

	for i := 0; i < 10; i++ {
		r := manifestRequest(t, srv, raw)
		if r.StatusCode == http.StatusTooManyRequests {
			r.Body.Close()
			t.Fatalf("iteration %d: limiter should be disabled, got 429", i)
		}
		r.Body.Close()
	}
}

// --- shared local helpers (only this test file's tests use them) ---

// authStoreForRateLimitTest is captured in the closure so mintSecondToken
// can reach into the SAME store the running server is validating against.
// This is a package-private test seam.
var rateLimitAuthStore *auth.Store

func newRateLimitedTestServer(t *testing.T, rpm, burst int) (*httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	cfg.Limits.Manifest.RequestsPerMinute = rpm
	cfg.Limits.Manifest.Burst = burst
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	rateLimitAuthStore = store
	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		rateLimitAuthStore = nil
	})
	return hs, raw
}

func mintSecondToken(t *testing.T, _ *httptest.Server) string {
	t.Helper()
	if rateLimitAuthStore == nil {
		t.Fatal("rateLimitAuthStore not initialised — call newRateLimitedTestServer first")
	}
	raw, _, err := rateLimitAuthStore.Mint("test-B")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func manifestRequest(t *testing.T, hs *httptest.Server, raw string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
