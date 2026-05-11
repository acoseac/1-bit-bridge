package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimitKey is the unexported context key used to thread an
// authenticated token ID from the authed() middleware into the
// downstream rate limit. Local to this file so it doesn't collide
// with future per-request keys other middleware may add.
type rateLimitKey int

const (
	ctxKeyTokenID rateLimitKey = iota
)

// withTokenID returns a child context carrying tokenID. authed() calls
// this on every successful validation so downstream middleware (today
// just rateLimitManifest) can read the identifier without re-running
// the bearer extraction.
func withTokenID(ctx context.Context, tokenID string) context.Context {
	return context.WithValue(ctx, ctxKeyTokenID, tokenID)
}

// tokenIDFromContext returns the validated token ID, or "" if no
// authed() middleware ran on the request (test harnesses, or a future
// public route that doesn't authenticate). The rate limiter falls open
// on empty IDs — see rateLimitManifest's docblock for the rationale.
func tokenIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTokenID).(string); ok {
		return v
	}
	return ""
}

// manifestLimiterCleanupInterval is how often idle per-token limiters
// are reaped from the in-memory map. A long-running bridge with a high
// churn of paired clients would otherwise accumulate one *rate.Limiter
// per ever-seen token forever.
const manifestLimiterCleanupInterval = 10 * time.Minute

// manifestLimiterIdleTimeout is how long a limiter must go untouched
// before it's eligible for cleanup. Must be > a typical pagination
// interval; 1 hour is generous and keeps a returning user's burst
// budget intact across normal day-to-day use.
const manifestLimiterIdleTimeout = 1 * time.Hour

// manifestRateLimiter is the per-token-ID token-bucket cache for the
// /v1/manifest rate limit. Created once at Server construction time
// and the reaper goroutine started via Start().
//
// Defense-in-depth, not security: every /v1/manifest caller IS already
// authenticated. The limiter protects against a paired client that
// misbehaves (buggy iOS build, a future web admin issuing repeated
// full-manifest dumps) from exhausting bridge CPU + bandwidth on
// 100+ MB manifest streams. Per-token rather than per-IP because IP
// is unreliable behind Tailscale CGNAT and the token IS the actual
// identity boundary.
type manifestRateLimiter struct {
	mu         sync.Mutex
	entries    map[string]*manifestLimiterEntry
	rate       rate.Limit
	burst      int
	reaperOnce sync.Once // enforces Start's "called multiple times → no-op" contract
}

type manifestLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newManifestRateLimiter constructs the limiter with the given budget.
// rpm <= 0 disables the limiter entirely (every Reserve() returns an
// effectively-zero delay) — operator opt-out path for bridges where
// the legitimate single-client traffic is bursty enough that any
// limit causes friction. burst <= 0 falls back to 1.
func newManifestRateLimiter(rpm, burst int) *manifestRateLimiter {
	r := rate.Limit(0)
	if rpm > 0 {
		r = rate.Limit(float64(rpm) / 60.0)
	}
	if burst < 1 {
		burst = 1
	}
	return &manifestRateLimiter{
		entries: make(map[string]*manifestLimiterEntry),
		rate:    r,
		burst:   burst,
	}
}

// limiterFor returns the per-token-ID limiter, creating it lazily on
// first call. Updates the lastSeen stamp for the reaper.
func (m *manifestRateLimiter) limiterFor(tokenID string) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[tokenID]; ok {
		e.lastSeen = time.Now()
		return e.limiter
	}
	lim := rate.NewLimiter(m.rate, m.burst)
	m.entries[tokenID] = &manifestLimiterEntry{
		limiter:  lim,
		lastSeen: time.Now(),
	}
	return lim
}

// reapIdle drops entries whose lastSeen is older than
// manifestLimiterIdleTimeout. Caller MUST hold m.mu.
func (m *manifestRateLimiter) reapIdle(now time.Time) int {
	dropped := 0
	for k, e := range m.entries {
		if now.Sub(e.lastSeen) > manifestLimiterIdleTimeout {
			delete(m.entries, k)
			dropped++
		}
	}
	return dropped
}

// Start launches the periodic-reaper goroutine. Returns a cancel
// function the caller invokes at shutdown — keeps the goroutine
// scoped to the Server's lifetime and prevents test leakage. Safe
// to call multiple times: subsequent calls are no-ops enforced by a
// sync.Once. The original docstring claimed idempotence but didn't
// actually enforce it — every call spawned a fresh reaper, leaking
// goroutines on accidental re-entry. Caught by Gemini Medium +
// Greptile P2 on PR #194.
func (m *manifestRateLimiter) Start(stop <-chan struct{}) {
	m.reaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(manifestLimiterCleanupInterval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					m.mu.Lock()
					m.reapIdle(now)
					m.mu.Unlock()
				}
			}
		}()
	})
}

// disabled reports whether the limiter is configured to allow everything.
// rate <= 0 means "no budget" — the bucket never fills, but the burst
// still lets some requests through. We treat rate <= 0 + burst > 0 as
// "disabled" so an operator's rpm=0 setting falls open instead of
// hard-failing the very first manifest call.
func (m *manifestRateLimiter) disabled() bool {
	return m.rate <= 0
}

// rateLimitManifest is the HTTP middleware applied to /v1/manifest.
// MUST be wrapped AFTER authed() so the validated token ID is in
// context — keying the bucket on Token.ID (rather than IP) is the
// whole point of the design.
//
// On exceeded: 429 with a Retry-After header derived from the
// limiter's Reserve().Delay() (the honest cooldown), Content-Type
// application/json, body `{"error":"rate_limited","message":"..."}`.
// iOS surfaces 429 as a transport error today (no typed handling) —
// a Mirror-PR follow-up can add Retry-After parsing and a typed
// BridgeError.rateLimited case.
//
// Empty token ID (no authed() upstream — shouldn't happen in
// production but covers test harnesses + future unauthenticated
// routes that get incorrectly wrapped) falls open: limiter does
// nothing and the request proceeds. Better than a hard 401 when
// the right shape is "this middleware doesn't apply to you".
func (s *Server) rateLimitManifest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.manifestRateLimiter == nil || s.manifestRateLimiter.disabled() {
			next(w, r)
			return
		}
		tokenID := tokenIDFromContext(r.Context())
		if tokenID == "" {
			next(w, r)
			return
		}
		lim := s.manifestRateLimiter.limiterFor(tokenID)
		res := lim.Reserve()
		// Reserve() always returns OK==true for a non-zero rate, so
		// the honest check is whether Delay() exceeds zero. A zero
		// delay means the bucket had capacity; non-zero means the
		// caller would need to sleep to consume the token, which is
		// our cue to respond 429.
		delay := res.Delay()
		if delay > 0 {
			// Cancel the reservation so the bucket doesn't drain on
			// a 429-rejected attempt — without this, every 429 also
			// counts as a consumed token and the recovery window
			// stretches out further than the limit advertises.
			res.Cancel()
			retry := int(delay.Seconds())
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many manifest requests; retry after the Retry-After window")
			return
		}
		next(w, r)
	}
}
