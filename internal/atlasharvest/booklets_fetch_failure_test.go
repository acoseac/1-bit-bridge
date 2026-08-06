package atlasharvest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBookletFetchFailureIsRecorded pins the wiring half of the head-of-line
// fix: a non-404 download failure must reach MarkBookletFetchFailed, which is
// what rotates the row in BookletsToFetch's checked_at ordering and burns one
// of its attempts.
//
// Pre-fix this branch only logged, so nothing on the failure path wrote
// checked_at or check_attempts — an available-but-unfetched row's checked_at
// is frozen (the check cycle never revisits available = 1 rows), so it stayed
// at the head of the queue and, with bookletFetchPerTick = 3, three such rows
// consumed the entire per-tick budget forever.
//
// A 500 is used deliberately: 404 has its own branch (flip unavailable + clear
// the tag) and 401/403 aborts the sweep for the credential wipe. Neither must
// record a fetch attempt.
//
// Negative control: delete the MarkBookletFetchFailed call from
// fetchOneBooklet's default branch and this test fails.
func TestBookletFetchFailureIsRecorded(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		w.WriteHeader(http.StatusInternalServerError) // transient upstream fault
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "e"}}
	c := bookletTestClient(t, srv.URL, sink, newFakeBookletFiles())

	c.tick(context.Background())

	if len(sink.failed) != 1 || sink.failed[0] != rel {
		t.Fatalf("fetch-failure marks = %v, want [%s] — without this the row keeps its "+
			"frozen checked_at and blocks the head of the fetch queue forever", sink.failed, rel)
	}
	// A transient failure is NOT a 404: the release stays available and keeps
	// its wire tag, so iOS still sees the booklet and a later attempt can land.
	if len(sink.unavail) != 0 {
		t.Errorf("unavailable marks = %v, want none on a 5xx", sink.unavail)
	}
	if _, tagged := sink.tags[rel]; tagged {
		t.Errorf("tag was rewritten on a 5xx; want untouched")
	}
	if len(sink.fetched) != 0 {
		t.Errorf("fetched marks = %v, want none", sink.fetched)
	}
	if tok := c.State.Snapshot().Token; tok == "" {
		t.Error("credential wiped by a 5xx — only 401/403 may do that")
	}
}

// TestBookletFetch404DoesNotRecordAFetchAttempt keeps the two failure branches
// distinct. A 404 flips the row unavailable, which hands it back to the CHECK
// rotation with a zeroed counter; recording a fetch attempt there as well would
// be meaningless bookkeeping on a row that has left the fetch queue.
func TestBookletFetch404DoesNotRecordAFetchAttempt(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "e"}}
	c := bookletTestClient(t, srv.URL, sink, newFakeBookletFiles())

	c.tick(context.Background())

	if len(sink.failed) != 0 {
		t.Errorf("fetch-failure marks = %v, want none — a 404 takes the 'flip unavailable' branch", sink.failed)
	}
	if len(sink.unavail) != 1 {
		t.Errorf("unavailable marks = %v, want the release flipped", sink.unavail)
	}
}

// TestBookletOversizedPDFRecordsAFetchFailure is the field case the queue fix
// exists for: the size guard refuses an over-cap PDF (never truncates it), and
// that refusal is permanent for that asset. Without a recorded attempt the
// release would re-download 64 MiB every tick, forever, ahead of every other
// pending booklet.
func TestBookletOversizedPDFRecordsAFetchFailure(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		chunk := make([]byte, 1<<20)
		for total := 0; total <= maxBookletBytes+1; total += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "e"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)

	c.tick(context.Background())

	if len(sink.failed) != 1 || sink.failed[0] != rel {
		t.Fatalf("fetch-failure marks = %v, want [%s] for an over-cap PDF", sink.failed, rel)
	}
	if len(sink.fetched) != 0 {
		t.Errorf("oversized PDF was marked fetched")
	}
	files.mu.Lock()
	_, present := files.files[rel]
	files.mu.Unlock()
	if present {
		t.Error("oversized PDF left a partial file, want removed — a truncated PDF is corrupt")
	}
}

// failingBookletServer answers the booklet fetch with a 500 and the check
// endpoint with an empty verdict list.
func failingBookletServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureLogs points the client's logger at a buffer at Debug level, so ANY
// emitted record is visible to the assertion.
func captureLogs(c *Client) *bytes.Buffer {
	var buf bytes.Buffer
	c.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// TestBookletFetchFailureOnShutdownIsQuietButStillStamps pins BOTH halves of
// the shutdown-noise fix, and the second half is the one that matters.
//
// A cancelled ctx makes the in-flight PDF fetch fail with context.Canceled and
// the progress stamp fail the same way, so an ungated pair of Warn logs would
// put two misleading lines per in-flight booklet in the journal on every clean
// stop. Both are gated on ctx.Err() == nil.
//
// But the gate covers the LOGS ONLY: MarkBookletFetchFailed is still CALLED,
// and the return value is unchanged. Gating the call instead would skip the
// progress record that keeps a failing row from pinning the head of the fetch
// queue — reintroducing the head-of-line block this PR exists to fix.
//
// Negative controls, all verified:
//   - drop either `if ctx.Err() == nil` → the "no log" assertion fails;
//   - move the gate around the MarkBookletFetchFailed CALL → the "stamp still
//     attempted" assertion fails.
func TestBookletFetchFailureOnShutdownIsQuietButStillStamps(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := failingBookletServer(t)

	sink := newFakeBookletSink()
	c := bookletTestClient(t, srv.URL, sink, newFakeBookletFiles())
	logs := captureLogs(c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the bridge is stopping mid-sweep

	landed, err := c.fetchOneBooklet(ctx, c.State.Snapshot(), rel, map[string]struct{}{})

	// Control flow is untouched by the gate.
	if landed || err != nil {
		t.Fatalf("fetchOneBooklet = (%v, %v), want (false, nil) — the ctx gate must "+
			"not change what this branch returns", landed, err)
	}
	// The stamp is still ATTEMPTED. This is the load-bearing assertion: skipping
	// it is what leaves the row pinned at the head of BookletsToFetch.
	if len(sink.failed) != 1 || sink.failed[0] != rel {
		t.Fatalf("fetch-failure stamp calls = %v, want [%s] — gate the LOG, never "+
			"the progress record", sink.failed, rel)
	}
	// And nothing was logged.
	if logs.Len() != 0 {
		t.Fatalf("logged during shutdown, want silence:\n%s", logs.String())
	}
}

// TestBookletFetchFailureUnderLiveCtxStillLogs is the companion: the gate is
// specific to a cancelled ctx, not a blanket removal of the warnings. A real
// upstream failure and a real stamp failure both still reach the journal.
func TestBookletFetchFailureUnderLiveCtxStillLogs(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := failingBookletServer(t)

	sink := newFakeBookletSink()
	sink.markFetchFailedErr = errors.New("disk full")
	c := bookletTestClient(t, srv.URL, sink, newFakeBookletFiles())
	logs := captureLogs(c)

	if _, err := c.fetchOneBooklet(context.Background(), c.State.Snapshot(), rel, map[string]struct{}{}); err != nil {
		t.Fatalf("fetchOneBooklet: %v", err)
	}

	out := logs.String()
	for _, want := range []string{"booklet_fetch_failed", "booklet_mark_fetch_failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q under a live ctx — the gate must suppress "+
				"shutdown noise only, never a genuine failure:\n%s", want, out)
		}
	}
}
