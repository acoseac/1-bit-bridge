package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRetryViaAdminWaitsOutTheRateLimitAndSaysSo pins the operator-facing
// half of the 60s guard.
//
// A silent wait on a rate limit reads as a hang, and the operator kills
// the process before it lands — so the wait must ANNOUNCE itself. This
// asserts both halves: the line is printed, and the retry actually
// completes once the server stops refusing.
func TestRetryViaAdminWaitsOutTheRateLimitAndSaysSo(t *testing.T) {
	shortenRetryPoll(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			// csrfGuard rejects body-bearing mutations without it; a CLI
			// that forgot the header would 415 against a real bridge.
			t.Errorf("missing Content-Type: application/json")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"rate_limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resetTracks": 5435, "harvestResubmitted": true,
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	addr := strings.TrimPrefix(srv.URL, "http://")
	code := retryViaAdmin(context.Background(), addr, "", &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if calls.Load() != 2 {
		t.Errorf("server saw %d calls, want 2 (one refused, one accepted)", calls.Load())
	}
	out := stdout.String()
	if !strings.Contains(out, "waiting on the server's rate limit") {
		t.Errorf("the wait was silent — an operator reads that as a hang.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "5435") {
		t.Errorf("did not report the re-queued count.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "re-submitted to Atlas") {
		t.Errorf("did not report the harvest re-submit.\ngot:\n%s", out)
	}
}

// TestRetryViaAdminReportsServerErrors — a 5xx must not be mistaken for
// a rate limit and silently retried until the budget expires.
func TestRetryViaAdminReportsServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"reset-failed","message":"disk full"}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := retryViaAdmin(context.Background(), strings.TrimPrefix(srv.URL, "http://"), "", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if calls.Load() != 1 {
		t.Errorf("server saw %d calls, want exactly 1 — a 5xx must not be retried", calls.Load())
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Errorf("did not surface the server's reason.\ngot: %s", stderr.String())
	}
}

// TestRetryViaAdminScopedUsesTheFolderEndpoint — a --path retry must hit
// the folder-scoped route, which carries its own per-path rate guard.
func TestRetryViaAdminScopedUsesTheFolderEndpoint(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resetTracks":12}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := retryViaAdmin(context.Background(), strings.TrimPrefix(srv.URL, "http://"),
		"Artist/Album", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if gotPath != "/api/library/enrichment/retry" {
		t.Errorf("path = %q, want the folder-scoped route", gotPath)
	}
	if !strings.Contains(gotBody, `"Artist/Album"`) {
		t.Errorf("body = %q, want it to carry the scope", gotBody)
	}
}

// TestRetryViaAdminGivesUpAfterTheBudget — a permanently rate-limited
// server must not hang forever.
func TestRetryViaAdminGivesUpAfterTheBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// Cancel stands in for the budget expiring — asserting the real 75s
	// budget would make this a 75-second test.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := retryViaAdmin(ctx, strings.TrimPrefix(srv.URL, "http://"), "", &stdout, &stderr)
	if code == 0 {
		t.Fatal("a permanently rate-limited server must not report success")
	}
}

// shortenRetryPoll collapses the 5s inter-attempt wait so a test that
// exercises the rate-limit path costs milliseconds, not seconds.
func shortenRetryPoll(t *testing.T) {
	t.Helper()
	prev := enrichmentRetryPollInterval
	enrichmentRetryPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { enrichmentRetryPollInterval = prev })
}

// TestValidMissFacetName pins the CLI's facet vocabulary against the
// manifest constants — a rename there must not silently make the flag
// reject every value.
func TestValidMissFacetName(t *testing.T) {
	for _, ok := range []string{"artwork", "artist", "release"} {
		if !validMissFacetName(ok) {
			t.Errorf("%q should be a valid facet", ok)
		}
	}
	for _, bad := range []string{"", "cover", "album", "ARTIST", "releases"} {
		if validMissFacetName(bad) {
			t.Errorf("%q should not be a valid facet", bad)
		}
	}
}
