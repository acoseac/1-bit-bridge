package atlasharvest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
