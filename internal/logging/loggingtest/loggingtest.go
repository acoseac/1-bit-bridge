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
//
// The same handler keeps every record it sees, so a test can also ask what
// else was logged: Record installs a Recorder on its own, and a Park is one.
package loggingtest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recorder keeps every record logged through slog.Default while it is
// installed. Record installs one; Logged and Failures read it.
type Recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

// Record points slog.Default at a Recorder, and restores the previous
// default when the test ends. Every level is recorded.
func Record(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{}
	install(t, recordHandler{r})
	return r
}

// keep records one record. Cloned, because a handler must not retain a
// record's attributes past Handle.
func (r *Recorder) keep(rec slog.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
}

// Logged returns, rendered one per line, every record logged so far whose
// message is msg.
func (r *Recorder) Logged(msg string) []string {
	return r.render(func(rec slog.Record) bool { return rec.Message == msg })
}

// Failures returns, rendered one per line, every record logged so far at
// Warn or above.
func (r *Recorder) Failures() []string {
	return r.render(func(rec slog.Record) bool { return rec.Level >= slog.LevelWarn })
}

// render formats the records match accepts as "LEVEL message key=value ...",
// in the order they were logged.
func (r *Recorder) render(match func(slog.Record) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		if !match(rec) {
			continue
		}
		var b strings.Builder
		b.WriteString(rec.Level.String())
		b.WriteByte(' ')
		b.WriteString(rec.Message)
		rec.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
			return true
		})
		out = append(out, b.String())
	}
	return out
}

// recordHandler is the slog.Handler behind Record.
type recordHandler struct{ r *Recorder }

// Enabled accepts every level, so the record reaches Handle whatever its
// level.
func (h recordHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the record.
func (h recordHandler) Handle(_ context.Context, rec slog.Record) error {
	h.r.keep(rec)
	return nil
}

// WithAttrs returns the same handler: the attributes do not decide anything.
func (h recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup returns the same handler, for the reason WithAttrs does.
func (h recordHandler) WithGroup(string) slog.Handler { return h }

// install points slog.Default at h and restores the previous default when
// the test ends.
func install(t testing.TB, h slog.Handler) {
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// Park holds the first goroutine that logs one message until the test lets
// it go. ParkOn installs it; Wait and Release drive it. It records every
// record as a Recorder does, the parked one included.
//
// Only the FIRST record carrying the message parks. A later one passes
// straight through, so a job that is retried after the release logs the
// same line without stopping again. Any goroutine that logs the message
// while the first is still parked waits behind it.
type Park struct {
	Recorder
	msg        string
	hold       sync.Once // only the first matching record parks
	parked     chan struct{}
	resume     chan struct{}
	resumeOnce sync.Once
}

// ParkOn points slog.Default at a handler that parks the first goroutine to
// log msg, and restores the previous default when the test ends.
//
// Release before anything waits for the parked goroutine. A pool's Stop
// joins its workers, so `defer pool.Stop(); defer park.Release()`, in that
// order, lets the worker go first on every way out of the test, a failed
// assertion included. The restore here is a cleanup and runs after both.
func ParkOn(t testing.TB, msg string) *Park {
	t.Helper()
	p := &Park{msg: msg, parked: make(chan struct{}), resume: make(chan struct{})}
	install(t, parkHandler{p})
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

// parkHandler is the slog.Handler behind Park.
type parkHandler struct{ p *Park }

// Enabled accepts every level, so the record reaches Handle whatever its
// level.
func (h parkHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the record, then parks the first goroutine whose record
// carries the watched message.
func (h parkHandler) Handle(_ context.Context, r slog.Record) error {
	h.p.keep(r)
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
