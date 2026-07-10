package dlna

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// TelemetryEntry is one captured renderer-connection record. Lives in
// the `TelemetryStore` ring buffer until evicted by the next entry
// past capacity.
//
// Fields chosen to support the Phase 0 + production diagnostic use
// cases:
//   - UserAgent — identify which renderer / control point dialed us
//   - AcceptHeader, RangeHeader, ContentFeaturesAccept — capture the
//     control-protocol shape the client sent (informs vendor profile
//     refinement: do we see Range patterns we didn't expect? Are some
//     control points asking for contentFeatures in unusual ways?)
//   - StatusCode + BytesServed — outcome surface (4xx/5xx errors
//     trace back to which client + path; bytes-served confirms the
//     transfer completed vs. was cut short)
//   - DurationMS — pulls double-duty as "slow request" diagnostic
//     AND as a coarse-grained RTT estimate for the chunk-allocator
//     network multiplier (PR 1 task #11 wires this in)
type TelemetryEntry struct {
	Timestamp             time.Time
	Method                string
	Path                  string
	UserAgent             string
	AcceptHeader          string
	RangeHeader           string
	ContentFeaturesAccept string
	StatusCode            int
	BytesServed           int64
	DurationMS            int64
	// RemoteAddr is the renderer's IP+port as seen by us. Used by
	// telemetry consumers (admin UI, vendor-profile heuristics) to
	// distinguish multiple renderers from the same vendor.
	RemoteAddr string
}

// TelemetryStore is a bounded ring buffer of TelemetryEntry records.
// Thread-safe via a sync.Mutex; reads (Snapshot) take a brief lock
// to copy entries out. Writes (Record) take the lock for an O(1)
// append-and-rotate.
//
// **Bounded by design.** Capacity defaults to 1000 entries. At ~200
// bytes per entry that's ~200KB — trivial memory cost, vastly more
// than enough for diagnostic context (the admin UI surfaces "last N
// connections" rather than full history). Older entries are silently
// evicted when capacity rolls over; consumers that need persistence
// log to slog at Record time instead of relying on the ring.
type TelemetryStore struct {
	mu       sync.Mutex
	entries  []TelemetryEntry // ring buffer
	head     int              // next write position
	full     bool             // true once we've wrapped around
	capacity int
}

// DefaultTelemetryCapacity is the ring buffer size used when
// `NewTelemetryStore` is called with capacity <= 0. 1000 entries
// matches the plan's specified default.
const DefaultTelemetryCapacity = 1000

// NewTelemetryStore constructs a ring buffer with the given capacity.
// capacity <= 0 collapses to DefaultTelemetryCapacity.
func NewTelemetryStore(capacity int) *TelemetryStore {
	if capacity <= 0 {
		capacity = DefaultTelemetryCapacity
	}
	return &TelemetryStore{
		entries:  make([]TelemetryEntry, capacity),
		capacity: capacity,
	}
}

// Record appends an entry to the ring. O(1) under the mutex.
// Concurrent callers serialize; the lock is held briefly (just for
// the index update + slot write).
func (s *TelemetryStore) Record(entry TelemetryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[s.head] = entry
	s.head = (s.head + 1) % s.capacity
	if s.head == 0 {
		s.full = true
	}
}

// Snapshot returns a defensive copy of every entry currently in the
// ring, ordered oldest-to-newest. Returns nil if the ring is empty.
func (s *TelemetryStore) Snapshot() []TelemetryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.full && s.head == 0 {
		return nil
	}
	var out []TelemetryEntry
	if s.full {
		// Ring has wrapped; oldest is at head, newest at head-1
		out = make([]TelemetryEntry, s.capacity)
		copy(out, s.entries[s.head:])
		copy(out[s.capacity-s.head:], s.entries[:s.head])
	} else {
		// Ring not yet full; entries 0..head-1 are valid in order
		out = make([]TelemetryEntry, s.head)
		copy(out, s.entries[:s.head])
	}
	return out
}

// Len returns the current number of entries in the ring. Useful for
// admin-UI "showing N connections" labels + tests.
func (s *TelemetryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return s.capacity
	}
	return s.head
}

// Capacity returns the ring's configured capacity.
func (s *TelemetryStore) Capacity() int { return s.capacity }

// -----------------------------------------------------------------------------
// Middleware
// -----------------------------------------------------------------------------

// TelemetryMiddleware wraps an http.Handler to record one
// TelemetryEntry per request. Wraps the ResponseWriter to capture
// status code + bytes-served, then records after the inner handler
// returns.
//
// **Lightweight by design.** The wrap is one struct alloc + one
// closure invocation + one mutex acquire — sub-millisecond
// per-request overhead even at the request rate the bridge's DLNA
// listener handles in practice (peak ~10 req/s during active
// playback from a single 2go).
//
// If `store` is nil, the middleware passes through without recording.
// Lets the bridge be configured with telemetry disabled
// (`cfg.DLNA.TelemetryEnabled = false`) without touching the
// middleware chain.
func TelemetryMiddleware(store *TelemetryStore, next http.Handler) http.Handler {
	if store == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recw := &telemetryWriter{ResponseWriter: w, statusCode: http.StatusOK}
		// Record in a defer so a panicking handler still produces a telemetry
		// entry (the inline Record after ServeHTTP would be skipped on panic,
		// losing exactly the request that failed dramatically). Re-panic
		// afterwards to preserve net/http's per-request panic recovery.
		// DeepSeek review.
		defer func() {
			rec := recover()
			status := recw.statusCode
			// A panic before any header/body was committed leaves statusCode
			// at the http.StatusOK default, misreporting a request that
			// actually failed. Record 500 in that case (net/http aborts the
			// connection on an unrecovered panic). Guard on !wroteHeader:
			// once WriteHeader/Write/ReadFrom has committed a status, THAT is
			// what the client received — don't retroactively relabel it.
			if rec != nil && !recw.wroteHeader {
				status = http.StatusInternalServerError
			}
			store.Record(TelemetryEntry{
				Timestamp:             start,
				Method:                r.Method,
				Path:                  r.URL.Path,
				UserAgent:             r.Header.Get("User-Agent"),
				AcceptHeader:          r.Header.Get("Accept"),
				RangeHeader:           r.Header.Get("Range"),
				ContentFeaturesAccept: r.Header.Get("getContentFeatures.dlna.org"),
				StatusCode:            status,
				BytesServed:           recw.bytesSent,
				DurationMS:            time.Since(start).Milliseconds(),
				RemoteAddr:            r.RemoteAddr,
			})
			if rec != nil {
				panic(rec) // preserve net/http's per-request panic recovery
			}
		}()
		next.ServeHTTP(recw, r)
	})
}

// telemetryWriter wraps http.ResponseWriter to capture status code +
// bytes-served for the TelemetryEntry. Implements http.Flusher so
// the wrap is transparent for streaming handlers (the DLNA file
// handler relies on Flush forwarding).
type telemetryWriter struct {
	http.ResponseWriter
	statusCode  int
	bytesSent   int64
	wroteHeader bool
}

func (w *telemetryWriter) WriteHeader(code int) {
	// 1xx informational responses (e.g. 100 Continue) are NOT the final status —
	// net/http allows a later WriteHeader with the real status after one, so
	// don't latch on them or we'd mis-report the final status the client got.
	// Gemini MEDIUM on PR #368.
	if code >= 100 && code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	// Record only the FIRST final status. net/http honours the first WriteHeader
	// and logs "superfluous WriteHeader" for later ones (they don't change the
	// response), so capturing the last would mis-report the status the client
	// actually received. DeepSeek review.
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *telemetryWriter) Write(p []byte) (int, error) {
	// A Write with no prior WriteHeader implicitly commits 200 OK; mark the
	// header written (statusCode stays at the http.StatusOK default) so a later
	// superfluous WriteHeader can't overwrite the status the client actually
	// received. Gemini MEDIUM on PR #368.
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(p)
	w.bytesSent += int64(n)
	return n, err
}

func (w *telemetryWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom preserves the zero-copy `sendfile(2)` fast path Go's
// HTTP server uses when the underlying writer implements
// `io.ReaderFrom`. `http.ServeContent` (used by the DLNA file
// handler under AdaptiveResponseWriter — which buffers and so
// blocks sendfile today) AND any future direct-io.Copy handler
// will hit this path; the byte count is still accurately
// captured for telemetry. If the underlying ResponseWriter
// doesn't implement ReaderFrom (rare — net/http's default
// response writer does), we fall back to `io.Copy` which
// routes through our Write method and bumps bytesSent through
// the normal path. Per Gemini Medium on PR #303.
func (w *telemetryWriter) ReadFrom(r io.Reader) (int64, error) {
	// ReadFrom commits the response body, implicitly 200 OK if no WriteHeader
	// preceded it — mark the header written for the same reason as Write.
	// Gemini MEDIUM on PR #368.
	w.wroteHeader = true
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.bytesSent += n
		return n, err
	}
	// Fallback: hand-rolled copy through our Write method so
	// `bytesSent` is bumped correctly. Calling `io.Copy(w, r)`
	// would recurse (io.Copy detects our ReadFrom and re-enters
	// this method infinitely); calling `io.Copy(w.ResponseWriter, r)`
	// would skip our Write and the byte count would be lost.
	// 32 KiB matches Go's internal copyBuffer default — same
	// chunk granularity as the non-ReaderFrom Write path so the
	// downstream chunking layer (AdaptiveResponseWriter) sees the
	// same shape regardless of which path landed bytes.
	buf := make([]byte, 32*1024)
	var total int64
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nw < nr {
				return total, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return total, nil
		}
		if er != nil {
			return total, er
		}
	}
}
