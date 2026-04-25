package updater

import (
	"log"
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
// The clamp uses CompareAndSwap rather than Store because a
// concurrent Begin() between our Add(-1) and the reset would
// otherwise be silently lost: Begin's Add(+1) takes us from -1 to
// 0 (or beyond), and a naive Store(0) clobbers that legitimate
// increment, leaving Inflight()==0 while a download is genuinely
// active. With CAS, the reset only fires if the counter is still
// at the underflow value; otherwise some other goroutine has
// already mutated it (Begin or another End) and we leave the
// counter alone. Caught in PR #42 review (Gemini).
func (t *Tracker) End() {
	if n := t.count.Add(-1); n < 0 {
		t.count.CompareAndSwap(n, 0)
		t.mu.Lock()
		first := !t.loggedUnderflow
		t.loggedUnderflow = true
		t.mu.Unlock()
		if first {
			log.Printf("updater: sessions tracker underflow — Begin/End mismatch (clamping to 0)")
		}
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
