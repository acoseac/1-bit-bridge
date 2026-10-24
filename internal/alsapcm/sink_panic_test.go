package alsapcm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sinkPanicValue is what the sink tests' panics carry: a value a panic could
// carry in the bridge, a library path, which no line of the log may show.
const sinkPanicValue = "a value the panic carries: /srv/music/Secret/Track.flac"

// logLines keeps every record a sink logs, rendered as key=value.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }

func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, b.String())
	return nil
}

func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }

// panicLogged requires one recovered panic in the log, naming the sink's
// writer, with its stack, and the panic's value in no line of the log; it
// returns the line.
func (l *logLines) panicLogged(t *testing.T) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range l.lines {
		if strings.Contains(line, sinkPanicMessage) {
			found = append(found, line)
		}
		if strings.Contains(line, sinkPanicValue) {
			t.Errorf("a line of the log carries the panic's value: %s", line)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0], "where=the sink's writer") || !strings.Contains(found[0], "stack=") {
		t.Fatalf("the panic logged %q, want one line naming the sink's writer, with its stack", found)
	}
	return found[0]
}

// panicAtNextClock makes the next read of the fake's clock panic with v: the
// sink's writer is the one goroutine that reads it, to stamp a measurement
// and in the kernel's drain.
func panicAtNextClock(k *fakeKernel, v any) {
	k.atNow(func() bool { return true }, func() { panic(v) })
}

// TestSinkEndsTypedWhenItsWriterPanics pins a panic on the sink's writer:
// recovered there and logged once (where, the endpoint, its stack, and its
// value only when it is the runtime's own error), and the sink's end, typed
// ReasonPanicked. The device is closed, once; whatever waited on the writer
// is answered with the end: the command it was carrying out, a Drain under
// way, a producer waiting for room; every later call answers it too, and
// Close returns.
func TestSinkEndsTypedWhenItsWriterPanics(t *testing.T) {
	for _, c := range []struct {
		name string
		// run brings the writer to a read of the clock, which panics, and
		// returns the answer of what waited on the writer.
		run func(t *testing.T, k *fakeKernel, s *Sink, waits <-chan struct{}) error
		// logged is what the line must hold besides.
		logged string
	}{
		{name: "carrying out a command", run: func(t *testing.T, k *fakeKernel, s *Sink, _ <-chan struct{}) error {
			submit(t, s, content(0, 0, 9600))
			await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait on the stream")
			panicAtNextClock(k, sinkPanicValue)
			// A pause measures the stream first, and the measurement is stamped.
			return s.Pause()
		}},
		{name: "draining", run: func(t *testing.T, k *fakeKernel, s *Sink, _ <-chan struct{}) error {
			submit(t, s, content(0, 0, 9600))
			await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait on the stream")
			panicAtNextClock(k, sinkPanicValue)
			// The kernel's drain takes its deadline from the clock first.
			return s.Drain(context.Background())
		}},
		{name: "with the producer waiting for room", run: func(t *testing.T, k *fakeKernel, s *Sink, waits <-chan struct{}) error {
			written := make(chan error, 1)
			go func() {
				_, err := s.Write(context.Background(), content(0, 0, 48000))
				written <- err
			}()
			await(t, waits, "the producer to wait for room")
			await(t, k.when(starved), "the writer to fill the hardware buffer and wait for room")
			panicAtNextClock(k, sinkPanicValue)
			// Room for the writer's next write, after which it measures.
			k.grant(100 * time.Millisecond)
			return receive(t, written, "the producer's Write")
		}},
		{name: "the runtime's own error", logged: "error=runtime error: index out of range",
			run: func(t *testing.T, k *fakeKernel, s *Sink, _ <-chan struct{}) error {
				submit(t, s, content(0, 0, 9600))
				await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait on the stream")
				k.atNow(func() bool { return true }, func() {
					var none []int
					_ = none[len(none)]
				})
				return s.Pause()
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			logs := &logLines{}
			cfg := sinkConfig()
			cfg.Lookahead = 100 * time.Millisecond
			cfg.Log = slog.New(logs)
			waits := make(chan struct{})
			var waited sync.Once
			s := openTestSinkWith(t, context.Background(), k, cfg, sinkSeams{producerWaits: func() { waited.Do(func() { close(waits) }) }})
			err := c.run(t, k, s, waits)
			wantReason(t, err, ReasonPanicked)
			p := waitState(t, s, SinkEnded)
			e := wantReason(t, p.Err, ReasonPanicked)
			if strings.Contains(e.Error(), sinkPanicValue) {
				t.Errorf("the end names the panic's value: %v", e)
			}
			k.checkClosedEach(t)
			k.inspect(func(k *fakeKernel) {
				if k.isOpen || k.closes != 1 {
					t.Errorf("after the writer's panic the node is open %v, closed %d times; want closed once", k.isOpen, k.closes)
				}
			})
			if n, err := s.Write(context.Background(), content(0, 0, 10)); n != 0 || ReasonOf(err) != ReasonPanicked {
				t.Errorf("a Write after the end: %d, %v; want nothing taken and the end", n, err)
			}
			wantReason(t, s.Resume(), ReasonPanicked)
			if err := s.Close(); err != nil {
				t.Errorf("Close after the end: %v", err)
			}
			line := logs.panicLogged(t)
			if c.logged != "" && !strings.Contains(line, c.logged) {
				t.Errorf("the line does not hold %q: %s", c.logged, line)
			}
		})
	}
}

// TestASinkLetsGoOfANodeWhoseCheckPanics pins a panic in the steps between a
// node's open and the sink taking it (the check after open, which the player
// gives as the binding's check over sysfs, and the kernel's setup of the
// stream): the node is closed whichever goroutine panicked, the open's
// caller's, whose panic it is to recover, or, at a reopen after a release,
// the writer's, which ends the sink typed.
func TestASinkLetsGoOfANodeWhoseCheckPanics(t *testing.T) {
	opener := func(k *fakeKernel) sinkSeams {
		return sinkSeams{open: func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) }, now: k.now}
	}
	callersPanic := func(f func()) (r any) {
		defer func() { r = recover() }()
		f()
		return nil
	}
	t.Run("the check after the open", func(t *testing.T) {
		k := newModel(t)
		cfg := sinkConfig()
		cfg.Admit = func(NodeInfo) error { panic(sinkPanicValue) }
		if r := callersPanic(func() { _, _ = openSink(context.Background(), cfg, opener(k)) }); r != sinkPanicValue {
			t.Fatalf("the open's caller recovered %v, want the check's panic", r)
		}
		k.inspect(func(k *fakeKernel) {
			if k.isOpen || k.closes != 1 {
				t.Errorf("after the check's panic the node is open %v, closed %d times; want closed once", k.isOpen, k.closes)
			}
		})
	})
	t.Run("the kernel's setup of the stream", func(t *testing.T) {
		k := newModel(t)
		k.commit = func(*hwParams) { panic(sinkPanicValue) }
		if r := callersPanic(func() { _, _ = openSink(context.Background(), sinkConfig(), opener(k)) }); r != sinkPanicValue {
			t.Fatalf("the open's caller recovered %v, want the setup's panic", r)
		}
		k.inspect(func(k *fakeKernel) {
			if k.isOpen || k.closes != 1 {
				t.Errorf("after the setup's panic the node is open %v, closed %d times; want closed once", k.isOpen, k.closes)
			}
		})
	})
	t.Run("the check after a reopen", func(t *testing.T) {
		k := newModel(t)
		logs := &logLines{}
		cfg := sinkConfig()
		cfg.ReleaseAfter = time.Second
		cfg.Log = slog.New(logs)
		var checks atomic.Int32
		cfg.Admit = func(NodeInfo) error {
			if checks.Add(1) > 1 {
				panic(sinkPanicValue)
			}
			return nil
		}
		s := openTestSink(t, k, cfg)
		submit(t, s, content(0, 0, 30000))
		await(t, k.when(starved), "the writer to fill the hardware buffer")
		k.play(t, 100*time.Millisecond)
		if err := s.Pause(); err != nil {
			t.Fatal(err)
		}
		k.grant(2 * time.Second)
		waitState(t, s, SinkReleased)
		k.revoke()
		wantReason(t, s.Resume(), ReasonPanicked)
		waitState(t, s, SinkEnded)
		k.checkClosedEach(t)
		k.inspect(func(k *fakeKernel) {
			if k.isOpen || k.opens != 2 || k.closes != 2 {
				t.Errorf("after the reopen's check panicked: open %v after %d opens and %d closes; want each closed once", k.isOpen, k.opens, k.closes)
			}
		})
		logs.panicLogged(t)
	})
}
