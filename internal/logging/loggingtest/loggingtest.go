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
//
// SetDefault is the way both install their handler, and the way any other
// test that points slog.Default at its own logger does: it puts back the
// log package's output and flags as well as the previous default, which a
// bare slog.SetDefault(prev) does not. TestNoTestSetsTheDefaultLoggerByHand,
// in cmd/bridge, refuses a test file that calls slog.SetDefault by hand
// outside this package's own tests, or logging.Init outside
// internal/logging's. What SetDefault changes is one per process, so it
// refuses a parallel test, as t.Setenv does.
package loggingtest

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recorder keeps every record logged through slog.Default while it is
// installed. Record installs one; Failures reads it.
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

// Failures returns, rendered one per line, every record logged so far at
// Warn or above, and only those whose message is one of msgs when any are
// given. A pass that logs its outcome at Info under the same message as its
// failure (the scanner's "duplicate stamping") is why the level is asked.
func (r *Recorder) Failures(msgs ...string) []string {
	return r.render(func(rec slog.Record) bool {
		if rec.Level < slog.LevelWarn {
			return false
		}
		if len(msgs) == 0 {
			return true
		}
		for _, m := range msgs {
			if rec.Message == m {
				return true
			}
		}
		return false
	})
}

// Lines returns, rendered one per line, every record logged so far whose
// message is msg, at any level. A pass's summary is logged at Info, below
// what Failures returns.
func (r *Recorder) Lines(msg string) []string {
	return r.render(func(rec slog.Record) bool { return rec.Message == msg })
}

// All returns, rendered one per line, every record logged so far, at any
// level and under any message: what a test searches when the question is
// whether ANY line carries a value (a credential that must reach no line).
func (r *Recorder) All() []string {
	return r.render(func(slog.Record) bool { return true })
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

// install points slog.Default at h until the test ends, through SetDefault.
func install(t testing.TB, h slog.Handler) {
	SetDefault(t, slog.New(h))
}

// SetDefault makes l the default slog logger until the test ends, and then
// puts back everything slog.SetDefault changed: the previous default, and
// the log package's output and flags.
//
// Putting back the previous default alone does not undo the rest.
// slog.SetDefault also points the log package's output at l's handler and
// zeroes its flags, and a default whose handler is slog's own (the one
// every test binary starts with) is restored without either being undone:
// that handler writes THROUGH the log package, whose output still points at
// the handler the finished test installed. Every later line in the binary,
// a failing test's own diagnostics included, then went into that test's
// buffer. Measured in internal/dlna/discovery (2026-09-28): of 200 runs of
// one test, only the first run's lines reached stderr.
//
// The output and flags are put back AFTER the previous default, and the
// order is load-bearing. Putting back a default whose handler is not
// slog's own points the log package at that handler again and zeroes its
// flags, so in the other order that call has the last word, and the log
// package is left on the previous default's handler instead of the writer
// it had (TestInstallersRestoreTheStandardLogger, over a default the test
// set).
//
// All of that is one per PROCESS, so a test that calls SetDefault (or
// Record, or ParkOn) cannot run in parallel with another that does. Each
// saves what it finds and puts that back at its end, so two that overlap
// put back each other's state: A saves D0 and installs DA, B saves DA and
// installs DB, A's cleanup puts back D0, then B's puts back DA, and the
// default is left on A's finished handler for the rest of the binary,
// which is the defect above, reached another way. It is not a data race
// (slog keeps its default in an atomic pointer, and the log package guards
// its output with a mutex and keeps its flags in an atomic), so -race
// reports nothing. SetDefault refuses it instead, the way the testing
// package refuses process-wide changes: it calls t.Setenv before it changes
// anything, which panics in a test that is parallel or has a parallel
// ancestor, and makes a later t.Parallel in the same test panic too.
func SetDefault(t testing.TB, l *slog.Logger) {
	t.Helper()
	// The panic comes from the testing package and names only t.Setenv:
	// this call is the reason, and it runs first, so a refused test has
	// changed nothing. The value names the test that holds the default.
	t.Setenv("LOGGINGTEST_SETDEFAULT", t.Name())
	prev := slog.Default()
	out, flags := log.Writer(), log.Flags()
	slog.SetDefault(l)
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(out)
		log.SetFlags(flags)
	})
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
