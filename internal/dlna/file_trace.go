package dlna

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// Phase 0 DLNA file-serve tracing — diagnostic instrumentation, OFF by default.
// -----------------------------------------------------------------------------
//
// Enabled by setting the env var `BRIDGE_DLNA_TRACE` to any non-empty value
// before `bridge serve`. When on, every `/dlna/file/...` request logs a START
// line (Range / UA / remote addr) and an END line (bytes served, duration, the
// longest single underlying socket-write block, and whether the renderer closed
// the connection). This exists to answer the Chord 2Go DSD-ring investigation's
// open questions WITHOUT guessing:
//
//   - Does the renderer issue a fresh `Range:` GET on UPnP Seek?  → a new START
//     line with a non-zero Range offset appears right after the user scrubs.
//   - On UPnP Pause, does the renderer CLOSE the socket or just STALL it?
//       * close  → the request Context is cancelled → END logs `closedByPeer=true`.
//       * stall  → the socket write blocks (TCP window fills as the renderer
//                  stops reading) → END logs a large `maxWriteBlockMs` with
//                  `closedByPeer=false`.
//   - Renderer network-buffer depth → `bytesAtMaxBlock` ≈ bytes delivered before
//     the first long write-block (i.e. roughly how much the 2Go had buffered when
//     playback paused).
//   - Metadata probes vs audio reads → the logged Range header distinguishes a
//     tail probe (e.g. `bytes=-128`) from a genuine audio-data range.
//
// The trace writer sits BELOW `AdaptiveResponseWriter` (it wraps the real
// `http.ResponseWriter`), so it observes the actual flush-to-socket calls — the
// ones that block under back-pressure — and never perturbs `http.ServeContent`'s
// Range/206 handling. It forwards `http.Flusher` so the adaptive writer's flush
// detection still works through it.

// fileTraceEnabled is resolved once at package init from the environment. It's a
// package var (not a const) so tests can flip it; production reads the env once.
var fileTraceEnabled = os.Getenv("BRIDGE_DLNA_TRACE") != ""

// traceResponseWriter wraps the underlying ResponseWriter to time each write to
// the socket. A long single-write duration means the kernel/Go send buffers
// filled and the write blocked waiting for the renderer to read — the
// back-pressure signal that distinguishes a STALLED pause from a closed socket.
type traceResponseWriter struct {
	http.ResponseWriter

	mu              sync.Mutex
	bytes           int64
	writeCount      int64
	maxWriteBlock   time.Duration
	bytesAtMaxBlock int64
}

// Write times the underlying write. The underlying `Write` blocks under TCP
// back-pressure, so its duration IS the stall measurement.
func (t *traceResponseWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := t.ResponseWriter.Write(p)
	blocked := time.Since(start)

	t.mu.Lock()
	t.bytes += int64(n)
	t.writeCount++
	if blocked > t.maxWriteBlock {
		t.maxWriteBlock = blocked
		t.bytesAtMaxBlock = t.bytes
	}
	t.mu.Unlock()
	return n, err
}

// Flush forwards to the underlying flusher so `AdaptiveResponseWriter.Flush`'s
// `http.Flusher` type-assertion still reaches the real socket flusher through
// this wrapper.
func (t *traceResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// snapshot returns the accumulated counters under lock.
func (t *traceResponseWriter) snapshot() (bytes, writes, bytesAtMaxBlock int64, maxBlock time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bytes, t.writeCount, t.bytesAtMaxBlock, t.maxWriteBlock
}

// newFileTrace sets up tracing for one /dlna/file/ request when enabled. It
// returns the ResponseWriter to hand to `AdaptiveResponseWriter` (the trace
// wrapper when on, the original `w` when off) and a `finish` to defer that emits
// the END line. When tracing is off both are zero-cost passthroughs.
func newFileTrace(w http.ResponseWriter, r *http.Request, trackID string) (http.ResponseWriter, func()) {
	if !fileTraceEnabled {
		return w, func() {}
	}

	start := time.Now()
	rangeHdr := r.Header.Get("Range")
	ua := r.Header.Get("User-Agent")
	remote := r.RemoteAddr

	packageLogger.Info("dlna file trace: request start",
		"trackID", trackID,
		"method", r.Method,
		"range", rangeHdr,
		"ua", ua,
		"remoteAddr", remote,
	)

	tw := &traceResponseWriter{ResponseWriter: w}

	finish := func() {
		bytes, writes, bytesAtMaxBlock, maxBlock := tw.snapshot()
		// finish() runs deferred on the handler goroutine, so the request
		// context's state here is authoritative for "did the peer go away":
		// net/http cancels r.Context() when the client closes the connection.
		// errors.Is(..., context.Canceled) — NOT a bare != nil — so a handler
		// deadline (context.DeadlineExceeded) is never mislabeled a peer close.
		// (net/http also cancels on graceful server shutdown, so this reads as
		// "peer disconnected OR server shutting down" — fine for a diagnostic
		// trace; just don't read it as "client abort" exclusively.) Replaces
		// the prior per-request goroutine + mutex + done channel, which only
		// ever set this one bool — the byte-counter snapshot has its own
		// traceResponseWriter.mu. (external review r3)
		closed := errors.Is(r.Context().Err(), context.Canceled)

		packageLogger.Info("dlna file trace: request end",
			"trackID", trackID,
			"range", rangeHdr,
			"bytes", bytes,
			"writes", writes,
			"durationMs", time.Since(start).Milliseconds(),
			"maxWriteBlockMs", maxBlock.Milliseconds(),
			"bytesAtMaxBlock", bytesAtMaxBlock,
			"closedByPeer", closed,
		)
	}

	return tw, finish
}
