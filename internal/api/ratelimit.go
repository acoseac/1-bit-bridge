package api

import (
	"context"
	"math"
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
	ctxKeyDeviceToken
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

// withDeviceToken / deviceTokenFromContext thread the validated
// X-Device-Token header (the client's durable recovery token) from
// authed() into the playlist + history handlers, which scope per-device
// state by it. "" when the header was absent or malformed; those handlers
// then reject with a 400 since per-device endpoints require it.
func withDeviceToken(ctx context.Context, deviceToken string) context.Context {
	return context.WithValue(ctx, ctxKeyDeviceToken, deviceToken)
}

func deviceTokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyDeviceToken).(string); ok {
		return v
	}
	return ""
}

// tokenLimiterCleanupInterval is how often idle per-token limiters
// are reaped from the in-memory map. A long-running bridge with a high
// churn of paired clients would otherwise accumulate one *rate.Limiter
// per ever-seen token forever.
const tokenLimiterCleanupInterval = 10 * time.Minute

// tokenLimiterIdleTimeout is how long a limiter must go untouched
// before it's eligible for cleanup. Must be > a typical pagination
// interval; 1 hour is generous and keeps a returning user's burst
// budget intact across normal day-to-day use.
const tokenLimiterIdleTimeout = 1 * time.Hour

// tokenRateLimiter is a per-token-ID token-bucket cache. One instance
// backs /v1/manifest; a second backs the mutating routes (see
// rateClass). Created at Server construction time, with the reaper
// goroutine started via Start().
//
// Defense-in-depth, not security: every /v1/manifest caller IS already
// authenticated. The limiter protects against a paired client that
// misbehaves (buggy iOS build, a future web admin issuing repeated
// full-manifest dumps) from exhausting bridge CPU + bandwidth on
// 100+ MB manifest streams. Per-token rather than per-IP because IP
// is unreliable behind Tailscale CGNAT and the token IS the actual
// identity boundary.
type tokenRateLimiter struct {
	mu         sync.Mutex
	entries    map[string]*tokenLimiterEntry
	rate       rate.Limit
	burst      int
	reaperOnce sync.Once // enforces Start's "called multiple times → no-op" contract
}

type tokenLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newTokenRateLimiter constructs the limiter with the given budget.
// rpm <= 0 disables the limiter entirely (every Reserve() returns an
// effectively-zero delay) — operator opt-out path for bridges where
// the legitimate single-client traffic is bursty enough that any
// limit causes friction. burst <= 0 falls back to 1.
func newTokenRateLimiter(rpm, burst int) *tokenRateLimiter {
	r := rate.Limit(0)
	if rpm > 0 {
		r = rate.Limit(float64(rpm) / 60.0)
	}
	if burst < 1 {
		burst = 1
	}
	return &tokenRateLimiter{
		entries: make(map[string]*tokenLimiterEntry),
		rate:    r,
		burst:   burst,
	}
}

// limiterFor returns the per-token-ID limiter, creating it lazily on
// first call. Updates the lastSeen stamp for the reaper.
func (m *tokenRateLimiter) limiterFor(tokenID string) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[tokenID]; ok {
		e.lastSeen = time.Now()
		return e.limiter
	}
	lim := rate.NewLimiter(m.rate, m.burst)
	m.entries[tokenID] = &tokenLimiterEntry{
		limiter:  lim,
		lastSeen: time.Now(),
	}
	return lim
}

// reapIdle drops entries whose lastSeen is older than
// tokenLimiterIdleTimeout. Caller MUST hold m.mu.
func (m *tokenRateLimiter) reapIdle(now time.Time) int {
	dropped := 0
	for k, e := range m.entries {
		if now.Sub(e.lastSeen) > tokenLimiterIdleTimeout {
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
func (m *tokenRateLimiter) Start(stop <-chan struct{}) {
	m.reaperOnce.Do(func() {
		go func() {
			t := time.NewTicker(tokenLimiterCleanupInterval)
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
func (m *tokenRateLimiter) disabled() bool {
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
		// Paginated requests are inherently client-paced: the iOS app
		// pulls the next page only after parsing the prior one (each
		// page returns up to 1000 tracks; a 50k-track library is ~50
		// HTTP round-trips). Burst budget — tuned for "a single
		// full-manifest re-pull after an offline window" — is exhausted
		// after 3 pages, then every subsequent page 429s and the
		// rescan terminates because iOS surfaces 429 as a transport
		// error and does not retry. The limiter's stated purpose is
		// defense against a misbehaving client issuing repeated
		// full-manifest dumps; paginated streams are NOT that traffic
		// shape (they're already paced by per-page processing time),
		// so exempting them is correct. Single-shot /v1/manifest with
		// no query params still pays the limit.
		//
		// Gate tied STRICTLY to a non-empty `limit` query param,
		// mirroring the handler's actual pagination predicate at
		// `manifestHandler` (api.go:803 — `if limitRaw != ""`). A
		// bare `?cursor=…` without `?limit=…` is NOT treated as
		// paginated server-side: the handler falls through to the
		// legacy single-shot full-manifest path, which is exactly
		// the runaway-dump case the limiter is built to constrain.
		// Per Gemini medium on PR #235: a `cursor`-OR-`limit` gate
		// would let a misbehaving authed client send `?cursor=anything`
		// to bypass the bucket while still receiving the full dump.
		// safeQuery for uniform query-handling discipline (limit is
		// numeric so there's no functional difference, but every other
		// handler routes through it).
		if safeQuery(r).Get("limit") != "" {
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
			// math.Ceil, not a bare int() truncation: a 1.9s delay
			// truncated to 1 advertises a 1s Retry-After, so a compliant
			// client sleeps 1s, wakes before the bucket has a token, and
			// gets another 429. Round up so the advertised window actually
			// clears the reservation. delay > 0 is guaranteed by the check
			// above, so math.Ceil(delay.Seconds()) is always >= 1 — no
			// separate floor is needed to avoid a 0s (immediate-hammer)
			// window.
			retry := int(math.Ceil(delay.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many manifest requests; retry after the Retry-After window")
			return
		}
		next(w, r)
	}
}

// rateClass says which bucket a route draws from. It is declared on every
// entry in routeRegistry, and the classification test REFUSES rateUnset —
// the zero value is deliberately invalid, because unlike routeKind (whose
// zero, boundedRoute, is the safe default) the permissive answer here is
// the dangerous one. A new route that forgets to choose would silently get
// no limit at all, which is precisely the gap this closes.
type rateClass int

const (
	// rateUnset is the zero value and is never valid. See above.
	rateUnset rateClass = iota

	// rateNone is a deliberate "no bucket": unauthenticated routes,
	// routes that carry their own limiter (pairing), and reads whose
	// legitimate traffic shape is a burst of hundreds (the artwork
	// sweep). Choosing it is a decision; forgetting to choose is not.
	rateNone

	// rateManifest is the bespoke /v1/manifest bucket, which keeps its
	// own budget and its own pagination exemption.
	rateManifest

	// rateWrite is every mutating route. See writeLimitDefaults for how
	// the budget was sized and why it is so generous.
	rateWrite

	// rateSearch is the per-keystroke read bucket — /v1/search only.
	// Reads are otherwise rateNone because the artwork sweep legitimately
	// bursts hundreds of requests; search is the exception, because a
	// client calls it as fast as someone types, so it deserves a bound
	// the other reads would be broken by.
	rateSearch
)

// rateLimitWrite is the middleware for rateWrite routes. Same mechanics as
// rateLimitManifest — per-token bucket, cancel the reservation on refusal,
// Retry-After rounded up — but a separate budget, because a manifest pull
// and a playlist push have nothing in common except the token.
//
// MUST be wrapped AFTER authed() so the validated token ID is in context.
// An empty token ID falls open, per the existing convention.
func (s *Server) rateLimitWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.writeRateLimiter == nil || s.writeRateLimiter.disabled() {
			next(w, r)
			return
		}
		tokenID := tokenIDFromContext(r.Context())
		if tokenID == "" {
			next(w, r)
			return
		}
		if retry, ok := reserveOrRetryAfter(s.writeRateLimiter, tokenID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many write requests; retry after the Retry-After window")
			return
		}
		next(w, r)
	}
}

// reserveOrRetryAfter takes one token from the caller's bucket. Returns
// ok=false plus the whole seconds a compliant client should wait.
//
// The reservation is CANCELLED on refusal so a rejected attempt does not
// also drain the bucket — without that, every 429 stretches the recovery
// window past what the response advertises. The delay rounds UP, because a
// truncated 1.9s→1s window has the client wake before a token exists and
// collect a second 429.
func reserveOrRetryAfter(l *tokenRateLimiter, tokenID string) (retryAfterSec int, ok bool) {
	res := l.limiterFor(tokenID).Reserve()
	delay := res.Delay()
	if delay <= 0 {
		return 0, true
	}
	res.Cancel()
	return int(math.Ceil(delay.Seconds())), false
}

// rateLimitSearch is the middleware for rateSearch routes. Its own bucket
// rather than the write one: a search is a read, and sharing the write
// budget would let a person typing consume the budget their playlist push
// needs.
func (s *Server) rateLimitSearch(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.searchRateLimiter == nil || s.searchRateLimiter.disabled() {
			next(w, r)
			return
		}
		tokenID := tokenIDFromContext(r.Context())
		if tokenID == "" {
			next(w, r)
			return
		}
		if retry, ok := reserveOrRetryAfter(s.searchRateLimiter, tokenID); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many search requests; retry after the Retry-After window")
			return
		}
		next(w, r)
	}
}
