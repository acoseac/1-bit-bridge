package dlna

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// TelemetryStore — ring buffer behavior
// -----------------------------------------------------------------------------

func Test_TelemetryStore_NewWithZeroCapacityCollapsesToDefault(t *testing.T) {
	for _, badCap := range []int{0, -1, -100} {
		s := NewTelemetryStore(badCap)
		if s.Capacity() != DefaultTelemetryCapacity {
			t.Errorf("capacity=%d should collapse to %d, got %d",
				badCap, DefaultTelemetryCapacity, s.Capacity())
		}
	}
}

func Test_TelemetryStore_EmptyStoreSnapshotReturnsNil(t *testing.T) {
	s := NewTelemetryStore(10)
	if got := s.Snapshot(); got != nil {
		t.Errorf("empty store Snapshot() should return nil, got %v", got)
	}
	if s.Len() != 0 {
		t.Errorf("empty store Len() should be 0, got %d", s.Len())
	}
}

func Test_TelemetryStore_RecordAndSnapshot(t *testing.T) {
	s := NewTelemetryStore(10)
	s.Record(TelemetryEntry{UserAgent: "first"})
	s.Record(TelemetryEntry{UserAgent: "second"})
	s.Record(TelemetryEntry{UserAgent: "third"})

	if s.Len() != 3 {
		t.Errorf("Len = %d, want 3", s.Len())
	}
	got := s.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot length = %d, want 3", len(got))
	}
	for i, want := range []string{"first", "second", "third"} {
		if got[i].UserAgent != want {
			t.Errorf("Snapshot[%d].UserAgent = %q, want %q", i, got[i].UserAgent, want)
		}
	}
}

func Test_TelemetryStore_RingBufferEvictsOldestWhenCapacityExceeded(t *testing.T) {
	s := NewTelemetryStore(3)
	for i, ua := range []string{"a", "b", "c", "d", "e"} {
		s.Record(TelemetryEntry{UserAgent: ua})
		_ = i
	}
	// After 5 entries into a capacity-3 ring: oldest "a"+"b" evicted,
	// snapshot should be ["c", "d", "e"] in order.
	got := s.Snapshot()
	if len(got) != 3 {
		t.Fatalf("expected len=3 after ring wrap, got %d", len(got))
	}
	for i, want := range []string{"c", "d", "e"} {
		if got[i].UserAgent != want {
			t.Errorf("after ring wrap, Snapshot[%d].UserAgent = %q, want %q (oldest 'a'+'b' should have been evicted)",
				i, got[i].UserAgent, want)
		}
	}
}

func Test_TelemetryStore_LenAfterRingWrap(t *testing.T) {
	s := NewTelemetryStore(5)
	for i := 0; i < 100; i++ {
		s.Record(TelemetryEntry{UserAgent: "u"})
	}
	if s.Len() != 5 {
		t.Errorf("after 100 records into capacity-5 ring, Len = %d, want 5", s.Len())
	}
}

func Test_TelemetryStore_SnapshotReturnsDefensiveCopy(t *testing.T) {
	s := NewTelemetryStore(10)
	s.Record(TelemetryEntry{UserAgent: "original"})
	got := s.Snapshot()
	got[0].UserAgent = "mutated"
	again := s.Snapshot()
	if again[0].UserAgent != "original" {
		t.Errorf("Snapshot must return defensive copy; got internal mutation: %v", again[0].UserAgent)
	}
}

// Test_TelemetryStore_ConcurrentRecordAndSnapshot is a race detector
// canary — run with `go test -race`. Ensures no data races between
// concurrent Record() + Snapshot() callers.
func Test_TelemetryStore_ConcurrentRecordAndSnapshot(t *testing.T) {
	s := NewTelemetryStore(100)
	var wg sync.WaitGroup

	// 10 concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Record(TelemetryEntry{UserAgent: "x", StatusCode: 200})
			}
		}()
	}
	// 10 concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Snapshot()
				_ = s.Len()
			}
		}()
	}
	wg.Wait()
}

// -----------------------------------------------------------------------------
// TelemetryMiddleware
// -----------------------------------------------------------------------------

func Test_TelemetryMiddleware_RecordsEntryPerRequest(t *testing.T) {
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	})
	h := TelemetryMiddleware(store, inner)

	req := httptest.NewRequest(http.MethodGet, "/test/path", nil)
	req.Header.Set("User-Agent", "Test-UA")
	req.Header.Set("Range", "bytes=0-99")
	req.RemoteAddr = "192.168.0.5:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := store.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 entry recorded, got %d", len(got))
	}
	entry := got[0]
	if entry.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", entry.Method)
	}
	if entry.Path != "/test/path" {
		t.Errorf("Path = %q, want /test/path", entry.Path)
	}
	if entry.UserAgent != "Test-UA" {
		t.Errorf("UserAgent = %q, want Test-UA", entry.UserAgent)
	}
	if entry.RangeHeader != "bytes=0-99" {
		t.Errorf("RangeHeader = %q, want bytes=0-99", entry.RangeHeader)
	}
	if entry.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", entry.StatusCode)
	}
	if entry.BytesServed != int64(len("hello world")) {
		t.Errorf("BytesServed = %d, want %d", entry.BytesServed, len("hello world"))
	}
	if entry.RemoteAddr != "192.168.0.5:54321" {
		t.Errorf("RemoteAddr = %q, want 192.168.0.5:54321", entry.RemoteAddr)
	}
}

func Test_TelemetryMiddleware_NilStorePassesThrough(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	h := TelemetryMiddleware(nil, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Errorf("inner handler should still be called when store=nil")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 (passthrough should preserve inner status)", rec.Code)
	}
}

func Test_TelemetryMiddleware_CapturesNonDefaultStatusCode(t *testing.T) {
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := store.Snapshot()
	if len(got) != 1 || got[0].StatusCode != http.StatusNotFound {
		t.Errorf("expected one entry with status 404, got %v", got)
	}
}

func Test_TelemetryMiddleware_PanicBeforeHeaderRecords500(t *testing.T) {
	// A handler that panics before writing any header/body leaves the
	// recorder at its 200 default. The middleware must record 500 (the
	// request failed) — and exactly ONE entry (no double-record).
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom before header")
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	// The middleware re-panics to preserve net/http's recovery; catch it here.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("middleware swallowed the panic; it must re-panic to preserve net/http recovery")
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	got := store.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry on panic (no double-record), got %d", len(got))
	}
	if got[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500 (panic before any header)", got[0].StatusCode)
	}
}

func Test_TelemetryMiddleware_PanicAfterHeaderKeepsStatus(t *testing.T) {
	// If the handler already committed a status (here 206 + a body chunk)
	// and THEN panics, the recorded status must stay 206 — that's what the
	// client received on the wire. The !wroteHeader guard prevents a
	// retroactive 500 relabel.
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("partial"))
		panic("boom mid-stream")
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/partial-then-panic", nil)
	rec := httptest.NewRecorder()

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(rec, req)
	}()

	got := store.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d", len(got))
	}
	if got[0].StatusCode != http.StatusPartialContent {
		t.Errorf("StatusCode = %d, want 206 — a committed status must not be relabeled 500 by a later panic", got[0].StatusCode)
	}
	if got[0].BytesServed != int64(len("partial")) {
		t.Errorf("BytesServed = %d, want %d", got[0].BytesServed, len("partial"))
	}
}

func Test_TelemetryMiddleware_CapturesDuration(t *testing.T) {
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 20ms (not 2ms) so the assertion below keeps a wide margin against
		// coarse OS timer granularity (notably Windows) — a 2ms sleep could
		// measure just under 2ms and truncate to 1, flaking `>= 2`.
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/slow", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := store.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].DurationMS < 5 {
		t.Errorf("DurationMS = %d, expected >= 5 (handler slept 20ms)", got[0].DurationMS)
	}
}

func Test_TelemetryMiddleware_FlushForwardsToUnderlying(t *testing.T) {
	store := NewTelemetryStore(10)
	flushed := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !flushed {
		t.Errorf("inner handler should see telemetryWriter as Flusher (wrap is transparent)")
	}
}

// Test_TelemetryMiddleware_DefaultStatusCodeIs200 — if the inner
// handler writes bytes without explicitly calling WriteHeader, the
// implicit status is 200. Our wrapper must capture that correctly
// rather than defaulting to 0.
func Test_TelemetryMiddleware_DefaultStatusCodeIs200(t *testing.T) {
	store := NewTelemetryStore(10)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No WriteHeader call; just write bytes
		_, _ = w.Write([]byte("hi"))
	})
	h := TelemetryMiddleware(store, inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := store.Snapshot()
	if len(got) != 1 || got[0].StatusCode != http.StatusOK {
		t.Errorf("implicit 200 not captured: %+v", got)
	}
}
