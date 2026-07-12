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
	// Origin check: csrfGuard lets every GET through unconditionally
	// (correct for the body-bearing-mutation threat model it was
	// designed for), but /api/events is a long-lived endpoint that
	// allocates per-connection tickers + snapshot work. A non-admin-
	// origin browser context on the same loopback (a random tab the
	// operator visited) could otherwise open and hold SSE connections
	// indefinitely. Reuse the same origin allowlist csrfGuard applies
	// to mutations: Origin-when-present must match AdminAddress; absent
	// Origin is allowed (curl, fetch from the admin UI itself in some
	// browsers). Qodo on PR #107.
	if origin := r.Header.Get("Origin"); origin != "" {
		if !s.originMatchesAdmin(origin) {
			http.Error(w, "admin refused: cross-origin request", http.StatusForbidden)
			return
		}
	}

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
		lastStats       []byte
		lastEndpoints   []byte
		lastPairing     []byte
		lastUpdates     []byte
		lastTailscale   []byte
		lastComposition []byte
		lastEnrichment  []byte
		lastUpscale     []byte
		lastAnalysis    []byte
	)

	// publish writes one named SSE frame and flushes. Returns the
	// underlying write error (if any) so the loop can break out
	// when the client has dropped — the ctx.Done() select will catch
	// it on the next tick anyway, but bailing immediately on a
	// short write avoids wasted snapshot work.
	//
	// SSE protocol detail: a `data:` line that contains a literal
	// newline corrupts the stream — every line of the data block must
	// be re-prefixed. `json.Marshal` (without indentation) emits
	// single-line JSON with all newlines escaped as `\n`, so today's
	// payloads never trigger this path; the per-line loop is cheap
	// defense in depth against a future caller using `MarshalIndent`
	// or embedding raw bytes (Gemini on PR #107).
	publish := func(event string, data []byte) error {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
		// bytes.Split on a non-newline payload returns a single-element
		// slice — fast path, no allocation surprise.
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// marshalAndPublish is the common diff-then-emit body shared by
	// every per-event publish closure. Logs marshal failures (Gemini
	// on PR #107 — silent skip masks regressions in response struct
	// shape), then short-circuits on cache equality, then publishes.
	marshalAndPublish := func(event string, snap any, last *[]byte) error {
		b, err := json.Marshal(snap)
		if err != nil {
			logger.Error("sse marshal", "event", event, "err", err)
			return nil
		}
		if bytes.Equal(b, *last) {
			return nil
		}
		*last = b
		return publish(event, b)
	}

	publishStats := func() error {
		return marshalAndPublish("stats", s.getStatsSSESnapshot(), &lastStats)
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
		return marshalAndPublish("endpoints", snap, &lastEndpoints)
	}

	publishPairing := func() error {
		return marshalAndPublish("pairing", s.getPairingSnapshot(), &lastPairing)
	}

	publishUpdates := func() error {
		return marshalAndPublish("updates", s.getUpdatesSnapshot(), &lastUpdates)
	}

	publishTailscale := func() error {
		return marshalAndPublish("tailscale", s.getTailscaleSnapshot(), &lastTailscale)
	}

	// composition (dashboard master-quality breakdown) is TTL-cached +
	// single-flighted in getCompositionSnapshot, so this stays cheap even
	// though the underlying scan is a full-table json_extract.
	publishComposition := func() error {
		return marshalAndPublish("composition", s.getCompositionSnapshot(), &lastComposition)
	}

	// enrichment (dashboard enrichment-progress card) rides the slow tick and is
	// TTL-cached + single-flighted in getEnrichmentSnapshot, so it stays cheap
	// even though the underlying matched/missing split is a full-table scan.
	publishEnrichment := func() error {
		return marshalAndPublish("enrichment", s.getEnrichmentSnapshot(), &lastEnrichment)
	}

	// upscale + analysis telemetry — these replace the Settings page's
	// former /api/upscale/stats and /api/analysis/stats 5 s pollers (one
	// HTTP request per open tab). Same snapshot the REST endpoints serve
	// (kept as thin wrappers + first-paint fallback). Use the connection
	// `ctx` so the snapshot's DB query (VariantStatsByKind / CountAnalysis)
	// is cancelled if the client disconnects mid-tick (Gemini on #436).
	publishUpscale := func() error {
		return marshalAndPublish("upscale", s.getUpscaleStatsSnapshot(ctx), &lastUpscale)
	}
	publishAnalysis := func() error {
		return marshalAndPublish("analysis", s.getAnalysisStatsSnapshot(ctx), &lastAnalysis)
	}

	// Initial snapshot — fires synchronously after headers so the
	// page hydrates on connect without waiting for the first tick.
	// Without this the dashboard would rely on its server-rendered
	// first paint AND have a 500 ms blank window for the panels
	// (pairing, endpoints) that don't have server-rendered first
	// paint at all. Bail on any write error: client already gone.
	for _, f := range []func() error{
		publishStats, publishPairing, publishEndpoints, publishUpdates, publishTailscale,
		publishComposition, publishEnrichment, publishUpscale, publishAnalysis,
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

	// wasScanning latches the previous fast-tick scanning state so we
	// fire one final stats publish after a scan completes (to flip the
	// "scanning" badge to "idle" and emit the post-scan TracksIndexed
	// without a 5 s lag). Outside that window, idle dashboards skip
	// the fast-tick stats snapshot entirely — the medium ticker still
	// catches non-scan changes (DeviceCount after a token mint, DBBytes
	// growth, etc.) within 5 s. Qodo on PR #107 — `CountTracks()` is
	// `SELECT COUNT(*)` and N open tabs would otherwise multiply DB
	// reads at 2 Hz on idle. Pairing stays unconditional on the fast
	// tick because pairing.List() is a cheap map iteration AND the
	// SecondsUntilExpiry countdown depends on it during a join flow.
	var wasScanning bool
	// wasUpscaleBusy latches the prior fast-tick pool-busy state so the
	// worker grid fires one final frame as the pool goes idle (workers →
	// idle sub-second), then the fast-tick upscale publish goes quiet.
	var wasUpscaleBusy bool

	for {
		select {
		case <-ctx.Done():
			return
		case <-fastTk.C:
			scanning := s.deps.Scanner.IsScanning()
			if scanning || wasScanning {
				if err := publishStats(); err != nil {
					return
				}
			}
			wasScanning = scanning
			if err := publishPairing(); err != nil {
				return
			}
			// Worker grid at per-second resolution WHILE the pool is busy
			// (diff-suppressed, so a steadily-running job doesn't spam).
			// Idle bridges skip this — the 5 s medium tick still carries
			// the full upscale snapshot. UpscaleBusy is a cheap atomic
			// probe (no DB); the publish itself runs the snapshot only
			// when the gate opens.
			busy := s.deps.UpscaleBusy != nil && s.deps.UpscaleBusy()
			if busy || wasUpscaleBusy {
				if err := publishUpscale(); err != nil {
					return
				}
			}
			wasUpscaleBusy = busy
		case <-medTk.C:
			if err := publishStats(); err != nil {
				return
			}
			if err := publishEndpoints(); err != nil {
				return
			}
			// Settings-page telemetry (replaces the former 5 s pollers).
			// Diff-suppressed: idle bridges emit nothing between heartbeats.
			if err := publishUpscale(); err != nil {
				return
			}
			if err := publishAnalysis(); err != nil {
				return
			}
		case <-slowTk.C:
			if err := publishUpdates(); err != nil {
				return
			}
			if err := publishTailscale(); err != nil {
				return
			}
			// Composition rides the slow tick — format only changes
			// after a scan; the snapshot is TTL-cached + diff-suppressed.
			if err := publishComposition(); err != nil {
				return
			}
			// Enrichment progress also rides the slow tick — it fills over
			// minutes-to-hours, and the snapshot is TTL-cached + diff-
			// suppressed (idle bridges emit nothing between heartbeats).
			if err := publishEnrichment(); err != nil {
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
