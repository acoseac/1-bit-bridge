package enrich

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

// TestIsTransient_PinsClassification covers the contract added by PR
// #N: transient infrastructure failures (timeouts, 5xx, 429, conn-
// resets, deadline-exceeded) MUST be classified as retryable so the
// enricher leaves `enriched_at` at 0; persistent failures (404, JSON
// decode, 4xx-other-than-429) MUST NOT, so the worker doesn't loop
// indefinitely on guaranteed-fail errors.
//
// Pre-fix, `enrichOne` called `markSkipped` on every SearchRelease
// error — a 30-second MB outage permanently poisoned every track
// currently being enriched. The classifier in `IsTransient`
// reverses that for the genuine-transient subset only.
func TestIsTransient_PinsClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil — not transient", nil, false},

		// Cancellation is NOT transient — the caller cancelled
		// (shutdown / pause / similar). enricher already handles
		// this branch separately by checking ctx.Err() before
		// IsTransient is consulted.
		{"context canceled", context.Canceled, false},

		// Deadline-exceeded IS transient. The enricher's batch
		// loop applies a per-call deadline; hitting it means the
		// MB upstream took too long, not that the track is
		// permanently bad.
		{"context deadline exceeded", context.DeadlineExceeded, true},

		// net.Error with Timeout() == true — the most common shape
		// for connect / read timeouts surfaced by net/http.
		{"net timeout", &fakeNetErr{timeout: true}, true},
		// A net.Error without Timeout() (e.g., DNS NXDOMAIN) is
		// not classified as transient by the timeout check alone —
		// arguable but simple, and avoids over-retrying genuinely
		// broken DNS state. The host-level errors below pick up
		// the cases we care about.
		{"net non-timeout", &fakeNetErr{timeout: false}, false},

		// TCP-level resets / aborts are transient — the upstream
		// closed the socket mid-handshake, not a permanent state.
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"ECONNABORTED", syscall.ECONNABORTED, true},
		{"EPIPE", syscall.EPIPE, true},
		{"ETIMEDOUT", syscall.ETIMEDOUT, true},

		// HTTP status codes flow through the typed `*httpError` returned
		// by `MusicBrainzClient.get` and are matched via `errors.As`.
		{"typed httpError 500", &httpError{StatusCode: 500, Body: "server error"}, true},
		{"typed httpError 502", &httpError{StatusCode: 502, Body: "bad gateway"}, true},
		{"typed httpError 503", &httpError{StatusCode: 503, Body: "service unavailable"}, true},
		{"typed httpError 504", &httpError{StatusCode: 504, Body: "gateway timeout"}, true},
		{"typed httpError 429", &httpError{StatusCode: 429, Body: "too many requests"}, true},

		// Persistent statuses must NOT be classified as transient
		// — they will fail every retry forever and the worker
		// would loop indefinitely. errNotFound is the typed 404
		// already; check 4xx-other-than-429 here.
		{"typed httpError 400", &httpError{StatusCode: 400, Body: "bad request"}, false},
		{"typed httpError 401", &httpError{StatusCode: 401, Body: "unauthorized"}, false},
		{"typed httpError 403", &httpError{StatusCode: 403, Body: "forbidden"}, false},
		// errNotFound is the 404 path; verify both the typed
		// shape and the wrapped form get handled.
		{"errNotFound", errNotFound, false},
		{"wrapped not-found", fmt.Errorf("lookup: %w", errNotFound), false},

		// Wrap-survival: this is the regression the typed-httpError
		// shape closes. Pre-fix `IsTransient` did
		// `strings.HasPrefix(err.Error(), "musicbrainz: HTTP ")` over
		// the formatted message — any caller that wrapped the MB
		// error (`fmt.Errorf("retry: %w", err)`) silently broke the
		// prefix match, returned false on a real 5xx, and the
		// enricher poisoned the track exactly as PR #74 was
		// supposed to prevent. `errors.As` survives any depth of
		// wrap.
		{"wrapped typed 503 (regression net for PR #74-class poisoning)",
			fmt.Errorf("retry attempt: %w", &httpError{StatusCode: 503, Body: "x"}),
			true},
		{"double-wrapped typed 429",
			fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &httpError{StatusCode: 429, Body: "y"})),
			true},
		{"wrapped typed 400 stays persistent",
			fmt.Errorf("retry attempt: %w", &httpError{StatusCode: 400, Body: "z"}),
			false},

		// Decode errors / schema drift — guaranteed to fail every
		// retry, must not be classified as transient.
		{"JSON decode error", errors.New("invalid character ',' looking for beginning of value"), false},
		{"unrelated text", errors.New("some other thing went wrong"), false},

		// Plain-string errors that look like the old format string
		// no longer match — the typed shape is the only signal now.
		// This is intentional: the prior substring/prefix path was
		// brittle, and any caller that needs to mark a custom error
		// transient should attach a `*httpError` (or extend
		// `IsTransient` with another typed predicate).
		{"plain-string HTTP 503 no longer matches", errors.New("musicbrainz: HTTP 503: service unavailable"), false},
		{"body containing HTTP 503 elsewhere", errors.New("page says: HTTP 503 was returned"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestHTTPErrorFormatStability pins the `httpError.Error()` rendering
// byte-for-byte. The typed-shape refactor explicitly preserves the
// prior `fmt.Errorf("musicbrainz: HTTP %d: %s", ...)` format so log
// lines and any external tooling that scrapes them don't drift; this
// test fails loudly if a future change to `Error()` breaks that
// promise (coderabbit nit on PR #144). The classification cases above
// all go through `IsTransient` and would not catch a format regression
// in the rendered string itself.
func TestHTTPErrorFormatStability(t *testing.T) {
	cases := []struct {
		err  *httpError
		want string
	}{
		{&httpError{StatusCode: 503, Body: "service unavailable"}, "musicbrainz: HTTP 503: service unavailable"},
		{&httpError{StatusCode: 429, Body: "too many requests"}, "musicbrainz: HTTP 429: too many requests"},
		{&httpError{StatusCode: 400, Body: ""}, "musicbrainz: HTTP 400: "},
		{&httpError{StatusCode: 404, Body: "not found"}, "musicbrainz: HTTP 404: not found"},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}

// fakeNetErr satisfies net.Error for the timeout-classification test
// cases above without dragging in the full net.OpError plumbing.
type fakeNetErr struct {
	timeout bool
}

func (e *fakeNetErr) Error() string   { return "fake net error" }
func (e *fakeNetErr) Timeout() bool   { return e.timeout }
func (e *fakeNetErr) Temporary() bool { return e.timeout }

// Compile-time check.
var _ net.Error = (*fakeNetErr)(nil)
