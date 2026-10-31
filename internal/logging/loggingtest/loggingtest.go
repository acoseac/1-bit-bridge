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
	"encoding"
	"encoding/json"
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
	install(t, &recordHandler{r: r})
	return r
}

// NewRecorder returns a Recorder that is not installed as the default, for a
// component a test hands its logger (Logger) rather than one that reads
// slog.Default. It changes nothing process-wide, so a parallel test may use
// it.
func NewRecorder() *Recorder { return &Recorder{} }

// Logger is a logger whose records r keeps, at every level.
func (r *Recorder) Logger() *slog.Logger { return slog.New(&recordHandler{r: r}) }

// Records returns a copy of every record kept so far, in the order they were
// logged: what a test walks with Strings.
func (r *Recorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]slog.Record, len(r.records))
	for i, rec := range r.records {
		out[i] = rec.Clone()
	}
	return out
}

// Strings returns every string a handler could write for rec: its message,
// and each attribute's key and value, a LogValuer's value as it resolves, a
// group's attributes walked, and a value of any other kind as %v and %+v
// format it (the text handler's form, and an error's Error, which for
// errors.Join's error holds each error it joins), as its MarshalText writes
// it, and, for a value that is not an error, as JSON encodes it (the JSON
// handler's form: an error it writes by its Error). A test that must show a
// value reached no line of a record searches these, whichever handler a
// bridge runs.
func Strings(rec slog.Record) []string {
	out := []string{rec.Message}
	rec.Attrs(func(a slog.Attr) bool {
		out = appendAttrStrings(out, a)
		return true
	})
	return out
}

// appendAttrStrings appends what Strings says of a, and of each attribute a
// group value holds.
func appendAttrStrings(out []string, a slog.Attr) []string {
	out = append(out, a.Key)
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, g := range v.Group() {
			out = appendAttrStrings(out, g)
		}
		return out
	}
	out = append(out, v.String())
	if v.Kind() != slog.KindAny {
		return out
	}
	x := v.Any()
	out = append(out, fmt.Sprintf("%v", x), fmt.Sprintf("%+v", x))
	if tm, ok := x.(encoding.TextMarshaler); ok {
		if b, err := tm.MarshalText(); err == nil {
			out = append(out, string(b))
		}
	}
	if _, isErr := x.(error); !isErr {
		if b, err := json.Marshal(x); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// Carrying returns, rendered as Failures renders them, every record kept so
// far one of whose Strings contains any of values: a test asserts it empty to
// show none of them reached a log line. An empty value is ignored, since
// every string contains it.
func (r *Recorder) Carrying(values ...string) []string {
	return r.render(func(rec slog.Record) bool {
		for _, s := range Strings(rec) {
			for _, v := range values {
				if v != "" && strings.Contains(s, v) {
					return true
				}
			}
		}
		return false
	})
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

// recordHandler is the slog.Handler behind Record, a Recorder's Logger and
// ParkOn. It keeps every record with what the loggers derived from it bind,
// as slog's Handler contract says a handler writes them and Go's Text and
// JSON handlers do (log/slog/handler.go): WithAttrs and WithGroup each add an
// op to an ordered chain, and Handle folds the chain over the record's own
// attributes from the innermost op outward, an attrs op putting its
// attributes before what is folded so far and a group op wrapping what is
// folded so far in a group of its name. So an attribute bound before a group
// stays outside it, one bound after it (the record's own included) goes
// inside, and nothing is reordered or merged: a key bound and logged again is
// kept twice.
//
// What the contract ignores is not kept: an empty attribute (Attr{}), a
// group that holds nothing once its own attributes are, its name included,
// and a WithGroup with no name; a group with an empty key is kept as its
// attributes, in its place. A value is resolved when a handler formats it: a
// bound one when With binds it (a LogValuer changed after that still writes
// what it was), the record's own in Handle. A value of any other kind is kept
// as it is and formatted only when Strings or a rendering reads it, so a map
// or a struct changed after it was bound or logged reads as it is then,
// where a handler wrote what it was.
//
// Go 1.27.3's own handlers write one shape against the contract: a group
// whose attributes are all empty, followed by another attribute, whose name
// the Text handler leaves in the prefix of the keys after it and before
// which the JSON handler drops a separator. The recorder keeps the contract
// (TestSlogsHandlersWriteAnIgnoredGroupAgainstTheContract).
//
// A derivation shares no storage with the handler it derives from: its chain
// is a new slice, copied, and a bound attribute is resolved into one, so
// siblings and loggers on several goroutines keep their own. The Recorder and
// a Park are shared by pointer: every handler derived from ParkOn's parks on
// its one Park.
type recordHandler struct {
	r    *Recorder
	park *Park // ParkOn's, or nil
	ops  []recordOp
}

// recordOp is one derivation: a WithGroup's name, or a WithAttrs's
// attributes as they resolved when bound.
type recordOp struct {
	group string
	attrs []slog.Attr
}

// Enabled accepts every level, so the record reaches Handle whatever its
// level.
func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle keeps the record with h's chain folded over its attributes. For a
// Park it then parks the first goroutine whose record carries the watched
// message: once the record is kept, so a test reads it while it is parked,
// and with the Recorder's lock released, which keep holds only while it
// appends.
func (h *recordHandler) Handle(_ context.Context, rec slog.Record) error {
	var attrs []slog.Attr
	rec.Attrs(func(a slog.Attr) bool {
		attrs = appendWritten(attrs, a)
		return true
	})
	for i := len(h.ops) - 1; i >= 0; i-- {
		switch op := h.ops[i]; {
		case op.attrs != nil:
			folded := make([]slog.Attr, 0, len(op.attrs)+len(attrs))
			attrs = append(append(folded, op.attrs...), attrs...)
		case len(attrs) > 0:
			attrs = []slog.Attr{{Key: op.group, Value: slog.GroupValue(attrs...)}}
		}
	}
	kept := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	kept.AddAttrs(attrs...)
	h.r.keep(kept)
	if p := h.park; p != nil && rec.Message == p.msg {
		p.hold.Do(func() {
			close(p.parked)
			<-p.resume
		})
	}
	return nil
}

// WithAttrs returns a handler whose records carry as, resolved now, at the
// place in the chain h has reached: h itself when as holds nothing a handler
// would write.
func (h *recordHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var bound []slog.Attr
	for _, a := range as {
		bound = appendWritten(bound, a)
	}
	if len(bound) == 0 {
		return h
	}
	return h.derive(recordOp{attrs: bound})
}

// WithGroup returns a handler whose records nest what is added after it in a
// group of name: h itself for an empty name, as slog's Handler contract says.
func (h *recordHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.derive(recordOp{group: name})
}

// derive returns a handler with op after h's chain, in a chain of its own,
// so that no handler derived from h later writes where this one reads.
func (h *recordHandler) derive(op recordOp) *recordHandler {
	ops := make([]recordOp, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = op
	return &recordHandler{r: h.r, park: h.park, ops: ops}
}

// appendWritten appends to as what a handler writes of a, resolved: nothing
// for an empty attribute, nor for a group that holds nothing once its own
// attributes are resolved and dropped the same way; a group with an empty
// key's attributes in its place; anything else as it resolved. slog checks
// the same after the same resolution (handleState.appendAttr).
func appendWritten(as []slog.Attr, a slog.Attr) []slog.Attr {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() != slog.KindGroup {
		if a.Key == "" && a.Value.Kind() == slog.KindAny && a.Value.Any() == nil {
			return as
		}
		return append(as, a)
	}
	var members []slog.Attr
	for _, m := range a.Value.Group() {
		members = appendWritten(members, m)
	}
	switch {
	case len(members) == 0:
		return as
	case a.Key == "":
		return append(as, members...)
	}
	return append(as, slog.Attr{Key: a.Key, Value: slog.GroupValue(members...)})
}

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
// record as a Recorder does, the parked one included, what its logger bound
// with it (recordHandler).
//
// Only the FIRST record carrying the message parks, through whichever
// logger derived from the default logs it: they all share this one Park. A
// later one passes straight through, so a job that is retried after the
// release logs the same line without stopping again. Any goroutine that
// logs the message while the first is still parked waits behind it, its
// record kept first.
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
	install(t, &recordHandler{r: &p.Recorder, park: p})
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
