package updater

import (
	"sync"
	"sync/atomic"
)

// Tracker counts active file-serving requests so the install path can
// refuse to swap-and-restart while a download is in flight.
//
// Why this matters: the bridge serves DSF (DSD) streams to the iOS
// app over /v1/download. iOS pre-caches the full file before
// `engine.play()` so the render thread never blocks on I/O — but a
// mid-download restart still drops the TCP connection, the partial
// download is discarded, and the user sees an error mid-track. For
// PCM streams the cost is the same connection-drop class. For DSD
// the additional risk is Hugo 2 / XMOS DAC DoP-lock loss on the
// render thread that's about to consume the pre-cached file (~30 s
// recovery). See iOS CLAUDE.md "Things that have bitten before".
//
// The tracker is best-effort: it counts /v1/read + /v1/download
// requests entering serveFile, not in-flight HTTP TCP connections.
// A long-running download that the iOS side has already cancelled
// (TCP close) will still show as inflight until ServeContent's write
// loop notices and returns. That's fine — we'd rather over-report
// activity than silently restart through one.
//
// Cheap operations: Begin / End / Inflight are all atomic.Int64 ops,
// no mutex acquisition on the hot path. The mutex only fires on the
// first negative-count underflow log, which should never happen in
// practice.
type Tracker struct {
	count atomic.Int64

	mu              sync.Mutex
	loggedUnderflow bool
}

// NewTracker returns a fresh inflight counter.
func NewTracker() *Tracker {
	return &Tracker{}
}

// Begin marks the entry of a file-serving request. Always paired
// with End in a defer so a panic on the response path doesn't leak
// the counter.
func (t *Tracker) Begin() {
	t.count.Add(1)
}

// End marks the exit of a file-serving request. Clamps to zero on
// underflow (defensive — should never happen, but a leak the wrong
// direction here lets Install run during a download).
//
// Implemented as a load-then-CAS loop, NOT a bare Add(-1) + fix-up.
// A bare Add(-1) drives the counter transiently negative on a
// spurious End(), and a concurrent Begin() racing that window gets
// swallowed when the fix-up CAS fails (Begin's Add(+1) already moved
// the counter off the underflow value), leaving Inflight()==0 while a
// download is genuinely active — the exact gate-bypass this guards.
// The loop only decrements when it observes a positive count, so it
// never goes negative and a concurrent Begin() is never lost. Matched
// callers always observe ≥1 (their own Begin), so the underflow
// branch is reached only on a real Begin/End mismatch. PR #42 review
// (Gemini); refined to a load-then-CAS loop per the r2 review.
func (t *Tracker) End() {
	for {
		cur := t.count.Load()
		if cur <= 0 {
			// Spurious End() with no live Begin(). Do NOT decrement —
			// log once and leave the counter untouched.
			t.mu.Lock()
			first := !t.loggedUnderflow
			t.loggedUnderflow = true
			t.mu.Unlock()
			if first {
				logger.Warn("sessions tracker underflow — Begin/End mismatch (ignored)")
			}
			return
		}
		if t.count.CompareAndSwap(cur, cur-1) {
			return
		}
		// Lost a race with a concurrent Begin/End; reload and retry.
	}
}

// Inflight returns the current count of file-serving requests that
// have entered serveFile and not yet exited. Always non-negative.
func (t *Tracker) Inflight() int64 {
	if n := t.count.Load(); n > 0 {
		return n
	}
	return 0
}
