package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
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

// TestRetryViaAdminWaitsOutARateLimitWithAnUnreadableBody pins the
// ordering between the status check and the body-read error.
//
// postAdminJSON reports a truncated body as an error while still
// returning the status. Retrying a rate limit needs only the status — so
// checking err first makes a 429 whose body failed to read skip the wait
// and report failure. That is a regression the round-1 fix to
// postAdminJSON introduced, and this is the test that would have caught
// it.
func TestRetryViaAdminWaitsOutARateLimitWithAnUnreadableBody(t *testing.T) {
	shortenRetryPoll(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// Promise more bytes than we send, then hang up: the client's
			// ReadAll fails with an unexpected EOF while the status is 429.
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"rate_li`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resetTracks":42}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := retryViaAdmin(context.Background(), strings.TrimPrefix(srv.URL, "http://"), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — a 429 with an unreadable body must still be "+
			"waited out (stderr: %s)", code, stderr.String())
	}
	if calls.Load() != 2 {
		t.Errorf("server saw %d calls, want 2 — the rate limit was not retried", calls.Load())
	}
	if !strings.Contains(stdout.String(), "42") {
		t.Errorf("did not report the count after the retry succeeded:\n%s", stdout.String())
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

// TestProbeBridgeDistinguishesPublicModeFromDown is the regression test
// for a lie the first version of this command told.
//
// A boolean "is the admin API usable?" collapses two very different
// states onto one answer. On a PUBLIC-MODE bridge the admin listener is
// HTTPS behind an adminauth session, so a plain-HTTP loopback probe gets
// 400 and an HTTPS one gets 401 — and the command then reported "no
// bridge running" while the bridge was serving traffic, and (worse) took
// the offline path that writes to the store behind its back.
func TestProbeBridgeDistinguishesPublicModeFromDown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   bridgeLiveness
	}{
		{"loopback admin, healthy", http.StatusOK, bridgeAdminUsable},
		// What a public-mode bridge actually returns to a plain-HTTP probe.
		{"public mode, http against https", http.StatusBadRequest, bridgeUpAdminUnreachable},
		{"public mode, needs a session", http.StatusUnauthorized, bridgeUpAdminUnreachable},
		{"admin unhealthy", http.StatusInternalServerError, bridgeUpAdminUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			got := probeBridge(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
			if got != tc.want {
				t.Errorf("probeBridge = %v, want %v", got, tc.want)
			}
		})
	}
	// Nothing listening — the ONLY state that means "not running".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	if got := probeBridge(context.Background(), addr); got != bridgeDown {
		t.Errorf("probeBridge on a closed port = %v, want bridgeDown", got)
	}
}

// TestEnrichmentRetryRefusesBehindALiveBridge — the offline reset writes
// to the store from a second process, bypassing Store.mu, which only
// serialises writers WITHIN one process. When we can see a bridge running
// but cannot use its API, refusing is the only safe answer.
func TestEnrichmentRetryRefusesBehindALiveBridge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // public-mode admin
	}))
	defer srv.Close()

	_, cfgPath := writeProbeFixture(t, strings.TrimPrefix(srv.URL, "http://"))
	var stdout, stderr bytes.Buffer
	code := enrichmentRetryCmd(context.Background(), []string{"--config", cfgPath}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("retry succeeded behind a live bridge; it must refuse.\nstdout: %s", stdout.String())
	}
	msg := stderr.String()
	for _, want := range []string{"a bridge is running", "single-writer", "systemctl stop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

// TestCollectMissesProbesOnce — the probe is a real network round-trip
// with a 200ms budget, and an earlier version called it twice (once
// transitively inside missesViaAdmin, once for the Source line). Both
// callers need the same answer, so it happens once and is passed down.
func TestCollectMissesProbesOnce(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stats" {
			probes.Add(1)
			w.WriteHeader(http.StatusUnauthorized) // public-mode admin
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	loaded, _ := writeProbeFixture(t, strings.TrimPrefix(srv.URL, "http://"))
	rep, err := collectMisses(context.Background(), loaded, "")
	if err != nil {
		t.Fatalf("collectMisses: %v", err)
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("probed the admin port %d times, want exactly 1", got)
	}
	// And the fallback still tells the truth about why it read the store.
	if !strings.Contains(rep.Source, "bridge is running") {
		t.Errorf("Source = %q, want it to say the bridge is running", rep.Source)
	}
}

// writeProbeFixture writes a bridge.yaml whose admin address points at
// adminAddr, and returns both the loaded config and its path — the two
// liveness tests need one each. Shared so the fixture lives in one place.
func writeProbeFixture(t *testing.T, adminAddr string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	cfg := &config.Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   "127.0.0.1:17788",
		AdminAddress:    adminAddr,
		DataDir:         filepath.Join(dir, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "T",
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, cfgPath
}
