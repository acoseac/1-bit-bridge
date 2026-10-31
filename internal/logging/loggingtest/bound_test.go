package loggingtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// mutableValuer is a LogValuer whose value changes after it is used: slog's
// handlers write what it resolved to when With bound it, or when the record
// was handled.
type mutableValuer struct{ s *string }

func (v mutableValuer) LogValue() slog.Value { return slog.StringValue(*v.s) }

// boundCase is one record a logger writes after deriving itself with With
// and WithGroup, and the shape the recorder must keep of it (shapeOf).
type boundCase struct {
	name  string
	log   func(*slog.Logger)
	shape string
}

// boundCases are the derivations the Text and JSON handlers write one way
// and the recorder must keep the same way. Left out: what Go 1.27.3's
// handlers write against slog's own Handler contract, which the recorder
// keeps instead (TestSlogsHandlersWriteAnIgnoredGroupAgainstTheContract):
// a group whose attributes are all empty, with something after it, and a
// With that binds only empty attributes beside an empty group after a group
// nothing has been added to yet, which the JSON handler writes as
// `"name":{}` and no record can hold.
func boundCases() []boundCase {
	ctx := context.Background()
	return []boundCase{
		{"an attribute With binds", func(l *slog.Logger) { l.With("path", "/abs/with").Info("refused") }, "path=/abs/with"},
		{"a group WithGroup opens", func(l *slog.Logger) { l.WithGroup("abs-group").Info("m", "k", "v") }, "abs-group{k=v}"},
		{"With, then WithGroup, then With", func(l *slog.Logger) {
			l.With("a", 1).WithGroup("g").With("b", 2).Info("m", "c", 3)
		}, "a=1 g{b=2 c=3}"},
		{"a group inside a group", func(l *slog.Logger) {
			l.WithGroup("outer").With("x", "1").WithGroup("inner").Info("m", "k", "v")
		}, "outer{x=1 inner{k=v}}"},
		{"a key bound and logged again, kept twice", func(l *slog.Logger) {
			l.With("k", "bound").Info("m", "k", "logged")
		}, "k=bound k=logged"},
		{"Withs before a group stay outside it", func(l *slog.Logger) {
			l.With("x", "1").With("y", "2").WithGroup("g").Info("m", "z", "3")
		}, "x=1 y=2 g{z=3}"},
		{"a group nothing is added to is not written", func(l *slog.Logger) {
			l.With("x", "1").WithGroup("/abs/unused").Info("m")
		}, "x=1"},
		{"a group holding only an empty attribute is not written", func(l *slog.Logger) {
			l.WithGroup("/abs/group").LogAttrs(ctx, slog.LevelInfo, "m", slog.Attr{})
		}, ""},
		{"an empty attribute bound with With is dropped", func(l *slog.Logger) {
			l.With(slog.Attr{}).Info("m", "k", "v")
		}, "k=v"},
		{"an empty attribute bound inside a group leaves the group to the record", func(l *slog.Logger) {
			l.WithGroup("g").With(slog.Attr{}).Info("m", "k", "v")
		}, "g{k=v}"},
		{"a bound group of only empty attributes is dropped", func(l *slog.Logger) {
			l.WithGroup("/abs/pending").With(slog.Group("/abs/empty-bound", slog.Attr{})).Info("m")
		}, ""},
		{"a group bound with With", func(l *slog.Logger) {
			l.With(slog.Group("bound", slog.String("x", "1"))).Info("m")
		}, "bound{x=1}"},
		{"an inline group is flattened", func(l *slog.Logger) {
			l.Info("m", slog.Group("", slog.String("k", "v")), "after", "w")
		}, "k=v after=w"},
		{"an inline group bound with With is flattened", func(l *slog.Logger) {
			l.WithGroup("g").With(slog.Group("", slog.String("k", "v"))).Info("m")
		}, "g{k=v}"},
		{"an empty group in the record is not written", func(l *slog.Logger) {
			l.WithGroup("/abs/wrapping").Info("m", slog.Group("/abs/empty"))
		}, ""},
		{"an error and a number", func(l *slog.Logger) {
			l.With("n", 7).WithGroup("g").Warn("m", "err", errors.New("read /abs/error"))
		}, "n=7 g{err=read /abs/error}"},
		{"a LogValuer bound with With is what it was when bound", func(l *slog.Logger) {
			s := "/abs/bound"
			bound := l.With("v", mutableValuer{&s})
			s = "changed after binding"
			bound.Info("m")
		}, "v=/abs/bound"},
		{"a LogValuer in a group bound with With is what it was when bound", func(l *slog.Logger) {
			s := "/abs/bound-in-group"
			bound := l.With(slog.Group("g", "v", mutableValuer{&s}))
			s = "changed after binding"
			bound.Info("m")
		}, "g{v=/abs/bound-in-group}"},
		{"a LogValuer in the record is what it was when the record was handled", func(l *slog.Logger) {
			s := "/abs/logged"
			l.Info("m", "v", mutableValuer{&s})
			s = "changed after logging"
		}, "v=/abs/logged"},
	}
}

// TestTheRecorderKeepsWhatTheHandlersWrite drives each chain through
// slog's Text and JSON handlers and through a Recorder. Strings reports
// every key, group name and value either handler wrote, and nothing neither
// wrote; and the record keeps the structure they wrote: an attribute bound
// before a group at the root, one bound after it inside, the order as
// written, a key written twice twice, a LogValuer as it resolved when it was
// bound or handled.
func TestTheRecorderKeepsWhatTheHandlersWrite(t *testing.T) {
	for _, tc := range boundCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRecorder()
			tc.log(r.Logger())
			recs := r.Records()
			if len(recs) != 1 {
				t.Fatalf("kept %d records, want 1", len(recs))
			}
			if got := shapeOf(recs[0]); got != tc.shape {
				t.Errorf("kept %q, want %q", got, tc.shape)
			}

			wrote := map[string]bool{}
			for _, s := range textWrote(t, tc) {
				wrote[s] = true
			}
			for _, s := range jsonWrote(t, tc) {
				wrote[s] = true
			}
			reported := map[string]bool{}
			for _, s := range Strings(recs[0]) {
				reported[s] = true
			}
			for s := range wrote {
				if !reported[s] {
					t.Errorf("a handler wrote %q, and Strings does not report it", s)
				}
			}
			for s := range reported {
				if !wrote[s] {
					t.Errorf("Strings reports %q, which neither handler wrote", s)
				}
			}
		})
	}
}

// TestTheRecorderPassesSlogsOwnHandlerTests runs Go's own conformance suite
// for a slog.Handler (testing/slogtest) on the recorder, its records read
// back as the maps the suite reads a handler's output as: With and
// WithGroup in every order, empty attributes and groups ignored, an inline
// group's attributes in its place, every value resolved.
func TestTheRecorderPassesSlogsOwnHandlerTests(t *testing.T) {
	var r *Recorder
	slogtest.Run(t, func(*testing.T) slog.Handler {
		r = NewRecorder()
		return r.Logger().Handler()
	}, func(t *testing.T) map[string]any {
		recs := r.Records()
		if len(recs) != 1 {
			t.Fatalf("kept %d records, want 1", len(recs))
		}
		m := map[string]any{slog.LevelKey: recs[0].Level, slog.MessageKey: recs[0].Message}
		if !recs[0].Time.IsZero() {
			m[slog.TimeKey] = recs[0].Time
		}
		recs[0].Attrs(func(a slog.Attr) bool {
			putAttr(m, a)
			return true
		})
		return m
	})
}

// putAttr puts a into m as slogtest reads a handler's output: a group as a
// map of its own, any other value as Value.Any gives it.
func putAttr(m map[string]any, a slog.Attr) {
	if a.Value.Kind() != slog.KindGroup {
		m[a.Key] = a.Value.Any()
		return
	}
	g := map[string]any{}
	for _, member := range a.Value.Group() {
		putAttr(g, member)
	}
	m[a.Key] = g
}

// TestSlogsHandlersWriteAnIgnoredGroupAgainstTheContract pins a defect of Go
// 1.27.3's own handlers, where the recorder keeps slog's Handler contract
// ("If a group has no Attrs (even if it has a non-empty key), ignore it",
// log/slog's Handler doc) instead: for a group whose attributes are all
// empty, followed by another attribute, the Text handler leaves the group's
// name in the prefix of the keys after it, and the JSON handler drops the
// separator before them. If this goes red, the toolchain has fixed it, and
// the note in recordHandler's doc goes with this test.
func TestSlogsHandlersWriteAnIgnoredGroupAgainstTheContract(t *testing.T) {
	logIt := func(l *slog.Logger) { l.Info("m", slog.Group("/abs/holds-nothing", slog.Attr{}), "k", "v") }
	var text, js bytes.Buffer
	logIt(slog.New(slog.NewTextHandler(&text, &slog.HandlerOptions{ReplaceAttr: dropTime})))
	logIt(slog.New(slog.NewJSONHandler(&js, &slog.HandlerOptions{ReplaceAttr: dropTime})))
	if !strings.Contains(text.String(), "/abs/holds-nothing.k=v") {
		t.Errorf("the Text handler no longer leaves the ignored group's name on the next key: %q", text.String())
	}
	if json.Valid(bytes.TrimSpace(js.Bytes())) {
		t.Errorf("the JSON handler now writes valid JSON for the ignored group: %q", js.String())
	}
	r := NewRecorder()
	logIt(r.Logger())
	if got := shapeOf(r.Records()[0]); got != "k=v" {
		t.Errorf("kept %q, want the contract's %q", got, "k=v")
	}
}

// TestADerivationThatAddsNothingIsTheHandlerItself pins slog's Handler
// contract: WithGroup with an empty name, and WithAttrs with no attribute a
// handler would write, return the receiver.
func TestADerivationThatAddsNothingIsTheHandlerItself(t *testing.T) {
	base := NewRecorder().Logger().Handler()
	derived := base.WithAttrs([]slog.Attr{slog.String("k", "v")}).WithGroup("g")
	for _, h := range []slog.Handler{base, derived} {
		for name, got := range map[string]slog.Handler{
			`WithGroup("")`:          h.WithGroup(""),
			"WithAttrs(nil)":         h.WithAttrs(nil),
			"WithAttrs of no attrs":  h.WithAttrs([]slog.Attr{}),
			"WithAttrs(empty group)": h.WithAttrs([]slog.Attr{slog.Group("empty")}),
			"WithAttrs(Attr{})":      h.WithAttrs([]slog.Attr{{}}),
		} {
			if got != h {
				t.Errorf("%s returned another handler", name)
			}
		}
	}
}

// TestDerivedLoggersShareNoStorage: siblings derived from one parent keep
// their own bindings (the parent's chain has room to spare, so a derivation
// that appended into it would hand the second sibling's binding to the
// first), one logger's records keep their own attributes from one emission
// to the next, and loggers derived and logged on goroutines at once keep
// theirs (run under -race).
func TestDerivedLoggersShareNoStorage(t *testing.T) {
	t.Parallel()
	r := NewRecorder()
	// Three Withs: a chain of three, which an append grows to room for four.
	parent := r.Logger().With("p1", "1").With("p2", "2").With("p3", "3")
	a, b := parent.With("sib", "a"), parent.With("sib", "b")
	ga, gb := parent.WithGroup("ga"), parent.WithGroup("gb")
	a.Info("a")
	b.Info("b")
	ga.Info("ga", "k", "1")
	gb.Info("gb", "k", "2")
	a.Info("again", "x", "1")
	a.Info("again", "y", "2")

	var wg sync.WaitGroup
	const goroutines, lines = 8, 40
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := parent.WithGroup(fmt.Sprintf("g%d", i)).With("i", i)
			for j := range lines {
				l.Info("concurrent", "j", j)
			}
		}()
	}
	wg.Wait()

	want := map[string][]string{
		"a":     {"p1=1 p2=2 p3=3 sib=a"},
		"b":     {"p1=1 p2=2 p3=3 sib=b"},
		"ga":    {"p1=1 p2=2 p3=3 ga{k=1}"},
		"gb":    {"p1=1 p2=2 p3=3 gb{k=2}"},
		"again": {"p1=1 p2=2 p3=3 sib=a x=1", "p1=1 p2=2 p3=3 sib=a y=2"},
	}
	got := map[string][]string{}
	concurrent := map[string]bool{}
	for _, rec := range r.Records() {
		if rec.Message == "concurrent" {
			concurrent[shapeOf(rec)] = true
			continue
		}
		got[rec.Message] = append(got[rec.Message], shapeOf(rec))
	}
	for msg, w := range want {
		if strings.Join(got[msg], "|") != strings.Join(w, "|") {
			t.Errorf("%q kept %q, want %q", msg, got[msg], w)
		}
	}
	for i := range goroutines {
		for j := range lines {
			if s := fmt.Sprintf("p1=1 p2=2 p3=3 g%d{i=%d j=%d}", i, i, j); !concurrent[s] {
				t.Errorf("no concurrent record %q", s)
			}
		}
	}
	if len(concurrent) != goroutines*lines {
		t.Errorf("kept %d distinct concurrent records, want %d", len(concurrent), goroutines*lines)
	}
}

// TestRecordAndParkOnKeepWhatTheDefaultLoggerBinds: under Record and under
// ParkOn, a logger derived from slog.Default keeps what With binds and the
// name of the group WithGroup opens.
func TestRecordAndParkOnKeepWhatTheDefaultLoggerBinds(t *testing.T) {
	for _, in := range []struct {
		name    string
		install func(*testing.T) *Recorder
	}{
		{"Record", func(t *testing.T) *Recorder { return Record(t) }},
		{"ParkOn", func(t *testing.T) *Recorder { return &ParkOn(t, "a message this test never logs").Recorder }},
	} {
		t.Run(in.name, func(t *testing.T) {
			r := in.install(t)
			slog.Default().With("path", "/abs/default-with").WithGroup("/abs/default-group").Info("refused", "k", "v")
			for _, v := range []string{"/abs/default-with", "/abs/default-group"} {
				if got := r.Carrying(v); len(got) != 1 {
					t.Errorf("Carrying(%q) = %q, want the one record", v, got)
				}
			}
		})
	}
}

// componentBuiltBeforeAnyInstall is a component logger made at package init,
// before any test installs a Recorder, as every package-level
// logging.Component logger is.
var componentBuiltBeforeAnyInstall = logging.Component("loggingtest-b604")

// TestAComponentLoggerKeepsWhatItBinds drives the path every component
// takes: logging.Component's handler replays its own chain onto whatever
// slog.Default is when it logs, here a Recorder or a Park, with With and
// WithGroup interleaved. Its component attribute stays at the root.
func TestAComponentLoggerKeepsWhatItBinds(t *testing.T) {
	const want = "component=loggingtest-b604 a=/abs/a g{b=/abs/b h{c=/abs/c}}"
	for _, in := range []struct {
		name    string
		install func(*testing.T) *Recorder
	}{
		{"Record", func(t *testing.T) *Recorder { return Record(t) }},
		{"ParkOn", func(t *testing.T) *Recorder { return &ParkOn(t, "a message this test never logs").Recorder }},
	} {
		t.Run(in.name, func(t *testing.T) {
			r := in.install(t)
			componentBuiltBeforeAnyInstall.With("a", "/abs/a").WithGroup("g").With("b", "/abs/b").WithGroup("h").
				Info("component line", "c", "/abs/c")
			var shapes []string
			for _, rec := range r.Records() {
				if rec.Message == "component line" {
					shapes = append(shapes, shapeOf(rec))
				}
			}
			if len(shapes) != 1 || shapes[0] != want {
				t.Errorf("kept %q, want [%q]", shapes, want)
			}
		})
	}
}

// TestParkingKeepsItsInvariantsThroughDerivedLoggers: a Park is one, shared
// by every logger derived from its handler. The first matching line, through
// either of two derived loggers, parks, and its record (what its logger
// bound included) is kept before the Park says it arrived, and can be read
// while it is parked, so the Recorder's lock is not held across the park. A
// second matching line waits behind it, its record kept too, and both go on
// once the test releases them; a later matching line passes straight
// through, and nothing closes the Park's channel twice.
func TestParkingKeepsItsInvariantsThroughDerivedLoggers(t *testing.T) {
	p := ParkOn(t, "parked line")
	defer p.Release()
	first := slog.Default().With("first", "/abs/first")
	second := slog.Default().WithGroup("g").With("second", "/abs/second")

	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(firstDone); first.Info("parked line", "n", 1) }()
	p.Wait(t)
	if got := p.Carrying("/abs/first"); len(got) != 1 {
		t.Errorf("the parked record with what its logger bound is not kept: %q", p.All())
	}
	go func() { defer close(secondDone); second.Info("parked line", "n", 2) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(p.Carrying("/abs/second")) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the second matching line's record was not kept: %q", p.All())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-secondDone:
		t.Error("the second matching line went on while the first was parked")
	case <-firstDone:
		t.Error("the parked line went on before the release")
	default:
	}
	p.Release()
	for _, done := range []chan struct{}{firstDone, secondDone} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a matching line did not go on after the release")
		}
	}
	first.Info("parked line", "n", 3)
	second.Info("parked line", "n", 4)
	if got := p.Lines("parked line"); len(got) != 4 {
		t.Errorf("kept %d matching lines, want 4: %q", len(got), got)
	}
}

// shapeOf renders rec's attributes as `key=value`, a group as
// `name{…}`, in their order, a value as its Value.String writes it
// unresolved: what the record holds, not what Strings makes of it.
func shapeOf(rec slog.Record) string {
	var parts []string
	rec.Attrs(func(a slog.Attr) bool {
		parts = append(parts, shapeOfAttr(a))
		return true
	})
	return strings.Join(parts, " ")
}

func shapeOfAttr(a slog.Attr) string {
	if a.Value.Kind() != slog.KindGroup {
		return a.Key + "=" + a.Value.String()
	}
	var in []string
	for _, m := range a.Value.Group() {
		in = append(in, shapeOfAttr(m))
	}
	return a.Key + "{" + strings.Join(in, " ") + "}"
}

// dropTime leaves the record's time out of a handler's line, so a test can
// read the rest of it.
func dropTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

// textWrote is what slog's Text handler writes of tc's record, its time and
// its level aside: each key's group names and own name (split at the dot the
// handler joins them with), each value, and the message, unquoted.
func textWrote(t *testing.T, tc boundCase) []string {
	t.Helper()
	var buf bytes.Buffer
	tc.log(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime})))
	line := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("the Text handler wrote more than one line: %q", buf.String())
	}
	var out []string
	for line != "" {
		var key, value string
		key, line = textToken(t, line, '=')
		value, line = textToken(t, line, ' ')
		switch key {
		case slog.LevelKey:
		case slog.MessageKey:
			out = append(out, value)
		default:
			out = append(out, strings.Split(key, ".")...)
			out = append(out, value)
		}
	}
	return out
}

// textToken reads one key or value from the front of s, quoted or not, and
// returns it and what follows the separator after it.
func textToken(t *testing.T, s string, sep byte) (string, string) {
	t.Helper()
	if strings.HasPrefix(s, `"`) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			t.Fatalf("a quoted token does not end: %q", s)
		}
		v, err := strconv.Unquote(q)
		if err != nil {
			t.Fatalf("unquote %q: %v", q, err)
		}
		rest := s[len(q):]
		if rest != "" {
			rest = rest[1:]
		}
		return v, rest
	}
	if i := strings.IndexByte(s, sep); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// jsonWrote is what slog's JSON handler writes of tc's record, its time and
// its level aside: each group's name, each key, each value and the message,
// read token by token so that order and a key written twice survive.
func jsonWrote(t *testing.T, tc boundCase) []string {
	t.Helper()
	var buf bytes.Buffer
	tc.log(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropTime})))
	dec := json.NewDecoder(&buf)
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("the JSON handler did not write an object: %v %v", tok, err)
	}
	var out []string
	var walk func(depth int)
	walk = func(depth int) {
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("read a key: %v", err)
			}
			key := tok.(string)
			tok, err = dec.Token()
			if err != nil {
				t.Fatalf("read the value of %q: %v", key, err)
			}
			if d, ok := tok.(json.Delim); ok && d == '{' {
				out = append(out, key)
				walk(depth + 1)
				if _, err := dec.Token(); err != nil {
					t.Fatalf("close the group %q: %v", key, err)
				}
				continue
			}
			switch {
			case depth == 0 && key == slog.LevelKey:
			case depth == 0 && key == slog.MessageKey:
				out = append(out, fmt.Sprint(tok))
			default:
				out = append(out, key, fmt.Sprint(tok))
			}
		}
	}
	walk(0)
	if _, err := dec.Token(); err != nil {
		t.Fatalf("close the object: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("the JSON handler wrote more than one object: %q", buf.String())
	}
	return out
}
