package dlna

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Mock ResponseWriter for controlled behavior testing
// -----------------------------------------------------------------------------

// mockResponseWriter records every Write call so tests can verify the
// chunking pattern. Optionally surfaces an error on the Nth Write to
// exercise the wrapper's error-propagation path.
type mockResponseWriter struct {
	header     http.Header
	statusCode int
	writes     [][]byte // each entry = one Write call's payload (copied)
	flushCount int

	errAfterWrite int   // if > 0, returns errOnWrite from the Nth Write call (1-indexed)
	errOnWrite    error // injected error for the trigger Write
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{header: http.Header{}, statusCode: 200}
}

func (m *mockResponseWriter) Header() http.Header  { return m.header }
func (m *mockResponseWriter) WriteHeader(code int) { m.statusCode = code }
func (m *mockResponseWriter) Flush()               { m.flushCount++ }

func (m *mockResponseWriter) Write(p []byte) (int, error) {
	// Record a defensive copy (since the wrapper may reuse its buffer slice).
	cp := make([]byte, len(p))
	copy(cp, p)
	m.writes = append(m.writes, cp)
	if m.errAfterWrite > 0 && len(m.writes) == m.errAfterWrite && m.errOnWrite != nil {
		return 0, m.errOnWrite
	}
	return len(p), nil
}

// totalBytes returns the sum of bytes across all recorded Write calls.
func (m *mockResponseWriter) totalBytes() int {
	n := 0
	for _, w := range m.writes {
		n += len(w)
	}
	return n
}

// -----------------------------------------------------------------------------
// Core behavior — buffering, flushing, threshold
// -----------------------------------------------------------------------------

func Test_AdaptiveResponseWriter_BuffersBelowThreshold(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)
	// Write 50 bytes — below 100-byte threshold, should not flush.
	n, err := aw.Write(make([]byte, 50))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 50 {
		t.Errorf("Write returned n=%d, want 50", n)
	}
	if len(mock.writes) != 0 {
		t.Errorf("under-threshold write should NOT flush to underlying; got %d underlying writes", len(mock.writes))
	}
	if aw.BufferedBytes() != 50 {
		t.Errorf("BufferedBytes=%d, want 50", aw.BufferedBytes())
	}
}

func Test_AdaptiveResponseWriter_FlushesAtThreshold(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)
	// Two 60-byte writes → 120 total → triggers flush after the second.
	_, _ = aw.Write(make([]byte, 60))
	_, _ = aw.Write(make([]byte, 60))
	if len(mock.writes) != 1 {
		t.Fatalf("expected 1 flush to underlying, got %d", len(mock.writes))
	}
	if mock.totalBytes() != 120 {
		t.Errorf("expected 120 bytes flushed, got %d", mock.totalBytes())
	}
	if aw.BufferedBytes() != 0 {
		t.Errorf("buffer should be empty after threshold flush, got %d bytes", aw.BufferedBytes())
	}
}

func Test_AdaptiveResponseWriter_LargeWriteImmediatelyTriggersFlush(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)
	// Single write larger than chunk size — should flush in one underlying Write.
	_, _ = aw.Write(make([]byte, 5000))
	if len(mock.writes) != 1 {
		t.Errorf("single large write should produce 1 underlying Write, got %d", len(mock.writes))
	}
	if len(mock.writes[0]) != 5000 {
		t.Errorf("underlying write size = %d, want 5000", len(mock.writes[0]))
	}
}

func Test_AdaptiveResponseWriter_ExplicitFlushDrainsBuffer(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 1000)
	_, _ = aw.Write(make([]byte, 200)) // way below threshold
	if len(mock.writes) != 0 {
		t.Fatalf("expected NO underlying writes yet, got %d", len(mock.writes))
	}
	aw.Flush()
	if len(mock.writes) != 1 {
		t.Errorf("Flush should drain buffer to one underlying Write, got %d", len(mock.writes))
	}
	if len(mock.writes[0]) != 200 {
		t.Errorf("flushed payload size = %d, want 200", len(mock.writes[0]))
	}
	if aw.BufferedBytes() != 0 {
		t.Errorf("buffer should be empty after Flush, got %d", aw.BufferedBytes())
	}
}

func Test_AdaptiveResponseWriter_FlushForwardsToUnderlyingFlusher(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)
	if mock.flushCount != 0 {
		t.Fatalf("precondition: mock.flushCount should be 0, got %d", mock.flushCount)
	}
	aw.Flush()
	if mock.flushCount != 1 {
		t.Errorf("AdaptiveResponseWriter.Flush() must forward to underlying http.Flusher, got mock.flushCount=%d", mock.flushCount)
	}
}

func Test_AdaptiveResponseWriter_FlushOnEmptyBufferIsNoOp(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)
	aw.Flush()
	if len(mock.writes) != 0 {
		t.Errorf("Flush on empty buffer should NOT generate underlying writes, got %d", len(mock.writes))
	}
	if mock.flushCount != 1 {
		t.Errorf("Flush must still forward to underlying Flusher even on empty buffer, got flushCount=%d", mock.flushCount)
	}
}

// -----------------------------------------------------------------------------
// Defensive inputs
// -----------------------------------------------------------------------------

func Test_AdaptiveResponseWriter_NonPositiveChunkSizeCollapsesToDefault(t *testing.T) {
	mock := newMockResponseWriter()
	for _, badSize := range []int{0, -1, -100000} {
		aw := NewAdaptiveResponseWriter(mock, badSize)
		if aw.ChunkSize() != DefaultChunkSize {
			t.Errorf("chunkSize=%d should collapse to DefaultChunkSize=%d, got %d", badSize, DefaultChunkSize, aw.ChunkSize())
		}
	}
}

// -----------------------------------------------------------------------------
// Error propagation
// -----------------------------------------------------------------------------

func Test_AdaptiveResponseWriter_FlushErrorPropagatesToCaller(t *testing.T) {
	mock := newMockResponseWriter()
	mock.errAfterWrite = 1
	mock.errOnWrite = errors.New("network down")
	aw := NewAdaptiveResponseWriter(mock, 100)

	// Trigger a flush by writing past the threshold.
	n, err := aw.Write(make([]byte, 150))
	if err == nil {
		t.Fatalf("expected error from Write after underlying failure, got nil (n=%d)", n)
	}
	if n != 0 {
		t.Errorf("on flush error, expected n=0 (io.Copy semantics), got %d", n)
	}
}

func Test_AdaptiveResponseWriter_StickyErrorPreventsFurtherWrites(t *testing.T) {
	mock := newMockResponseWriter()
	mock.errAfterWrite = 1
	mock.errOnWrite = errors.New("network down")
	aw := NewAdaptiveResponseWriter(mock, 100)

	// First write fails the flush
	_, err1 := aw.Write(make([]byte, 200))
	if err1 == nil {
		t.Fatalf("expected first write to fail, got nil err")
	}

	// Subsequent writes should fast-path to the sticky error WITHOUT
	// hitting the underlying writer again (io.Copy convention).
	preWrites := len(mock.writes)
	_, err2 := aw.Write(make([]byte, 50))
	if err2 == nil {
		t.Errorf("expected sticky error to fail subsequent Write, got nil")
	}
	if len(mock.writes) != preWrites {
		t.Errorf("sticky-error Write should NOT call underlying.Write again; underlying writes went from %d to %d", preWrites, len(mock.writes))
	}
}

// -----------------------------------------------------------------------------
// Status code and headers — Range / 206 Partial Content preservation
// -----------------------------------------------------------------------------

// Test_AdaptiveResponseWriter_StatusCodeAndHeadersPassthrough pins the
// load-bearing invariant that the wrapper preserves the underlying
// writer's status code and header semantics. Range / 206 Partial
// Content responses set by `http.ServeContent` MUST land on the wire
// as-is — a wrapper that buffered or altered headers would break the
// DLNA renderer compatibility the wrapper exists to preserve.
func Test_AdaptiveResponseWriter_StatusCodeAndHeadersPassthrough(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 100)

	// Set headers + status code via the wrapper, just like
	// http.ServeContent does for Range responses.
	aw.Header().Set("Content-Type", "audio/x-dsf")
	aw.Header().Set("Content-Range", "bytes 0-99/1000")
	aw.WriteHeader(206)

	if mock.statusCode != 206 {
		t.Errorf("status code did not pass through: mock got %d, want 206", mock.statusCode)
	}
	if mock.header.Get("Content-Type") != "audio/x-dsf" {
		t.Errorf("Content-Type did not pass through: %q", mock.header.Get("Content-Type"))
	}
	if mock.header.Get("Content-Range") != "bytes 0-99/1000" {
		t.Errorf("Content-Range did not pass through: %q", mock.header.Get("Content-Range"))
	}
}

// -----------------------------------------------------------------------------
// End-to-end with real http.ServeContent — the production usage pattern
// -----------------------------------------------------------------------------

// Test_AdaptiveResponseWriter_ServeContentRangeIntegration is the
// load-bearing integration test that confirms the wrapper composes
// correctly with `http.ServeContent` for a Range request. Uses
// httptest.ResponseRecorder + a strings.Reader as the content source.
func Test_AdaptiveResponseWriter_ServeContentRangeIntegration(t *testing.T) {
	content := strings.Repeat("ABCDEFGH", 200) // 1600 bytes of well-known content
	rec := httptest.NewRecorder()
	aw := NewAdaptiveResponseWriter(rec, 256) // small chunk size to exercise multi-flush behavior

	// Build a Range request for bytes 100-299 (200 bytes).
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Range", "bytes=100-299")

	// Use http.ServeContent to serve the content — the wrapper sits
	// between ServeContent and the recorder.
	http.ServeContent(aw, req, "test.bin", parseRFC3339("2026-05-26T12:00:00Z"), strings.NewReader(content))

	// CRITICAL: defer-Flush pattern in production. Drain remaining bytes.
	aw.Flush()

	if rec.Code != 206 {
		t.Errorf("expected 206 Partial Content for Range request, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Range") == "" {
		t.Errorf("ServeContent should have set Content-Range header for Range request, got empty")
	}
	got := rec.Body.String()
	if got != content[100:300] {
		t.Errorf("ServeContent body via wrapper did not match expected slice.\n  got %d bytes, want %d bytes",
			len(got), 200)
	}
}

// Test_AdaptiveResponseWriter_ServeContentFullFileIntegration covers the
// non-Range path (full file delivery) end-to-end. Confirms the wrapper
// passes the entire content through unchanged AND that the defer-Flush
// pattern drains the trailing bytes correctly (the tail of a file
// smaller than the buffer would otherwise be silently dropped).
func Test_AdaptiveResponseWriter_ServeContentFullFileIntegration(t *testing.T) {
	content := strings.Repeat("X", 1000)
	rec := httptest.NewRecorder()
	aw := NewAdaptiveResponseWriter(rec, 512) // chunk-size smaller than content

	req := httptest.NewRequest("GET", "/", nil)
	http.ServeContent(aw, req, "test.bin", parseRFC3339("2026-05-26T12:00:00Z"), strings.NewReader(content))
	aw.Flush() // drain the trailing bytes after ServeContent's final 32KB-loop iteration

	if rec.Code != 200 {
		t.Errorf("expected 200 OK for full request, got %d", rec.Code)
	}
	got := rec.Body.String()
	if got != content {
		t.Errorf("full file via wrapper did not match.\n  got %d bytes, want %d bytes", len(got), len(content))
	}
}

// Test_AdaptiveResponseWriter_ConservesTotalBytesAcrossManySmallWrites
// is the structural invariant test: across N small Write calls totaling
// M bytes, the underlying writer receives M bytes total (modulo any
// remaining buffer, which Flush() drains).
func Test_AdaptiveResponseWriter_ConservesTotalBytesAcrossManySmallWrites(t *testing.T) {
	mock := newMockResponseWriter()
	aw := NewAdaptiveResponseWriter(mock, 1024)
	const writeSize = 100
	const numWrites = 50
	for i := 0; i < numWrites; i++ {
		n, err := aw.Write(make([]byte, writeSize))
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		if n != writeSize {
			t.Fatalf("write %d returned %d, want %d", i, n, writeSize)
		}
	}
	aw.Flush() // drain any remainder
	expected := writeSize * numWrites
	if mock.totalBytes() != expected {
		t.Errorf("byte conservation: total flushed %d, expected %d (writeSize=%d × numWrites=%d)",
			mock.totalBytes(), expected, writeSize, numWrites)
	}
}

// -----------------------------------------------------------------------------
// Helper
// -----------------------------------------------------------------------------

// parseRFC3339 is a one-line helper for the integration tests. Returns
// zero time on parse failure (tests use fixed valid timestamps).
func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
