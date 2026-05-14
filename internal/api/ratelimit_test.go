package api

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestManifestLimitsConfig_EffectiveRPMPreservesExplicitZero is the
// regression guard for Gemini HIGH / Greptile P1 on PR #194: PROTOCOL.md
// documents `limits.manifest.requestsPerMinute: 0` as the limiter
// opt-out. With the pre-fix bare-int field, applyDefaults silently
// overrode operator zeros with the default value of 6. The pointer-
// typed field + EffectiveRPM helper must preserve explicit zero while
// applying the default only on missing fields (pointer == nil).
func TestManifestLimitsConfig_EffectiveRPMPreservesExplicitZero(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.ManifestLimitsConfig
		want int
	}{
		{
			name: "missing field → default",
			cfg:  config.ManifestLimitsConfig{},
			want: config.DefaultManifestRequestsPerMinute,
		},
		{
			name: "explicit zero → preserved (opt-out)",
			cfg:  config.ManifestLimitsConfig{RequestsPerMinute: intPtr(0)},
			want: 0,
		},
		{
			name: "explicit non-zero → preserved",
			cfg:  config.ManifestLimitsConfig{RequestsPerMinute: intPtr(120)},
			want: 120,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveRPM(); got != tc.want {
				t.Errorf("EffectiveRPM = %d, want %d", got, tc.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

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
	rpm, burst := 60, 1
	cfg.Limits.Manifest.RequestsPerMinute = &rpm
	cfg.Limits.Manifest.Burst = &burst
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
	srv, raw, _ := newRateLimitedTestServer(t, 60, 1)
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
	srv, raw, store := newRateLimitedTestServer(t, 60, 1)
	defer srv.Close()

	// Mint a second token against the same auth store the test server
	// is validating against. Threading the store explicitly (rather
	// than through a package-level handoff) keeps the test parallel-
	// safe under any future t.Parallel() addition (Greptile P2 on
	// PR #194).
	rawB := mintToken(t, store, "test-B")

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

// TestRateLimitManifestMiddleware_PaginationBypass: paginated requests
// (those carrying ?cursor= or ?limit=) are inherently client-paced —
// the iOS app pulls the next page only after parsing the prior one —
// so they're exempt from the bucket. A 50k-track library is ~50
// pages, far above any reasonable burst budget; without this bypass
// the limiter terminates legitimate paginated full-rescans after the
// first few pages and iOS surfaces it as a transport error.
//
// Burst=1 here so the underlying limiter would 429 any 2nd unmarked
// call. The pagination calls must succeed regardless.
func TestRateLimitManifestMiddleware_PaginationBypass(t *testing.T) {
	srv, raw, _ := newRateLimitedTestServer(t, 60, 1)
	defer srv.Close()

	// 10 sequential paginated requests with ?cursor=...; none should
	// trip the limiter. Provider is nil → 503 from the handler — we
	// only care that NONE come back as 429.
	for i := 0; i < 10; i++ {
		resp := manifestRequestWithQuery(t, srv, raw, fmt.Sprintf("cursor=page-%d", i))
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			t.Fatalf("cursor request %d should bypass rate limit, got 429", i)
		}
		resp.Body.Close()
	}

	// 10 more with ?limit=1000; same expectation. Exercises the OR
	// branch of the bypass gate independently.
	for i := 0; i < 10; i++ {
		resp := manifestRequestWithQuery(t, srv, raw, "limit=1000")
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			t.Fatalf("limit request %d should bypass rate limit, got 429", i)
		}
		resp.Body.Close()
	}

	// And finally — a NON-paginated request still pays the limit.
	// First call burns the burst (passes); the second-back-to-back
	// no-params call must 429. Confirms the bypass is targeted, not
	// a blanket disable.
	resp := manifestRequest(t, srv, raw)
	resp.Body.Close()
	resp = manifestRequest(t, srv, raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("no-query 2nd call should still 429, got %d", resp.StatusCode)
	}
}

// TestRateLimitManifestMiddleware_DisabledFallsOpen: rpm=0 in config
// disables the limiter and every call passes through.
func TestRateLimitManifestMiddleware_DisabledFallsOpen(t *testing.T) {
	srv, raw, _ := newRateLimitedTestServer(t, 0, 0)
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

// newRateLimitedTestServer builds an authenticated test server with the
// configured RPM / burst. Returns the server, an initial bearer token,
// and the underlying auth store so callers that want a second token
// can mint one without reaching into package-global state.
//
// rpm and burst use pointer semantics so an explicit zero (operator
// opt-out) is preserved across the config layer's EffectiveRPM /
// EffectiveBurst helpers.
func newRateLimitedTestServer(t *testing.T, rpm, burst int) (*httptest.Server, string, *auth.Store) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":0",
		LibraryName:   "X",
	}
	cfg.Limits.Manifest.RequestsPerMinute = &rpm
	cfg.Limits.Manifest.Burst = &burst
	store, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, store
}

// mintToken issues a fresh bearer against the given auth store. Tests
// that need a second token thread the store explicitly so there's no
// package-level state to race on under future t.Parallel() additions.
func mintToken(t *testing.T, store *auth.Store, name string) string {
	t.Helper()
	raw, _, err := store.Mint(name)
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

// manifestRequestWithQuery is the same as manifestRequest but appends
// arbitrary query parameters. Used by the pagination-bypass test to
// exercise the `?cursor=` / `?limit=` exemption.
func manifestRequestWithQuery(t *testing.T, hs *httptest.Server, raw, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/manifest?"+query, nil)
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
