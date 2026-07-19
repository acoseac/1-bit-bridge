package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

// httpLogger is the package-level slog logger for HTTP request telemetry.
// Component-tagged for grep-friendly filtering against the rest of the
// bridge's structured logs.
var httpLogger = logging.Component("http")

// contextKey is the unexported key type for request-scoped values stored
// in context.Context. Unexported so external packages can't collide with
// our keys (Go's context.Value linter rule).
type contextKey int

const (
	ctxKeyRequestID contextKey = iota
	ctxKeyLogger
)

// RequestIDFromContext returns the per-request ID established by the
// logging middleware, or "" if no middleware ran (e.g. tests that
// bypass the chain). Safe to call from any handler with the request's
// context.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// LoggerFromContext returns a slog logger pre-bound to the request_id
// (and method/path) for handlers that need to emit their own structured
// telemetry alongside the per-request line emitted by the logging
// middleware. Falls back to the unbound `http` component logger when no
// middleware has run, so callers don't have to nil-check the result.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return v
	}
	return httpLogger
}

// newRequestID produces a 16-hex character (8-byte / 64-bit) random
// identifier. Collisions are vanishingly unlikely at the bridge's
// realistic request volumes; using a full UUID would add a dependency
// and the extra entropy buys nothing operationally.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand is documented as not failing on platforms the
		// bridge supports; if it does, fall back to a time-based ID
		// rather than panic — log correlation degrades gracefully.
		return fmt.Sprintf("t%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// downloadThroughputMinBytes is the floor below which a /v1/download or
// /v1/read response is NOT recorded into the download-throughput
// telemetry. Tiny range probes (the hybrid-precache waiter, a seek that
// reads a few KB) would otherwise dominate the distribution with
// meaningless ratios. 2 MiB is comfortably above any metadata-shaped
// range read but well below a real track transfer.
const downloadThroughputMinBytes int64 = 2 * 1024 * 1024

// transportProto returns a short, bounded, CANONICAL protocol label for
// telemetry: "h3" / "h2" / "http/1.1". It prefers the negotiated ALPN
// (quic-go sets "h3"; the stdlib sets "h2"), tolerating any "h3-NN" draft
// suffix, and otherwise falls back to the request's major version. BOTH
// paths normalize to the same label set so a series never splits across
// "h2" (from ALPN) and "HTTP/2.0" (from r.Proto). The bridge is HTTPS-only,
// so the fallback is effectively tests / impossible plaintext — but keeping
// the labels canonical there too keeps logs + Prometheus clean. Bounded
// cardinality makes it safe as a Prometheus label.
func transportProto(r *http.Request) string {
	if r.TLS != nil {
		switch alpn := strings.ToLower(r.TLS.NegotiatedProtocol); {
		case strings.HasPrefix(alpn, "h3"):
			return "h3"
		case alpn == "h2":
			return "h2"
		}
	}
	switch r.ProtoMajor {
	case 3:
		return "h3"
	case 2:
		return "h2"
	default:
		return "http/1.1"
	}
}

// statusWriter wraps an http.ResponseWriter to capture the response
// status code and byte count for the logging middleware. Implements
// http.Flusher (SSE / chunked streaming need it) and Unwrap (Go 1.20+
// `http.ResponseController` reaches through it — and ResponseController
// is the supported path for callers that need Hijack / Deadline / etc.
// behind a wrapped writer). The legacy direct-assertion path
// `w.(http.Hijacker)` will NOT succeed once the request has been
// wrapped here — handlers that need Hijack must use ResponseController.
// The bridge has no Hijack callers today (only SSE), so this is a
// future-proofing note rather than a regression risk.
//
// bytes is int64 — manifest responses for 50k-track libraries already
// exceed 100 MB, and future streaming paths threading through this
// middleware would overflow a 32-bit int on Pi-class 32-bit hosts.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wroteHeader {
		// Defensive: stdlib already logs a "superfluous response.WriteHeader"
		// warning; don't double-record. Keep the original status as the
		// authoritative one.
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush delegates to the wrapped writer if it supports http.Flusher.
// Required for /v1/events (SSE) and /v1/manifest (chunked streaming).
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// FlushError delegates the flush to the wrapped writer and surfaces its error
// (e.g. a dead SSE client). Without it, http.NewResponseController(sw).Flush()
// matches the void Flush() above at this level and always returns nil — making
// the SSE handlers' `if err := rc.Flush(); err != nil` disconnect check dead.
func (s *statusWriter) FlushError() error {
	return http.NewResponseController(s.ResponseWriter).Flush()
}

// Unwrap returns the wrapped ResponseWriter so the stdlib's
// http.ResponseController and downstream type assertions can reach the
// underlying writer (Go 1.20+).
func (s *statusWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// requestLogging wraps the mux with a per-request log line and a request_id
// propagated through context.Context. Emits one slog record per request
// on completion at level INFO (2xx/3xx), WARN (4xx), or ERROR (5xx).
//
// Only r.URL.Path is logged, never r.URL.RawQuery — query strings on
// the bridge frequently carry filesystem paths (?path=/Music/...) that
// would be PII in any aggregated log surface.
//
// X-Request-ID is echoed in the response header so users can include it
// in bug reports and operators can correlate against server logs.
//
// ORDERING INVARIANT: this middleware reads the matched route Pattern back
// from the request COPY it forwards (`rc`, below) — that is the request the
// downstream http.ServeMux stamps Pattern onto. Any middleware inserted
// between requestLogging and the ServeMux MUST forward that same
// *http.Request unchanged; a further `r.WithContext(...)` copy would make
// the mux stamp Pattern on the newer copy, silently reverting
// HTTPRequestsTotal to "_unmatched" and disabling the download-throughput
// telemetry. `recoverer` — the only middleware between here and the mux
// today — forwards the request as-is, so the chain is correct.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newRequestID()
		w.Header().Set("X-Request-ID", reqID)

		sw := &statusWriter{ResponseWriter: w}

		reqLogger := httpLogger.With(
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
		)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, reqID)
		ctx = context.WithValue(ctx, ctxKeyLogger, reqLogger)

		// r.WithContext returns a shallow COPY of the request; the
		// downstream http.ServeMux records the matched route Pattern on
		// THAT copy, not on the original r. Hold the copy so we can read
		// Pattern back after routing — reading r.Pattern here would always
		// be "" (every route mislabeled "_unmatched" in HTTPRequestsTotal,
		// and the download-throughput gate below would never match).
		rc := r.WithContext(ctx)
		start := time.Now()
		next.ServeHTTP(sw, rc)

		// If the handler returned without ever calling Write or
		// WriteHeader (rare — typically only for handlers that hijack
		// the connection or that crash before responding), record the
		// status as 0 rather than misleadingly stamping 200.
		status := sw.status
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		duration := time.Since(start)
		proto := transportProto(r)
		reqLogger.LogAttrs(ctx, level, "http",
			slog.Int("status", status),
			slog.Int64("duration_ms", duration.Milliseconds()),
			slog.Int64("bytes", sw.bytes),
			slog.String("proto", proto),
		)
		// Prometheus mirrors. Pattern (route template) over the raw
		// path so cardinality stays bounded — `/v1/list?path=...` is
		// the same line of code as `/v1/list?path=other`, and
		// counting each query string as a distinct path label would
		// blow the metric out. We approximate the template via
		// `r.Pattern`, which on Go 1.22+ exposes the registered
		// pattern; if empty (defensive — handler registered without
		// a method pattern, or 404 fallthroughs that ServeMux didn't
		// match), collapse to the constant `"_unmatched"` so the
		// label cardinality stays bounded by the route count + 1
		// rather than the request-path cardinality (which can be
		// arbitrary attacker-controlled garbage).
		labelPath := rc.Pattern
		if labelPath == "" {
			labelPath = "_unmatched"
		}
		metrics.HTTPRequestsTotal.WithLabelValues(
			labelPath,
			fmt.Sprintf("%d", status),
			proto,
		).Inc()
		metrics.HTTPRequestDurationHist.WithLabelValues(labelPath).Observe(duration.Seconds())

		// Download-throughput telemetry — the signal that answers "does
		// HTTP/3 actually beat HTTP/2 on our links?". Strictly gated to
		// the two file-delivery endpoints: the broad `streamingRoute`
		// kind also covers /v1/manifest and the long-lived SSE routes
		// (/v1/events, /v1/pairing/{id}/events), and an SSE channel held
		// open for hours with a handful of heartbeat bytes would poison
		// the distribution with a near-zero ratio. 206 is accepted
		// alongside 200 because /v1/read always serves a range and a
		// ranged /v1/download is partial-content too. The byte floor
		// drops tiny range probes; the duration guard keeps a +Inf
		// (zero-duration division) out of the histogram.
		//
		// NEVER log the file path here: it lives in the query string the
		// middleware deliberately omits as PII. request_id (already bound
		// on reqLogger) correlates this line with the `http` line above.
		if (labelPath == "GET /v1/download" || labelPath == "GET /v1/read") &&
			(status == http.StatusOK || status == http.StatusPartialContent) &&
			sw.bytes >= downloadThroughputMinBytes && duration > 0 {
			// Mbit/s is decimal megabits (10^6 bits/s) by network-telemetry
			// convention — NOT mebibits (2^20). Keep 1_000_000 so the value
			// matches the metric's "Mbit/s" help text and standard tooling.
			mbps := float64(sw.bytes) * 8 / (duration.Seconds() * 1_000_000)
			reqLogger.LogAttrs(ctx, slog.LevelInfo, "download_complete",
				slog.String("proto", proto),
				slog.Int64("bytes_sent", sw.bytes),
				slog.Int64("duration_ms", duration.Milliseconds()),
				slog.Float64("throughput_mbps", mbps),
				slog.Int("status", status),
			)
			metrics.HTTPDownloadThroughputMbps.WithLabelValues(proto).Observe(mbps)
		}
	})
}

// recoverer catches panics in downstream handlers, logs them via the
// per-request slog logger with the full stack, and emits a sanitized
// 500 response. Without this, an unhandled panic falls through to
// net/http's default recovery, which logs to the stdlib `log` package
// — bypassing our structured-log pipeline and losing the request_id.
//
// The 500 body is only emitted when the handler has not yet started
// writing (statusWriter.wroteHeader is false). After headers are
// committed, the only honest signal is the closed connection — the
// caller has already begun parsing a successful response and can't
// switch to a 500 mid-flight.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the stdlib's documented "abort
			// without logging" signal — for instance, a writer detecting
			// a closed client connection. Don't log it as a real panic.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			LoggerFromContext(r.Context()).Error("panic recovered",
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			if sw, ok := w.(*statusWriter); ok && !sw.wroteHeader {
				writeError(w, http.StatusInternalServerError, "internal",
					"the bridge encountered an internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
