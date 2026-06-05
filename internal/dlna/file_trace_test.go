package dlna

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// slowWriter is a fake http.ResponseWriter whose Write blocks for `block` on a
// designated call index, simulating TCP back-pressure (renderer stopped reading).
type slowWriter struct {
	http.ResponseWriter
	flushed   bool
	callIdx   int
	blockAt   int // 1-based call index that should block; 0 = never
	block     time.Duration
	totalWrit int
}

func (s *slowWriter) Write(p []byte) (int, error) {
	s.callIdx++
	if s.blockAt > 0 && s.callIdx == s.blockAt {
		time.Sleep(s.block)
	}
	s.totalWrit += len(p)
	return len(p), nil
}

func (s *slowWriter) Flush() { s.flushed = true }

func newSlowWriter() *slowWriter {
	return &slowWriter{ResponseWriter: httptest.NewRecorder()}
}

func TestTraceResponseWriter_AccumulatesBytesAndWrites(t *testing.T) {
	tw := &traceResponseWriter{ResponseWriter: newSlowWriter()}
	for i := 0; i < 3; i++ {
		if _, err := tw.Write([]byte("hello")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	bytes, writes, _, _ := tw.snapshot()
	if bytes != 15 {
		t.Fatalf("bytes = %d, want 15", bytes)
	}
	if writes != 3 {
		t.Fatalf("writes = %d, want 3", writes)
	}
}

func TestTraceResponseWriter_RecordsMaxWriteBlock(t *testing.T) {
	sw := newSlowWriter()
	sw.blockAt = 2 // the 2nd write blocks
	sw.block = 20 * time.Millisecond
	tw := &traceResponseWriter{ResponseWriter: sw}

	// 3 writes of 4 bytes each; the 2nd one blocks ~20ms.
	for i := 0; i < 3; i++ {
		if _, err := tw.Write([]byte("data")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	bytes, writes, bytesAtMaxBlock, maxBlock := tw.snapshot()
	if writes != 3 || bytes != 12 {
		t.Fatalf("writes=%d bytes=%d, want 3/12", writes, bytes)
	}
	if maxBlock < 10*time.Millisecond {
		t.Fatalf("maxBlock = %v, want >= 10ms (the blocking write)", maxBlock)
	}
	// The block happened on the 2nd write → cumulative bytes at that point = 8.
	if bytesAtMaxBlock != 8 {
		t.Fatalf("bytesAtMaxBlock = %d, want 8 (bytes after the 2nd write)", bytesAtMaxBlock)
	}
}

func TestTraceResponseWriter_FlushForwardsToUnderlying(t *testing.T) {
	sw := newSlowWriter()
	tw := &traceResponseWriter{ResponseWriter: sw}
	tw.Flush()
	if !sw.flushed {
		t.Fatal("Flush did not forward to the underlying http.Flusher")
	}
}

func TestNewFileTrace_DisabledIsPassthrough(t *testing.T) {
	prev := fileTraceEnabled
	fileTraceEnabled = false
	defer func() { fileTraceEnabled = prev }()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/dlna/file/abc", nil)
	dst, finish := newFileTrace(w, r, "abc")
	if dst != http.ResponseWriter(w) {
		t.Fatal("disabled trace should return the original writer unchanged")
	}
	finish() // must be a no-op that doesn't panic
}

func TestNewFileTrace_EnabledWrapsWriter(t *testing.T) {
	prev := fileTraceEnabled
	fileTraceEnabled = true
	defer func() { fileTraceEnabled = prev }()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/dlna/file/abc", nil)
	r.Header.Set("Range", "bytes=0-")
	dst, finish := newFileTrace(w, r, "abc")
	if _, ok := dst.(*traceResponseWriter); !ok {
		t.Fatalf("enabled trace should return *traceResponseWriter, got %T", dst)
	}
	if _, err := dst.Write([]byte("xyz")); err != nil {
		t.Fatalf("Write through trace writer: %v", err)
	}
	finish() // closes the ctx-watch goroutine + emits the END line
}
