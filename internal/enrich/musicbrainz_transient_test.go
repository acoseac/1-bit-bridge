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

		// HTTP status codes are encoded in the error string by
		// the MB client's Do formatter ("musicbrainz: HTTP NNN: …").
		{"HTTP 500", fmt.Errorf("musicbrainz: HTTP 500: server error"), true},
		{"HTTP 502", fmt.Errorf("musicbrainz: HTTP 502: bad gateway"), true},
		{"HTTP 503", fmt.Errorf("musicbrainz: HTTP 503: service unavailable"), true},
		{"HTTP 504", fmt.Errorf("musicbrainz: HTTP 504: gateway timeout"), true},
		{"HTTP 429", fmt.Errorf("musicbrainz: HTTP 429: too many requests"), true},

		// Persistent statuses must NOT be classified as transient
		// — they will fail every retry forever and the worker
		// would loop indefinitely. errNotFound is the typed 404
		// already; check 4xx-other-than-429 here.
		{"HTTP 400", fmt.Errorf("musicbrainz: HTTP 400: bad request"), false},
		{"HTTP 401", fmt.Errorf("musicbrainz: HTTP 401: unauthorized"), false},
		{"HTTP 403", fmt.Errorf("musicbrainz: HTTP 403: forbidden"), false},
		// errNotFound is the 404 path; verify both the typed
		// shape and the wrapped form get handled.
		{"errNotFound", errNotFound, false},
		{"wrapped not-found", fmt.Errorf("lookup: %w", errNotFound), false},

		// Decode errors / schema drift — guaranteed to fail every
		// retry, must not be classified as transient.
		{"JSON decode error", errors.New("invalid character ',' looking for beginning of value"), false},
		{"unrelated text", errors.New("some other thing went wrong"), false},

		// Bodies that mention "HTTP 503" without the canonical
		// prefix do NOT match — the parser only reads the
		// status code right after "musicbrainz: HTTP ", so an
		// upstream's HTML error page mentioning a status code
		// elsewhere can't false-positive.
		{"body containing HTTP 503 but no prefix", errors.New("page says: HTTP 503 was returned"), false},
		// Persistent 4xx WHOSE BODY CONTAINS "HTTP 503" must
		// NOT be classified as transient — coderabbit MAJOR
		// catch on PR #74 follow-up. Pre-fix substring match
		// would treat this as transient and the worker would
		// retry a guaranteed-fail track forever.
		{"persistent 400 with body mentioning HTTP 503",
			fmt.Errorf("musicbrainz: HTTP 400: server says HTTP 503 was returned earlier"),
			false},
		{"persistent 401 with body mentioning HTTP 429",
			fmt.Errorf("musicbrainz: HTTP 401: too many requests (HTTP 429) on prior call"),
			false},
		// And the structured parser correctly handles the
		// canonical transient cases even when the body is
		// noisy.
		{"transient 503 with messy body",
			fmt.Errorf("musicbrainz: HTTP 503: <html>random text mentioning HTTP 200</html>"),
			true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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
