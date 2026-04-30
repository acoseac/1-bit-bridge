package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// --- GET /api/events ---
//
// Server-Sent Events stream that replaces the dashboard's previous
// per-page setInterval polls (`/api/stats` 3 s, `/api/endpoints` 30 s,
// `/api/pairing` 3 s, `/api/updates` 3 s, `/api/tailscale/status` 30 s).
// One open connection per browser tab; the server multiplexes named
// events at three cadences:
//
//   - 500 ms: stats, pairing (highest churn — scan progress, join requests)
//   - 5 s:   endpoints (interface enumeration is per-call expensive)
//   - 30 s:  updates, tailscale (cached snapshots from optional providers)
//
// Diff-then-publish: each named event keeps a per-connection cache of
// the last serialised JSON. A new snapshot only gets written when its
// JSON differs from the cached frame, so an idle bridge produces zero
// wire traffic between heartbeats. The SecondsUntilExpiry field on
// pendingPairingRow decrements every second while a request is
// pending; that's deliberate (see getPairingSnapshot doc) and means
// pairing frames will land approximately every second during a join
// flow — the server streams the countdown to the browser without a
// client-side ticker.
//
// Heartbeat: a 15 s `: heartbeat` SSE comment keeps proxies / NAT
// awake and gives the client a way to detect a half-open connection
// even when no real data has changed. Comment lines (leading `:`)
// are silently ignored by the EventSource parser.
//
// Shutdown: `r.Context()` derives from the http.Server's BaseContext,
// which Serve wires to the parent context passed by serveCmd. When the
// bridge shuts down, all in-flight SSE handlers see ctx.Done() within
// the existing 5 s graceful-shutdown window. No per-handler bgScans
// bookkeeping needed — the stdlib does the right thing once BaseContext
// is set.
//
// Concurrency: per-client tickers, no broker / fan-out. Admin is a
// single-user loopback console; even with several browser tabs the
// sample work is bounded. CountTracks is the only call that scales
// with library size and is page-cache cheap on a warm SQLite store.
// If measurement ever shows hot reads bottlenecking, swap in a single
// poller goroutine + per-client subscriber channels.
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusNotImplemented, "no-sse",
			"streaming not supported on this connection")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store, no-transform")
	h.Set("Connection", "keep-alive")
	// X-Accel-Buffering disables response buffering in nginx-class
	// reverse proxies. The admin listener binds loopback by default
	// (no proxy in the path), but this is cheap defense in depth for
	// any future "expose admin via Tailscale" deployment that puts
	// the listener behind nginx.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Per-event last-serialised cache. Compared byte-wise to the
	// next snapshot's JSON; the publish writes only on change. nil
	// (zero value) is "never sent" — the initial snapshot below
	// always emits regardless of cache state.
	var (
		lastStats     []byte
		lastEndpoints []byte
		lastPairing   []byte
		lastUpdates   []byte
		lastTailscale []byte
	)

	// publish writes one named SSE frame and flushes. Returns the
	// underlying write error (if any) so the loop can break out
	// when the client has dropped — the ctx.Done() select will catch
	// it on the next tick anyway, but bailing immediately on a
	// short write avoids wasted snapshot work.
	publish := func(event string, data []byte) error {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	publishStats := func() error {
		snap := s.getStatsSSESnapshot()
		b, err := json.Marshal(snap)
		if err != nil {
			return nil // skip rather than tear down the stream
		}
		if bytes.Equal(b, lastStats) {
			return nil
		}
		lastStats = b
		return publish("stats", b)
	}

	publishEndpoints := func() error {
		snap, eErr := s.getEndpointsSnapshot()
		if eErr != nil {
			// Mirror REST behaviour: log + skip. We can't surface a
			// 5xx mid-stream, so the operator sees the last good list
			// until the listen-address misconfig is fixed.
			logger.Error("sse endpoints snapshot", "code", eErr.code, "msg", eErr.msg)
			return nil
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return nil
		}
		if bytes.Equal(b, lastEndpoints) {
			return nil
		}
		lastEndpoints = b
		return publish("endpoints", b)
	}

	publishPairing := func() error {
		snap := s.getPairingSnapshot()
		b, err := json.Marshal(snap)
		if err != nil {
			return nil
		}
		if bytes.Equal(b, lastPairing) {
			return nil
		}
		lastPairing = b
		return publish("pairing", b)
	}

	publishUpdates := func() error {
		snap := s.getUpdatesSnapshot()
		b, err := json.Marshal(snap)
		if err != nil {
			return nil
		}
		if bytes.Equal(b, lastUpdates) {
			return nil
		}
		lastUpdates = b
		return publish("updates", b)
	}

	publishTailscale := func() error {
		snap := s.getTailscaleSnapshot()
		b, err := json.Marshal(snap)
		if err != nil {
			return nil
		}
		if bytes.Equal(b, lastTailscale) {
			return nil
		}
		lastTailscale = b
		return publish("tailscale", b)
	}

	// Initial snapshot — fires synchronously after headers so the
	// page hydrates on connect without waiting for the first tick.
	// Without this the dashboard would rely on its server-rendered
	// first paint AND have a 500 ms blank window for the panels
	// (pairing, endpoints) that don't have server-rendered first
	// paint at all. Bail on any write error: client already gone.
	for _, f := range []func() error{
		publishStats, publishPairing, publishEndpoints, publishUpdates, publishTailscale,
	} {
		if err := f(); err != nil {
			return
		}
	}

	fastTk := time.NewTicker(500 * time.Millisecond)
	defer fastTk.Stop()
	medTk := time.NewTicker(5 * time.Second)
	defer medTk.Stop()
	slowTk := time.NewTicker(30 * time.Second)
	defer slowTk.Stop()
	heartbeatTk := time.NewTicker(15 * time.Second)
	defer heartbeatTk.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fastTk.C:
			if err := publishStats(); err != nil {
				return
			}
			if err := publishPairing(); err != nil {
				return
			}
		case <-medTk.C:
			if err := publishEndpoints(); err != nil {
				return
			}
		case <-slowTk.C:
			if err := publishUpdates(); err != nil {
				return
			}
			if err := publishTailscale(); err != nil {
				return
			}
		case <-heartbeatTk.C:
			// SSE comment line — invisible to the EventSource parser
			// but keeps the connection visibly alive for proxies and
			// gives the client TCP a chance to detect a half-open
			// connection between real data frames.
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
