package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// withTestSlog routes slog.Default to a buffer for the duration of the
// test so we can assert against the structured log records the middleware
// emits. Restores the previous default on cleanup.
//
// **Must NOT be used with t.Parallel()** — slog.SetDefault mutates a
// process-global; two parallel tests calling this would race on the
// default-logger swap and read each other's buffers. The tests below
// run sequentially today; if a future change adds t.Parallel() to any
// of them, this helper needs to grow a sync.Mutex (or move to per-test
// loggers via slog.New + slog.NewLogLogger to avoid the global).
func withTestSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestNewRequestID_FormatAndUniqueness(t *testing.T) {
	a, b := newRequestID(), newRequestID()
	if a == b {
		t.Fatalf("expected distinct ids, both = %q", a)
	}
	hexRE := regexp.MustCompile(`^[0-9a-f]{16}$`)
	fbRE := regexp.MustCompile(`^t[0-9a-f]{16}$`) // crypto/rand failure fallback
	for _, id := range []string{a, b} {
		if !hexRE.MatchString(id) && !fbRE.MatchString(id) {
			t.Errorf("id %q matches neither expected shape", id)
		}
	}
}

func TestLogging_EmitsLineWithRequestIDAndStatus(t *testing.T) {
	buf := withTestSlog(t)
	h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	id := rr.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("X-Request-ID response header was not set")
	}

	line := buf.String()
	if !strings.Contains(line, "msg=http") {
		t.Errorf("expected msg=http log line, got %q", line)
	}
	if !strings.Contains(line, "status=418") {
		t.Errorf("expected status=418 in log line, got %q", line)
	}
	if !strings.Contains(line, "request_id="+id) {
		t.Errorf("expected request_id=%s in log line, got %q", id, line)
	}
	if !strings.Contains(line, "bytes=2") {
		t.Errorf("expected bytes=2 in log line, got %q", line)
	}
}

func TestLogging_LevelMapping(t *testing.T) {
	cases := []struct {
		status   int
		wantLvl  string
		wantCode string
	}{
		{http.StatusOK, "INFO", "200"},
		{http.StatusNotFound, "WARN", "404"},
		{http.StatusInternalServerError, "ERROR", "500"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			buf := withTestSlog(t)
			h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			h.ServeHTTP(rr, req)

			line := buf.String()
			if !strings.Contains(line, "level="+tc.wantLvl) {
				t.Errorf("status %d: expected level=%s in log line, got %q", tc.status, tc.wantLvl, line)
			}
			if !strings.Contains(line, "status="+tc.wantCode) {
				t.Errorf("status %d: expected status=%s in log line, got %q", tc.status, tc.wantCode, line)
			}
		})
	}
}

func TestLogging_DoesNotLogQueryString(t *testing.T) {
	buf := withTestSlog(t)
	h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/list?path=/sensitive/path/Music", nil)
	h.ServeHTTP(rr, req)

	line := buf.String()
	if strings.Contains(line, "sensitive") {
		t.Fatalf("query string leaked into log line: %q", line)
	}
	if !strings.Contains(line, "path=/v1/list") {
		t.Errorf("expected bare path=/v1/list, got %q", line)
	}
}

func TestLoggerFromContext_PropagatesRequestID(t *testing.T) {
	buf := withTestSlog(t)
	var seenID string
	h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
		// Emit a handler-side log; assert it carries request_id too.
		LoggerFromContext(r.Context()).Info("handler-side")
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	h.ServeHTTP(rr, req)

	if seenID == "" {
		t.Fatal("handler did not observe a request_id in context")
	}
	if !strings.Contains(buf.String(), `msg=handler-side`) {
		t.Errorf("handler-side log not found in buffer: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "request_id="+seenID) {
		t.Errorf("handler-side log missing request_id=%s: %q", seenID, buf.String())
	}
}

func TestLoggerFromContext_FallsBackToComponentLogger(t *testing.T) {
	// No middleware run — caller threading a bare context.
	got := LoggerFromContext(context.Background())
	if got == nil {
		t.Fatal("LoggerFromContext returned nil for bare ctx")
	}
	// Smoke: the returned logger must not panic on emit.
	got.Info("smoke")
}

func TestStatusWriter_DefaultsTo200OnImplicitWrite(t *testing.T) {
	buf := withTestSlog(t)
	h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No WriteHeader call — write body directly.
		_, _ = w.Write([]byte("body"))
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	h.ServeHTTP(rr, req)

	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("expected implicit 200 captured, got %q", buf.String())
	}
}

func TestStatusWriter_PreservesFirstStatusOnDoubleWriteHeader(t *testing.T) {
	buf := withTestSlog(t)
	h := requestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.WriteHeader(http.StatusInternalServerError) // bug-shape: shouldn't happen but mustn't crash
		_, _ = w.Write([]byte("denied"))
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	h.ServeHTTP(rr, req)

	if !strings.Contains(buf.String(), "status=403") {
		t.Errorf("expected first status (403) preserved, got %q", buf.String())
	}
}

func TestRecoverer_EmitsSanitized500AndLogsPanic(t *testing.T) {
	buf := withTestSlog(t)
	chain := requestLogging(recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"error":"internal"`) {
		t.Errorf("response body should carry sanitized internal code, got %q", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "kaboom") {
		t.Errorf("panic string must NOT leak into response body: %q", rr.Body.String())
	}

	line := buf.String()
	if !strings.Contains(line, "msg=\"panic recovered\"") && !strings.Contains(line, "msg=panic recovered") {
		t.Errorf("panic was not logged, got %q", line)
	}
	if !strings.Contains(line, "kaboom") {
		t.Errorf("panic value should be in server log, got %q", line)
	}
	if !strings.Contains(line, "status=500") {
		t.Errorf("logging middleware should observe status=500 after recovery, got %q", line)
	}
}

func TestRecoverer_DoesNotEmit500AfterHeaderCommitted(t *testing.T) {
	buf := withTestSlog(t)
	chain := requestLogging(recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("mid-stream")
	})))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("first-write 200 must not be overwritten by recoverer, got %d", rr.Code)
	}
	// We still log it.
	if !strings.Contains(buf.String(), "mid-stream") {
		t.Errorf("panic should still be logged even after header commit: %q", buf.String())
	}
}

func TestRecoverer_PropagatesAbortHandler(t *testing.T) {
	// http.ErrAbortHandler is the stdlib's documented "abort without
	// logging" signal. We re-panic it so net/http can do its own thing.
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("expected ErrAbortHandler to propagate, got %v", r)
		}
	}()
	chain := recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	chain.ServeHTTP(rr, req)
}

func TestStatusWriter_DelegatesFlush(t *testing.T) {
	// Wrap a Flusher-implementing recorder; assert Flush gets called via our wrapper.
	flushed := false
	inner := flushNotifier{recorder: httptest.NewRecorder(), onFlush: func() { flushed = true }}
	sw := &statusWriter{ResponseWriter: inner}
	sw.Flush()
	if !flushed {
		t.Error("statusWriter.Flush did not delegate to underlying Flusher")
	}
}

// flushNotifier is a tiny test-only ResponseWriter that records when
// Flush is called. http.ResponseWriter requires Header/Write/WriteHeader,
// all delegated to the wrapped Recorder.
type flushNotifier struct {
	recorder *httptest.ResponseRecorder
	onFlush  func()
}

func (f flushNotifier) Header() http.Header         { return f.recorder.Header() }
func (f flushNotifier) Write(b []byte) (int, error) { return f.recorder.Write(b) }
func (f flushNotifier) WriteHeader(c int)           { f.recorder.WriteHeader(c) }
func (f flushNotifier) Flush()                      { f.onFlush() }
