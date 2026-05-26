package dlna

import (
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
		next.ServeHTTP(recw, r)
		store.Record(TelemetryEntry{
			Timestamp:             start,
			Method:                r.Method,
			Path:                  r.URL.Path,
			UserAgent:             r.Header.Get("User-Agent"),
			AcceptHeader:          r.Header.Get("Accept"),
			RangeHeader:           r.Header.Get("Range"),
			ContentFeaturesAccept: r.Header.Get("getContentFeatures.dlna.org"),
			StatusCode:            recw.statusCode,
			BytesServed:           recw.bytesSent,
			DurationMS:            time.Since(start).Milliseconds(),
			RemoteAddr:            r.RemoteAddr,
		})
	})
}

// telemetryWriter wraps http.ResponseWriter to capture status code +
// bytes-served for the TelemetryEntry. Implements http.Flusher so
// the wrap is transparent for streaming handlers (the DLNA file
// handler relies on Flush forwarding).
type telemetryWriter struct {
	http.ResponseWriter
	statusCode int
	bytesSent  int64
}

func (w *telemetryWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *telemetryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytesSent += int64(n)
	return n, err
}

func (w *telemetryWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
