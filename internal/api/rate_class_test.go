package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRouteRegistry_everyRouteHasARateClass is the whole point of the
// rateClass field. The zero value is INVALID by construction, so a new
// route that forgets to choose fails here rather than shipping unlimited.
//
// This is the opposite arrangement to `kind`, where the zero value
// (boundedRoute) is the safe default and a forgotten choice is harmless.
// Here the permissive answer is the dangerous one.
func TestRouteRegistry_everyRouteHasARateClass(t *testing.T) {
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		if rt.rateClass == rateUnset {
			t.Errorf("route %q has no rateClass. Choose one deliberately:\n"+
				"  rateWrite    — it mutates state\n"+
				"  rateNone     — unauthenticated, self-limiting, or a legitimate high-burst read\n"+
				"  rateManifest — the bespoke /v1/manifest bucket", rt.pattern)
		}
	}
}

// TestRouteRegistry_everyMutatingRouteIsRateWrite pins the classification
// by METHOD rather than by a hand-listed set, so a new POST/PUT/DELETE
// route cannot be added as rateNone by omission — it has to be argued for
// in the exemption list below, in this file, where someone will read it.
func TestRouteRegistry_everyMutatingRouteIsRateWrite(t *testing.T) {
	// Mutating routes that legitimately draw from no bucket.
	exempt := map[string]string{
		// Pairing carries its own per-IP limiter (pairingRateLimiter) and
		// is UNAUTHENTICATED, so there is no token to key a bucket on.
		"POST /v1/pairing/requests":      "unauthenticated; own per-IP limiter",
		"DELETE /v1/pairing/{requestID}": "unauthenticated; own per-IP limiter",
	}
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		method, _, _ := strings.Cut(rt.pattern, " ")
		switch method {
		case "POST", "PUT", "PATCH", "DELETE":
		default:
			continue
		}
		if why, ok := exempt[rt.pattern]; ok {
			if rt.rateClass != rateNone {
				t.Errorf("route %q is listed exempt (%s) but is not rateNone", rt.pattern, why)
			}
			continue
		}
		if rt.rateClass != rateWrite {
			t.Errorf("mutating route %q is not rateWrite. Either give it rateWrite, "+
				"or add it to the exemption list in this test WITH a reason.", rt.pattern)
		}
	}
}

// TestRouteRegistry_readsAreNotWriteLimited is the inverse guard. A GET
// accidentally classified rateWrite would draw from the mutation budget
// and could 429 a sync — which iOS does not retry.
func TestRouteRegistry_readsAreNotWriteLimited(t *testing.T) {
	s := newRouteRegistryTestServer(t)
	for _, rt := range s.routeRegistry() {
		method, _, _ := strings.Cut(rt.pattern, " ")
		if method == "GET" && rt.rateClass == rateWrite {
			t.Errorf("read route %q is rateWrite; reads must not draw from the mutation budget", rt.pattern)
		}
	}
}

// TestWriteLimiterAbsorbsASyncBurstThenBounds is the sizing assertion, and
// it is the one that matters most: iOS surfaces a 429 as a transport error
// and does NOT retry, so a bucket sized too tight does not slow a sync
// down — it breaks it.
//
// The traffic shape is a BURST, not a rate: a device returning from an
// offline window pushes every dirty playlist at once and then goes quiet.
func TestWriteLimiterAbsorbsASyncBurstThenBounds(t *testing.T) {
	l := newTokenRateLimiter(120, 300)

	// A burst far larger than any real sync must pass without a single
	// refusal.
	const realisticBurst = 250
	for i := 0; i < realisticBurst; i++ {
		if _, ok := reserveOrRetryAfter(l, "tok"); !ok {
			t.Fatalf("refused request %d of a %d-request burst; a real sync would break here",
				i+1, realisticBurst)
		}
	}

	// A runaway client is still bounded: past the burst, refusals start.
	refused := 0
	for i := 0; i < 500; i++ {
		if _, ok := reserveOrRetryAfter(l, "tok"); !ok {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("a 500-request runaway was never refused; the limiter bounds nothing")
	}

	// Buckets are per token — one misbehaving client must not spend
	// another's budget.
	if _, ok := reserveOrRetryAfter(l, "other-token"); !ok {
		t.Error("a second token was refused; buckets must be per-token")
	}
}

func TestReserveOrRetryAfterRoundsUpAndDoesNotDrainOnRefusal(t *testing.T) {
	// rate 60/min = 1/s, burst 1: the second call must be refused.
	l := newTokenRateLimiter(60, 1)
	if _, ok := reserveOrRetryAfter(l, "tok"); !ok {
		t.Fatal("first call refused with a full bucket")
	}
	retry, ok := reserveOrRetryAfter(l, "tok")
	if ok {
		t.Fatal("second call allowed; burst of 1 should be exhausted")
	}
	if retry < 1 {
		t.Errorf("Retry-After = %d; must round UP to at least 1s, or a compliant client "+
			"wakes before a token exists and collects a second 429", retry)
	}
	// The refusal must NOT have consumed a token: a second refusal should
	// advertise the same window, not a longer one.
	retry2, ok := reserveOrRetryAfter(l, "tok")
	if ok {
		t.Fatal("third call allowed unexpectedly")
	}
	if retry2 > retry {
		t.Errorf("refusal drained the bucket: Retry-After grew %d -> %d across two refused attempts",
			retry, retry2)
	}
}

// TestWriteLimitDisabledFallsOpen pins the documented opt-out.
func TestWriteLimitDisabledFallsOpen(t *testing.T) {
	l := newTokenRateLimiter(0, 1)
	if !l.disabled() {
		t.Fatal("rpm=0 must disable the limiter")
	}
}

// TestRateLimitWriteFallsOpenWithoutAToken mirrors the manifest limiter's
// convention: no authed() upstream means the middleware does not apply.
func TestRateLimitWriteFallsOpenWithoutAToken(t *testing.T) {
	s := newRouteRegistryTestServer(t)
	s.writeRateLimiter = newTokenRateLimiter(60, 1)

	called := 0
	h := s.rateLimitWrite(func(w http.ResponseWriter, r *http.Request) { called++ })
	for i := 0; i < 5; i++ {
		h(httptest.NewRecorder(), httptest.NewRequest("PUT", "/v1/favorites", nil))
	}
	if called != 5 {
		t.Errorf("handler ran %d/5 times without a token in context; the middleware must fall open", called)
	}
}
