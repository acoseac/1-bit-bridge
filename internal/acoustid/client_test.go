package acoustid

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testKey = "test-api-key-do-not-use"

func testFP() Fingerprint {
	return Fingerprint{Value: "AQABz0mUaEkSRZEG", Duration: 243.55, DistinctB64: 40}
}

// newTestClient wires a Client at an httptest server. handler sees the real
// request, so tests can assert on what actually went over the wire.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, testKey, "test-agent", srv.Client())
}

// TestLookupRequestsItsMetadata is the regression pin for a gap that would
// fail SILENTLY: AcoustID returns bare AcoustID IDs unless `meta` asks for
// more, so dropping a token here does not error — it just empties a field the
// gate grades, quietly turning a clause into a no-op.
//
// `sources` is the one that matters most: without it every result reads as
// zero sources and the reliability clause rejects everything, or (worse, if
// someone then "fixes" it by relaxing the clause) grades nothing at all.
func TestLookupRequestsItsMetadata(t *testing.T) {
	var gotQuery url.Values
	var gotRaw string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotRaw = r.URL.RawQuery
		fmt.Fprint(w, `{"status":"ok","results":[]}`)
	})
	_, _ = c.Lookup(context.Background(), testFP())

	meta := gotQuery.Get("meta")
	for _, want := range []string{"recordings", "releasegroups", "sources", "compress"} {
		if !strings.Contains(meta, want) {
			t.Errorf("meta=%q is missing %q — that silently disables a gate clause", meta, want)
		}
	}
	// The '+' separators must survive as literal '+', not be percent-escaped
	// into %2B: AcoustID uses '+' as the meta delimiter, and url.Values.Encode
	// would escape them.
	if !strings.Contains(gotRaw, "meta=recordings+releasegroups+sources+compress") {
		t.Errorf("raw query = %q; meta separators must stay literal '+'", gotRaw)
	}
	if got := gotQuery.Get("duration"); got != "244" {
		t.Errorf("duration = %q, want 244 (rounded whole seconds)", got)
	}
	if got := gotQuery.Get("client"); got != testKey {
		t.Errorf("client = %q", got)
	}
	if got := gotQuery.Get("fingerprint"); got != testFP().Value {
		t.Errorf("fingerprint = %q", got)
	}
}

func TestLookupDecodesResults(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","results":[{
			"id":"9ff43b6a-4f16-427c-93c2-92307ca505e0","score":0.97,
			"recordings":[{"id":"cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff","title":"T","duration":639,"sources":8,
				"artists":[{"id":"6d7b7cd4-254b-4c25-83f6-dd20f98ceacd","name":"M83"}],
				"releasegroups":[{"id":"ddaa2d4d-314e-3e7c-b1d0-f6d207f5aa2f","title":"RG","type":"Album"}]}]}]}`)
	})
	res, err := c.Lookup(context.Background(), testFP())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	r := res[0]
	if r.Score != 0.97 {
		t.Errorf("score = %v", r.Score)
	}
	// sources lives on the RECORDING, not the result — verified against a live
	// response. Reading it off the result yields 0 and rejects every track.
	if r.Recordings[0].Sources != 8 {
		t.Errorf("recording sources = %d, want 8", r.Recordings[0].Sources)
	}
	if len(r.Recordings) != 1 || r.Recordings[0].Duration != 639 {
		t.Fatalf("recordings decoded wrong: %+v", r.Recordings)
	}
	if len(r.Recordings[0].Artists) != 1 || r.Recordings[0].Artists[0].Name != "M83" {
		t.Errorf("artists decoded wrong: %+v", r.Recordings[0].Artists)
	}
	if len(r.Recordings[0].ReleaseGroups) != 1 || r.Recordings[0].ReleaseGroups[0].Title != "RG" {
		t.Errorf("release groups decoded wrong: %+v", r.Recordings[0].ReleaseGroups)
	}
}

func TestLookupNoResultsIsErrNoMatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","results":[]}`)
	})
	if _, err := c.Lookup(context.Background(), testFP()); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

// TestLookupErrorUnderHTTP200 — AcoustID signals failure in the body's
// `status` field and can do so with a 200, so the status is checked on every
// response rather than only on a non-2xx. Missing this would decode an error
// envelope as zero results and read it as "no match".
func TestLookupErrorUnderHTTP200(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"error","error":{"code":4,"message":"invalid API key"}}`)
	})
	_, err := c.Lookup(context.Background(), testFP())
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrNoMatch) {
		t.Fatal("an error envelope must not be mistaken for a clean no-match")
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("err = %v, want the upstream message", err)
	}
}

// TestLookupErrorEnvelopeClassifiesFromItsCode pins the fix for the one error
// shape the structured classifier could not see.
//
// The envelope path returned a plain fmt.Errorf, which discarded the code. Walk
// IsTransient with it: not a context error, not a RateLimitError, not an
// httpError, and statusFromMessage misses because the message reads "upstream
// error <code>" rather than "HTTP <status>" — so it came out persistent, every
// time. A transient upstream condition delivered that way (AcoustID answers
// with a 200 and an error body) therefore made the sweeper cache a permanent
// "no match" for the batch in flight: the PR #74 poisoning class.
//
// Driven through the real Lookup against a stub rather than by handing
// IsTransient a hand-built error, because the defect was in what Lookup
// PRODUCES; a test that constructed the typed error itself would have passed
// before the fix.
func TestLookupErrorEnvelopeClassifiesFromItsCode(t *testing.T) {
	cases := []struct {
		name          string
		code          int
		message       string
		wantTransient bool
	}{
		{"internal error", 5, "internal error", true},
		{"too many requests", 14, "too many requests", true},
		{"invalid api key", 4, "invalid API key", false},
		{"invalid fingerprint", 7, "invalid fingerprint", false},
		{"unknown code", 999, "something new", false},
		{"no error object at all", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"status":"error","error":{"code":` + strconv.Itoa(tc.code) +
				`,"message":"` + tc.message + `"}}`
			if tc.code == 0 {
				body = `{"status":"error"}` // envelope with no error object
			}
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, body)
			})

			_, err := c.Lookup(context.Background(), testFP())
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrNoMatch) {
				t.Fatal("an error envelope must not be mistaken for a clean no-match")
			}
			if got := IsTransient(err); got != tc.wantTransient {
				t.Errorf("IsTransient(%v) = %v, want %v — the sweeper caches a permanent "+
					"miss for anything it reads as persistent", err, got, tc.wantTransient)
			}
			// The operator-facing half must survive the retyping.
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("err = %v, want the upstream message %q", err, tc.message)
			}
		})
	}
}

func TestLookupRateLimitCarriesRetryAfter(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"status":"error","error":{"code":14,"message":"rate limit exceeded"}}`)
	})
	_, err := c.Lookup(context.Background(), testFP())

	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err = %v, want a RateLimitError so the sweeper can pause its whole pool", err)
	}
	if rle.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rle.RetryAfter)
	}
	if !IsTransient(err) {
		t.Error("a rate limit must classify transient")
	}
}

// TestLookupNeverLeaksTheAPIKey — the request URL carries `client=<key>` and a
// multi-kilobyte fingerprint. net/url's own error type stringifies with the
// full URL, so any error path that wraps it naively leaks the key into logs.
func TestLookupNeverLeaksTheAPIKey(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		// A server that is closed immediately, so Do() fails at connect.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		c := NewClient(srv.URL, testKey, "test-agent", srv.Client())
		srv.Close()

		_, err := c.Lookup(context.Background(), testFP())
		if err == nil {
			t.Fatal("expected a transport error")
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("error leaked the API key: %v", err)
		}
		if strings.Contains(err.Error(), testFP().Value) {
			t.Fatalf("error leaked the fingerprint: %v", err)
		}
	})

	t.Run("upstream error body", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream exploded")
		})
		_, err := c.Lookup(context.Background(), testFP())
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("error leaked the API key: %v", err)
		}
	})
}

// TestLookupRedirectGuard — this was the only outbound client in the repo
// following redirects to arbitrary hosts (enrich/deezer, updater/github,
// upnp/discovery and dlna/discovery all pin theirs). Defence in depth rather
// than a live hole — the production base URL is a constant, no Authorization
// header travels, and a redirect does not carry the query string that holds the
// key — but "the base is a constant" stops being true the first time someone
// points it at a proxy.
func TestLookupRedirectGuard(t *testing.T) {
	// NOTE both servers are on 127.0.0.1 and differ only by PORT, which is the
	// realistic shape of the thing being guarded against — a redirect naming
	// another service on the same host — and the reason the comparison is on
	// the whole origin rather than the hostname alone. A hostname-only guard
	// (the shape enrich/deezer uses, correctly, for its named CDN hosts) passes
	// this redirect straight through.
	t.Run("refuses a hop to another origin", func(t *testing.T) {
		var elsewhere atomic.Int32
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			elsewhere.Add(1)
			fmt.Fprint(w, `{"status":"ok","results":[]}`)
		}))
		defer sink.Close()

		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, sink.URL+"/v2/lookup", http.StatusFound)
		})

		_, err := c.Lookup(context.Background(), testFP())
		if err == nil {
			t.Fatal("expected the redirect to be refused")
		}
		if n := elsewhere.Load(); n != 0 {
			t.Fatalf("followed the redirect off-host %d time(s)", n)
		}
		// The refusal travels back through *url.Error, which stringifies with
		// the full URL — the same leak TestLookupNeverLeaksTheAPIKey guards.
		if strings.Contains(err.Error(), testKey) {
			t.Fatalf("the refusal leaked the API key: %v", err)
		}
	})

	t.Run("still follows a hop within the base host", func(t *testing.T) {
		// Host-scoped, not a blanket ban: a same-host redirect is ordinary
		// server behaviour and must keep working.
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/moved") {
				http.Redirect(w, r, r.URL.Path+"/moved?"+r.URL.RawQuery, http.StatusFound)
				return
			}
			fmt.Fprint(w, `{"status":"ok","results":[{"id":"x","score":0.9}]}`)
		})
		res, err := c.Lookup(context.Background(), testFP())
		if err != nil {
			t.Fatalf("Lookup across a same-host redirect: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d results, want 1", len(res))
		}
	})

	t.Run("does not mutate the caller's http.Client", func(t *testing.T) {
		// *http.Client values are routinely shared across services; installing
		// the guard on the caller's would apply this package's host restriction
		// to every redirect in the process (the trap enrich.NewDeezerClient
		// copies to avoid).
		shared := &http.Client{}
		_ = NewClient("https://api.acoustid.org/v2", testKey, "ua", shared)
		if shared.CheckRedirect != nil {
			t.Error("NewClient installed its redirect guard on the caller's client")
		}
	})
}

func TestLookupRefusesWithoutAKey(t *testing.T) {
	c := NewClient("", "", "test-agent", http.DefaultClient)
	if _, err := c.Lookup(context.Background(), testFP()); err == nil {
		t.Fatal("expected an error when no API key is configured")
	}
}

// TestIsTransientPinsClassification is the load-bearing table. A persistent
// error misclassified as transient loops the sweeper forever; a transient one
// misclassified as persistent poisons a track that would have succeeded on
// retry (the PR #74 class).
func TestIsTransientPinsClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, true},
		{"429", &httpError{StatusCode: 429, Body: "rate limit"}, true},
		{"500", &httpError{StatusCode: 500, Body: "boom"}, true},
		{"503", &httpError{StatusCode: 503, Body: "unavailable"}, true},
		{"400", &httpError{StatusCode: 400, Body: "invalid fingerprint"}, false},
		{"404", &httpError{StatusCode: 404, Body: "not found"}, false},
		// The body-mention trap: a persistent 4xx whose body quotes a 5xx must
		// stay persistent. Substring-matching the message would flip it and
		// retry forever.
		{"400 whose body mentions HTTP 503", &httpError{StatusCode: 400, Body: "upstream said HTTP 503"}, false},
		{"429 wrapped", fmt.Errorf("lookup: %w", &httpError{StatusCode: 429}), true},
		{"400 wrapped", fmt.Errorf("lookup: %w", &httpError{StatusCode: 400}), false},
		{"rate limit error", &RateLimitError{RetryAfter: time.Second, err: &httpError{StatusCode: 429}}, true},
		{"econnrefused", &net.OpError{Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		{"enetunreach", &net.OpError{Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}, true},
		{"econnreset", &net.OpError{Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"dns servfail", &net.DNSError{Err: "server misbehaving"}, true},
		{"dns nxdomain", &net.DNSError{Err: "no such host", IsNotFound: true}, false},
		{"plain decode error", errors.New("acoustid: decoding response: unexpected EOF"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStatusFromMessageIsAnchored — the fallback path parses the status out of
// httpError's own stable prefix. It must never read a number that merely
// appears inside an upstream body.
// The name matters here: this pins PREFIX QUALIFICATION, not positional
// anchoring. Its previous name ("...IsAnchored") plus a docblock
// claiming HasPrefix sent a review pass at replacing strings.Index with
// strings.HasPrefix — which would break the wrapped-error case below,
// the shape most real callers produce.
func TestStatusFromMessageRecognisesWrappedErrors(t *testing.T) {
	cases := []struct {
		msg      string
		wantCode int
		wantOK   bool
	}{
		{"acoustid: HTTP 503: unavailable", 503, true},
		// Wrapped by a caller — the prefix is no longer at offset 0.
		// This is the case HasPrefix would lose.
		{"lookup failed: acoustid: HTTP 429: slow down", 429, true},
		{"musicbrainz: HTTP 503: not ours", 0, false},
		{"upstream returned HTTP 503 in its body", 0, false},
		{"acoustid: HTTP notanumber: x", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		code, ok := statusFromMessage(tc.msg)
		if code != tc.wantCode || ok != tc.wantOK {
			t.Errorf("statusFromMessage(%q) = (%d,%v), want (%d,%v)",
				tc.msg, code, ok, tc.wantCode, tc.wantOK)
		}
	}
}

// TestParseRetryAfter mirrors the enrich twin's table. Every case below is a
// subtlety that was fixed once already there — keep the two in lockstep.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"0", 0},
		{"  45  ", 45 * time.Second},
		{"-5", 0},
		{"garbage", 0},
		// Capped in the SECONDS domain before multiplying: 2^33 seconds would
		// overflow time.Duration's int64 nanoseconds and bypass the cap.
		{"8589934592", MaxRetryAfter},
		// Beyond int64 → ErrRange → clamp, not fall through to 0.
		{"99999999999999999999", MaxRetryAfter},
		{"-99999999999999999999", 0},
		// Non-compliant fractional: honour the integer prefix rather than
		// dropping the backoff entirely.
		{"86400.5", MaxRetryAfter},
		{"30.7", 30 * time.Second},
		// HTTP-date forms contain no '.', so the truncation never mis-slices.
		{"Thu, 30 Jul 2026 12:01:00 GMT", time.Minute},
		{"Thu, 30 Jul 2026 11:59:00 GMT", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.header, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// TestMinIntervalForBase — pacing derives from the base URL so it cannot drift
// from the host it protects, and it FAILS SAFE: anything unparseable resolves
// to the public interval, so a malformed config can only make us more polite.
func TestMinIntervalForBase(t *testing.T) {
	cases := []struct {
		base string
		want time.Duration
	}{
		{"https://api.acoustid.org/v2", PublicMinInterval},
		{"https://acoustid.org/v2", PublicMinInterval},
		{"https://API.AcoustID.ORG/v2", PublicMinInterval},
		{"https://api.acoustid.org:443/v2", PublicMinInterval},
		// Dot-anchored: these are third parties, not the public service.
		{"https://notacoustid.org/v2", SelfHostedMinInterval},
		{"https://acoustid.org.example.com/v2", SelfHostedMinInterval},
		{"http://127.0.0.1:9999/v2", SelfHostedMinInterval},
		// Fail-safe: no host to reason about → assume public.
		{"", PublicMinInterval},
		{"not a url", PublicMinInterval},
		{"/relative/path", PublicMinInterval},
	}
	for _, tc := range cases {
		if got := minIntervalForBase(tc.base); got != tc.want {
			t.Errorf("minIntervalForBase(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
	// An empty base means the public service, so the constructed client must
	// pace at the public interval.
	if got := NewClient("", testKey, "ua", http.DefaultClient).MinInterval(); got != PublicMinInterval {
		t.Errorf("default client MinInterval = %v, want %v", got, PublicMinInterval)
	}
}

func TestNewClientTrimsBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"status":"ok","results":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v2/", testKey, "ua", srv.Client())
	_, _ = c.Lookup(context.Background(), testFP())
	if gotPath != "/v2/lookup" {
		t.Errorf("path = %q, want /v2/lookup — a trailing slash must not double up", gotPath)
	}
}

// TestRateLimitErrorIsNilSafe — RetryAfter is exported while the cause is not,
// so a caller outside this package (the sweeper, the control harness) can
// legitimately construct one with only the duration. That must not panic.
func TestRateLimitErrorIsNilSafe(t *testing.T) {
	e := &RateLimitError{RetryAfter: 30 * time.Second}
	if msg := e.Error(); msg == "" {
		t.Fatal("Error() must produce a message without a cause")
	}
	if !strings.Contains(e.Error(), "30s") {
		t.Errorf("Error() = %q, want the retry-after duration", e.Error())
	}
	if e.Unwrap() != nil {
		t.Error("Unwrap() should be nil when there is no cause")
	}
	// The classification must still hold: a bare rate-limit is transient.
	if !IsTransient(e) {
		t.Error("a rate limit must classify transient even without a cause")
	}
}
