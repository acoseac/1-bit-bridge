package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// frame is one event payload pulled from the SSE stream.
type frame struct {
	event string
	data  string
}

// readFrames pulls SSE frames off the body until either `want` frames
// have arrived (across any event types) or the deadline expires. Comment
// lines (`: heartbeat`) are skipped — they're not data events the
// EventSource parser surfaces.
func readFrames(t *testing.T, body io.Reader, want int, deadline time.Duration) []frame {
	t.Helper()
	out := make([]frame, 0, want)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(body)
		// SSE frames can be longer than the default 64 KB scanner buffer
		// in pathological cases; bump generously.
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var current frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				continue
			case strings.HasPrefix(line, "event: "):
				current.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				current.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if current.event != "" && current.data != "" {
					out = append(out, current)
					current = frame{}
					if len(out) >= want {
						return
					}
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(deadline):
	}
	return out
}

// TestEventsStreamInitialSnapshot drives the SSE handler over a real
// httptest.NewServer (httptest.NewRecorder buffers the body until the
// handler returns, which never happens for a streaming endpoint) and
// asserts the initial-snapshot publish lands all nine named events
// with valid JSON shapes.
func TestEventsStreamInitialSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type: got %q want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("Cache-Control: got %q want no-cache", cc)
	}

	frames := readFrames(t, resp.Body, 10, 3*time.Second)
	if len(frames) != 10 {
		t.Fatalf("frames: got %d want 10 (got: %v)", len(frames), frames)
	}
	seen := map[string]bool{}
	for _, f := range frames {
		seen[f.event] = true
		var anyJSON any
		if err := json.Unmarshal([]byte(f.data), &anyJSON); err != nil {
			t.Errorf("event %q invalid JSON: %v (raw: %q)", f.event, err, f.data)
		}
	}
	for _, want := range []string{"stats", "pairing", "endpoints", "updates", "tailscale", "composition", "sources", "enrichment", "upscale", "analysis"} {
		if !seen[want] {
			t.Errorf("missing initial-snapshot event %q", want)
		}
	}
}

// TestEventsStreamDiffSuppression asserts that after the initial
// snapshot, an idle bridge produces no further data frames — only
// the heartbeat comment (which readFrames skips) keeps the connection
// alive. With zero pending pairing requests the SecondsUntilExpiry
// frame-per-second cadence doesn't apply.
func TestEventsStreamDiffSuppression(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Drain initial snapshot (10 named events incl. composition/sources/enrichment/upscale/analysis).
	initial := readFrames(t, resp.Body, 10, 3*time.Second)
	if len(initial) != 10 {
		t.Fatalf("initial snapshot incomplete: got %d frames", len(initial))
	}

	// Read for 2 seconds — the 500 ms ticker fires ~4 times in this
	// window, but with no state change, diff suppression should keep
	// every publish a no-op. We expect exactly zero further frames.
	extra := readFrames(t, resp.Body, 1, 2*time.Second)
	if len(extra) > 0 {
		t.Errorf("expected no frames after initial snapshot on idle bridge, got %d: %v",
			len(extra), extra)
	}
}

// TestEventsStreamWakesOnStateChange asserts the diff loop publishes
// a fresh `stats` frame after a real change — minting a token bumps
// DeviceCount, which is part of statsResponse.
//
// Cadence note: the fast (500 ms) ticker only does the stats snapshot
// while a scan is in flight (Qodo on PR #107 — keep the SQLite
// COUNT(*) cost off idle dashboards). DeviceCount is an off-scan
// change, so the medium (5 s) ticker carries it. The test window is
// sized for the medium tick (~6 s) plus a small jitter buffer.
func TestEventsStreamWakesOnStateChange(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := readFrames(t, resp.Body, 10, 3*time.Second); len(got) != 10 {
		t.Fatalf("initial snapshot incomplete: %d frames", len(got))
	}

	// State change: mint a token off-handler. Direct Auth.Mint avoids
	// the HTTP roundtrip and keeps the test focused on the SSE diff
	// behaviour, not the mint endpoint.
	if _, _, err := srv.deps.Auth.Mint("test-device"); err != nil {
		t.Fatal(err)
	}

	// Wait up to ~6 s for the medium ticker to fire and publish.
	post := readFrames(t, resp.Body, 1, 6*time.Second)
	if len(post) == 0 {
		t.Fatalf("no frames after state change")
	}
	for _, f := range post {
		if f.event != "stats" {
			continue
		}
		var s statsResponse
		if err := json.Unmarshal([]byte(f.data), &s); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		if s.DeviceCount != 1 {
			t.Errorf("DeviceCount: got %d want 1", s.DeviceCount)
		}
		return
	}
	t.Fatalf("no stats frame in post-change window: %v", post)
}

// TestEventsStreamShutdown asserts that cancelling the request context
// (the same signal http.Server's BaseContext-derived contexts get on
// graceful shutdown) returns the handler within the 5 s grace window.
// This is a proxy for the production shutdown path — `Serve` wires
// BaseContext to its parent ctx, so canceling the parent cancels the
// request ctx via the same chain.
func TestEventsStreamShutdown(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := readFrames(t, resp.Body, 10, 3*time.Second); len(got) != 10 {
		t.Fatalf("initial snapshot incomplete: %d frames", len(got))
	}

	cancel()

	// After cancel, the handler's r.Context().Done() fires and it
	// returns; the connection closes server-side and io.ReadAll
	// finishes. Bound the wait so a regression doesn't hang CI.
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		done <- err
	}()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not exit within 2 s of context cancel")
	}
}

// noFlushRecorder shadows httptest.ResponseRecorder's Flush method by
// implementing http.ResponseWriter manually (no embedding) so the
// http.Flusher cast in apiEvents fails.
type noFlushRecorder struct {
	rec *httptest.ResponseRecorder
}

func (n *noFlushRecorder) Header() http.Header         { return n.rec.Header() }
func (n *noFlushRecorder) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n *noFlushRecorder) WriteHeader(c int)           { n.rec.WriteHeader(c) }

// TestEventsHandlerNoFlusher asserts the 501 response path when the
// underlying ResponseWriter doesn't implement http.Flusher. SSE
// requires Flush; without it the handler must refuse rather than
// silently buffer the entire stream into memory.
func TestEventsHandlerNoFlusher(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	w := &noFlushRecorder{rec: rec}
	req := httptest.NewRequest("GET", "/api/events", nil)
	srv.apiEvents(w, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status: got %d want 501", rec.Code)
	}
}

// TestEventsRejectsCrossOriginGET asserts the SSE handler refuses
// a GET that carries a non-matching Origin. csrfGuard lets all GETs
// through (correct for body-bearing-mutation defense), but the SSE
// endpoint is long-lived and would otherwise be openable from any
// random tab on the same loopback. Apply the same Origin allowlist
// csrfGuard uses for mutations. Qodo on PR #107.
func TestEventsRejectsCrossOriginGET(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/events", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Origin", "http://attacker.example")
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rw.Code)
	}
}

// TestStatsSSESnapshotZeroesUptime locks in the contract that
// getStatsSSESnapshot zeroes UptimeSec so the SSE diff key is stable
// across ticks where nothing meaningful changed. The REST snapshot
// (getStatsSnapshot) keeps UptimeSec for back-compat.
func TestStatsSSESnapshotZeroesUptime(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// Force a non-zero uptime by backdating StartedAt; StartedAt is
	// captured at New() time so we have to mutate via the deps struct.
	srv.deps.StartedAt = time.Now().Add(-30 * time.Second)

	full := srv.getStatsSnapshot()
	if full.UptimeSec == 0 {
		t.Fatalf("getStatsSnapshot returned UptimeSec=0 with backdated StartedAt — test setup wrong")
	}
	sse := srv.getStatsSSESnapshot()
	if sse.UptimeSec != 0 {
		t.Errorf("getStatsSSESnapshot UptimeSec: got %d want 0", sse.UptimeSec)
	}
	// Round-trip through JSON to confirm the wire shape (and not just
	// the in-memory struct) carries the zero. Encoding/json emits the
	// zero literally — no `omitempty` on the field.
	b, _ := json.Marshal(sse)
	if !strings.Contains(string(b), `"uptimeSec":0`) {
		t.Errorf("SSE JSON missing uptimeSec:0 — got %s", b)
	}
}
