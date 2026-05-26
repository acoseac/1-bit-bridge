package dlna

import "net/http"

// AdaptiveResponseWriter wraps an `http.ResponseWriter` to buffer writes
// up to a configurable chunk-size threshold before flushing to the
// underlying writer. The point of the wrapper is to let the bridge
// influence the byte-chunking pattern that downstream DLNA renderers
// see WITHOUT bypassing `http.ServeContent` (which owns Range request
// parsing + 206 Partial Content responses, both load-bearing for DLNA
// renderer compatibility).
//
// **Why not replace http.ServeContent:** ServeContent's internal
// read-write loop uses a hardcoded 32 KB buffer and handles
// multi-part byte-range requests automatically. Replacing it would
// forfeit Range handling — DLNA renderers (especially Chord 2go and
// the libavformat-based control points like mConnect) depend on Range
// for ID3 metadata extraction at file head + tail, plus for any
// future seek operation. The wrapper composes WITH ServeContent:
//
//	aw := dlna.NewAdaptiveResponseWriter(w, dlna.ChunkSizeFor(...))
//	defer aw.Flush()
//	http.ServeContent(aw, r, name, modtime, file)
//
// `defer aw.Flush()` is structural — it drains any buffered bytes
// remaining after ServeContent's final Write but before the handler
// returns + net/http closes the connection. Without the defer, the
// trailing bytes of a file smaller than the chunk threshold (or the
// tail portion of any file) would be silently dropped.
//
// **Range preservation invariant:** the wrapper passes Header() and
// WriteHeader(status) through to the underlying writer unchanged.
// Status code 206 (Partial Content) set by `http.ServeContent` for
// Range responses lands on the wire as 206; Content-Length and
// Content-Range headers ServeContent sets land as-is. Pinned by
// `Test_AdaptiveResponseWriter_StatusCodeAndHeadersPassthrough`.
type AdaptiveResponseWriter struct {
	http.ResponseWriter
	chunkSize int
	buffer    []byte
	err       error // sticky — first error from flushBuffer disables further writes
}

// NewAdaptiveResponseWriter wraps `w` to enforce the given chunk size on
// flushes. A non-positive chunkSize collapses to `DefaultChunkSize`
// (matches the defensive-input contract of `ChunkSizeFor`).
func NewAdaptiveResponseWriter(w http.ResponseWriter, chunkSize int) *AdaptiveResponseWriter {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &AdaptiveResponseWriter{
		ResponseWriter: w,
		chunkSize:      chunkSize,
		buffer:         make([]byte, 0, chunkSize),
	}
}

// Write appends p to the internal buffer and flushes to the underlying
// writer when the buffer reaches or exceeds the chunk-size threshold.
// Always reports `(len(p), nil)` on success (the wrapper accepted all
// bytes from p, even if they're still sitting in the buffer un-flushed).
// On a sticky error from a prior flush, returns `(0, err)` so `io.Copy`
// — the call shape `http.ServeContent` uses internally — aborts the
// transfer cleanly.
//
// **Don't change the return value to surface partial counts** — that
// would break io.Copy's standard expectation of "n < len(p) AND err
// != nil → stop". Either accept all of p (n == len(p)) or accept none
// and surface the error (n == 0).
func (w *AdaptiveResponseWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.buffer = append(w.buffer, p...)
	if len(w.buffer) >= w.chunkSize {
		if err := w.flushBuffer(); err != nil {
			// flushBuffer set w.err already; surface on this Write
			// rather than letting it land silently on the next call.
			return 0, err
		}
	}
	return len(p), nil
}

// Flush drains any buffered bytes to the underlying writer, then
// forwards to the underlying `http.Flusher` if implemented. Conforms
// to `http.Flusher` (no error return) so net/http's internal flush
// detection works through the wrapper.
//
// Used both for explicit mid-stream flush (the `http.Flusher`
// interface) AND for the `defer aw.Flush()` drain pattern at handler
// exit. Errors from the buffer drain are captured into the sticky
// `w.err` field — the next Write call will surface them. If no
// further Write follows (typical for handler exit), the error is
// silently swallowed; that's acceptable here because the connection
// is going down anyway and downstream consumers (renderers) handle
// transfer-failure transparently via TCP semantics.
func (w *AdaptiveResponseWriter) Flush() {
	// Sticky-error short-circuit: once flushBuffer has set w.err,
	// further drain attempts can only re-fail on the same underlying
	// writer and would obscure the original error. Per CodeRabbit
	// Major on PR #303 — the prior shape would retry the buffer
	// write on every Flush() call even after a sticky failure.
	if w.err != nil {
		return
	}
	_ = w.flushBuffer()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// BufferedBytes reports the number of bytes currently sitting in the
// internal buffer (not yet flushed). Test-affordance for verifying the
// chunking behavior precisely; production callers don't need it but
// it costs nothing to expose.
func (w *AdaptiveResponseWriter) BufferedBytes() int {
	return len(w.buffer)
}

// ChunkSize reports the configured per-write threshold. Test-affordance
// + diagnostic exposure (telemetry can log the effective chunk size
// chosen per-connection).
func (w *AdaptiveResponseWriter) ChunkSize() int {
	return w.chunkSize
}

// flushBuffer writes the internal buffer to the underlying writer and
// resets the buffer length. On error, sets the sticky `w.err` field so
// subsequent Write calls fast-path to the error.
func (w *AdaptiveResponseWriter) flushBuffer() error {
	// Sticky-error gate: once w.err is set, no more underlying-
	// writer attempts. Repeated drain on a failed writer can both
	// surface a different error on the retry path AND produce
	// duplicate writes on the rare case where the underlying writer
	// recovered between the two attempts (the latter is the more
	// dangerous case — bit-exact contract forbids any duplicate
	// byte). Per CodeRabbit Major on PR #303.
	if w.err != nil {
		return w.err
	}
	if len(w.buffer) == 0 {
		return nil
	}
	if _, err := w.ResponseWriter.Write(w.buffer); err != nil {
		w.err = err
		// Clear the buffer even on failure so a later (incorrect)
		// caller that bypasses the sticky-error gate can't replay
		// the same bytes. Defensive — the gate above is the primary
		// defence; this is belt-and-braces.
		w.buffer = w.buffer[:0]
		return err
	}
	w.buffer = w.buffer[:0]
	return nil
}
