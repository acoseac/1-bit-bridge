package atlasharvest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDefaultHarvestHTTPClientHasNoOverallTimeout is the structural pin.
//
// http.Client.Timeout caps the ENTIRE exchange including the body read, so a
// value on the shared client applies equally to a small submit ack and to a
// 64 MiB booklet PDF / 32 MiB results page. The old 30s value therefore made
// any booklet or results page that could not transfer in 30s fail forever —
// the same shape as internal/updater PR #374, where a poll-sized timeout
// killed multi-MiB downloads permanently. Mirrors
// TestNewClient_DownloadClientHasNoOverallTimeout there.
//
// Negative control: restore `Timeout: 30 * time.Second` and this fails.
func TestDefaultHarvestHTTPClientHasNoOverallTimeout(t *testing.T) {
	if defaultHarvestHTTPClient.Timeout != 0 {
		t.Fatalf("defaultHarvestHTTPClient.Timeout = %v, want 0 — a whole-exchange "+
			"timeout caps the body read and cannot serve both a small ack and a "+
			"64 MiB PDF; each leg sets its own context deadline instead",
			defaultHarvestHTTPClient.Timeout)
	}
}

// trickleServer streams body in chunks separated by gap, flushing each one, so
// the transfer takes a wall-clock duration the client must be willing to wait
// out. It answers the booklet-fetch path only.
func trickleServer(t *testing.T, body []byte, chunks int, gap time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		size := (len(body) + chunks - 1) / chunks
		for off := 0; off < len(body); off += size {
			end := min(off+size, len(body))
			if _, err := w.Write(body[off:end]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(gap)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestBookletFetchDeadlineIsSizedForThePDFNotTheAck proves the booklet leg gets
// its own generous deadline rather than the ack-sized one — the whole point of
// splitting the single client timeout into two per-request tiers. The body here
// takes far longer to arrive than RequestTimeout allows, and must still land
// complete.
//
// Negative control: change fetchBookletPDF to wrap the ctx with
// c.requestTimeout() instead of c.bulkTimeout() and this fails.
func TestBookletFetchDeadlineIsSizedForThePDFNotTheAck(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("z"), 256<<10)...)
	srv := trickleServer(t, pdf, 6, 60*time.Millisecond) // ~360ms on the wire

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "e"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)
	// An ack-sized deadline that the transfer above comfortably exceeds.
	c.RequestTimeout = 50 * time.Millisecond
	c.BulkTimeout = 30 * time.Second

	if err := c.fetchBooklets(context.Background()); err != nil {
		t.Fatalf("fetchBooklets: %v", err)
	}

	files.mu.Lock()
	got := files.files[rel]
	files.mu.Unlock()
	if !bytes.Equal(got, pdf) {
		t.Fatalf("stored %d bytes, want %d — a slow large body must not be cut off by "+
			"the ack-sized deadline", len(got), len(pdf))
	}
	if len(sink.fetched) != 1 {
		t.Errorf("fetched marks = %v, want the download recorded", sink.fetched)
	}
	if len(sink.failed) != 0 {
		t.Errorf("fetch-failure marks = %v, want none on a successful slow transfer", sink.failed)
	}
}

// TestRequestTimeoutStillBoundsASmallJSONLeg is the other half: removing the
// client-wide timeout must not leave the background harvester able to hang
// forever on an unresponsive Atlas. The small JSON legs carry their own
// (shorter) deadline.
//
// Negative control: drop the context.WithTimeout from postJSON and this fails
// on the outer ceiling instead of returning.
func TestRequestTimeoutStillBoundsASmallJSONLeg(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang // never responds
	}))
	t.Cleanup(func() { close(hang); srv.Close() })

	c := bookletTestClient(t, srv.URL, newFakeBookletSink(), nil)
	c.RequestTimeout = 100 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		var out struct{}
		done <- c.postJSON(context.Background(), c.State.Snapshot(),
			"/v1/atlas/harvest/booklets/check", bookletsCheckRequest{}, &out)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("postJSON against a hanging server returned nil, want a deadline error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("postJSON did not return — the small JSON legs must carry their own " +
			"deadline now that the shared http.Client has none")
	}
}
