// Package loggingtest parks a goroutine on its own log line: the way a test
// stops a worker between two statements with no hook in production code.
// Like net/http/httptest it is imported only by tests, so none of it reaches
// the binary.
//
// It exists so there is ONE definition of the technique. Two pools need it
// (internal/analyze and internal/transcode), and the part that makes it work
// is easy to lose in a copy: logging.Component resolves slog.Default at LOG
// time, so pointing the default at a handler reaches every package-level
// component logger without a seam.
package loggingtest

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// Park holds the first goroutine that logs one message until the test lets
// it go. ParkOn installs it; Wait and Release drive it.
//
// Only the FIRST record carrying the message parks. A later one passes
// straight through, so a job that is retried after the release logs the
// same line without stopping again. Any goroutine that logs the message
// while the first is still parked waits behind it.
type Park struct {
	msg        string
	hold       sync.Once // only the first matching record parks
	parked     chan struct{}
	resume     chan struct{}
	resumeOnce sync.Once
}

// ParkOn points slog.Default at a handler that parks the first goroutine to
// log msg, and restores the previous default when the test ends. Every other
// record is dropped.
//
// Release before anything waits for the parked goroutine. A pool's Stop
// joins its workers, so `defer pool.Stop(); defer park.Release()`, in that
// order, lets the worker go first on every way out of the test, a failed
// assertion included. The restore here is a cleanup and runs after both.
func ParkOn(t testing.TB, msg string) *Park {
	t.Helper()
	p := &Park{msg: msg, parked: make(chan struct{}), resume: make(chan struct{})}
	prev := slog.Default()
	slog.SetDefault(slog.New(parkHandler{p}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return p
}

// Wait blocks until a goroutine is parked in the log call, failing the test
// if none arrives. A deadline rather than a bare receive, so a message that
// is never logged reads as a failure and not as a hung test binary.
func (p *Park) Wait(t testing.TB) {
	t.Helper()
	select {
	case <-p.parked:
	case <-time.After(3 * time.Second):
		t.Fatalf("nothing logged %q within 3s", p.msg)
	}
}

// Release lets the parked goroutine go. Idempotent, because a test calls it
// inline and defers it as well.
func (p *Park) Release() { p.resumeOnce.Do(func() { close(p.resume) }) }

// parkHandler is the slog.Handler behind Park. Every other record is
// dropped.
type parkHandler struct{ p *Park }

// Enabled accepts every level, so the record reaches Handle whatever its
// level.
func (h parkHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle parks the first goroutine whose record carries the watched message.
func (h parkHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.p.msg {
		h.p.hold.Do(func() {
			close(h.p.parked)
			<-h.p.resume
		})
	}
	return nil
}

// WithAttrs returns the same handler: the attributes do not decide anything.
func (h parkHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup returns the same handler, for the reason WithAttrs does.
func (h parkHandler) WithGroup(string) slog.Handler { return h }
