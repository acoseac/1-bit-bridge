package alsapcm_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The lines a recovered panic is logged with: the backend's, for a panic on
// one of its goroutines, and the sink's, for one on a sink's writer.
const (
	backendPanicMessage = "player: a panic was recovered in the backend; the session faults and lets go of the device"
	sinkPanicMessage    = "player: a panic was recovered in the sink's writer; the sink ends and closes its device"
)

// lines keeps every record the backend logs, rendered as key=value.
type lines struct {
	mu    sync.Mutex
	lines []string
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }

func (l *lines) Handle(_ context.Context, r slog.Record) error {
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

func (l *lines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lines) WithGroup(string) slog.Handler      { return l }

// panicLogged requires one recovered panic logged with msg, naming where,
// with its stack, and the panic's value in no line of the log.
func (l *lines) panicLogged(t *testing.T, msg, where string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range l.lines {
		if strings.Contains(line, msg) {
			found = append(found, line)
		}
		if strings.Contains(line, panicValue) {
			t.Errorf("a line of the log carries the panic's value: %s", line)
		}
	}
	if len(found) != 1 || !strings.Contains(found[0], "where="+where) || !strings.Contains(found[0], "stack=") {
		t.Errorf("the panic logged %q, want one line naming %q, with its stack", found, where)
	}
}

// withLog has the rig's backend, and the sinks it opens, log to l.
func withLog(l *lines) func(*rigOptions) { return func(o *rigOptions) { o.log = slog.New(l) } }

// letGo waits for the fake's node to be closed, every open of it: the
// device let go.
func (r *rig) letGo() {
	r.t.Helper()
	r.k.Await(r.t, "the device to be let go", func(v alsapcm.KernelView) bool { return v.Opens > 0 && !v.Open })
}

// playsOn plays a new entry, ref, after a fault: the engine and the backend
// go on, and the device opens again.
func (r *rig) playsOn(ref string) {
	r.t.Helper()
	r.playsOnAfter(ref, nil)
}

// playsOnAfter optionally lets the new playback settle before observing its
// open. With a free-running fake DAC it can have closed again by then.
func (r *rig) playsOnAfter(ref string, settle func()) {
	r.t.Helper()
	r.files.add(ref, wav(48000, 24, pattern(9, 20000, 24)))
	ids := r.insert(ref)
	opens := r.k.View().Opens
	if err := r.eng.PlayID(bg, ids[0]); err != nil {
		r.t.Fatalf("a play after the fault: %v", err)
	}
	if settle != nil {
		settle()
	}
	r.k.Await(r.t, "the device to open again", func(v alsapcm.KernelView) bool { return v.Opens > opens })
}

// wantFailed requires f to be the backend's own fault, naming no value.
func wantFailed(t *testing.T, f engine.Fault, entry uint32) {
	t.Helper()
	if f.Reason != playback.ReasonFailed || f.Entry != entry || strings.Contains(f.Detail, panicValue) {
		t.Errorf("fault %+v; want %s in entry %d, naming no value", f, playback.ReasonFailed, entry)
	}
}

// TestPlaybackFaultsOnAPanicInTheProducer pins a panic on a session's
// producer, here in a read of the content past its first chunk (a decoder's
// defect, or a reader's): recovered there and logged once, and the
// session's fault, typed backend_failed, which the reporter reports, so the
// engine stops playback and releases the session and the device is let go;
// the engine and the backend then play on.
func TestPlaybackFaultsOnAPanicInTheProducer(t *testing.T) {
	logs := &lines{}
	r := newRig(t, withLog(logs))
	r.files.add("a", wav(48000, 24, pattern(1, 200000, 24)))
	ids := r.insert("a")
	r.play()
	// Three seconds is past the sink's two-second lookahead, hardware
	// buffer and decoder chunk. No read reaches it until the DAC can play,
	// so the panic cannot race Play's successful answer.
	r.files.mu.Lock()
	r.files.panicRead["a"] = 44 + 3*48000*2*3
	r.files.mu.Unlock()
	r.k.Free()
	f, _ := r.faulted()
	wantFailed(t, f, ids[0])
	r.letGo()
	logs.panicLogged(t, backendPanicMessage, "the producer")
	// Free still applies to the next session. Force its entire playback
	// before the observation, the scheduling window that used to lose it.
	r.playsOnAfter("b", func() {
		r.waitStopped(engine.ReasonEndOfQueue)
		r.letGo()
	})
}

// TestPlaybackFaultsOnAPanicInTheReporter pins a panic on a session's
// reporter, at its start and as it sends an event: recovered there and
// logged once, and the session's fault, which the reporter, the one
// goroutine that sends the session's events, sends itself, so the engine
// releases the session and the device is let go. The event is a successor's
// Submitted, which the reporter sends once the producer has given the sink
// the successor's first frames, after its Prepared, which reaches the
// engine.
func TestPlaybackFaultsOnAPanicInTheReporter(t *testing.T) {
	for _, c := range []struct {
		name string
		opts func(*rigOptions)
		set  func(r *rig)
	}{
		{name: "at its start", opts: func(o *rigOptions) {
			var once atomic.Bool
			o.onTick = func() {
				if once.CompareAndSwap(false, true) {
					panic(panicValue)
				}
			}
		}},
		{name: "sending an event", set: func(r *rig) {
			r.rec.mu.Lock()
			r.rec.panicOn = func(e event) bool { return e.kind == "submitted" }
			r.rec.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &lines{}
			opts := []func(*rigOptions){withLog(logs)}
			if c.opts != nil {
				opts = append(opts, c.opts)
			}
			r := newRig(t, opts...)
			if c.set != nil {
				c.set(r)
			}
			r.files.add("a", wav(48000, 24, pattern(1, 9000, 24)))
			r.files.add("b", wav(48000, 24, pattern(2, 9000, 24)))
			ids := r.insert("a", "b")
			// The play answers nil once its session started, or with the
			// fault when the engine took the fault first.
			if err := r.eng.Play(bg); err != nil && engine.ReasonOf(err) != engine.ReasonFault {
				t.Fatalf("play: %v, want nil or %s", err, engine.ReasonFault)
			}
			f, _ := r.faulted()
			wantFailed(t, f, ids[0])
			r.letGo()
			logs.panicLogged(t, backendPanicMessage, "the reporter")
			r.playsOn("c")
		})
	}
}

// TestPlaybackFaultsOnAPanicInASuccessorsOpen pins a panic on a successor's
// opener, here in the resolver's open of its content: recovered there and
// logged once, what the open opened closed, and the session's fault, so the
// engine releases the session and the device is let go.
func TestPlaybackFaultsOnAPanicInASuccessorsOpen(t *testing.T) {
	logs := &lines{}
	r := newRig(t, withLog(logs))
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	r.files.add("b", wav(48000, 24, pattern(2, 100000, 24)))
	// Hold a read past Start's first chunk, before EOF. Without this,
	// a fits in the lookahead plus hardware buffer and can enter its final
	// drain before b is inserted; Prepare then answers ErrSessionEnding
	// and no successor ever opens (so no panic can happen).
	parked, release := r.files.stall("a", 44+48000*2*3)
	t.Cleanup(release)
	ids := r.insert("a")
	r.play()
	receiveIn(t, parked, "the producer to wait before EOF")
	r.files.mu.Lock()
	r.files.panicOpen["b"] = true
	r.files.mu.Unlock()
	next := r.insert("b")
	prepared := r.waitCall("b to be handed to the opener", func(c call) bool {
		return c.kind == "prepare" && c.next != nil && c.next.Entry == next[0]
	})
	if prepared.err != nil {
		t.Fatalf("prepare b: %v", prepared.err)
	}
	release()
	f, _ := r.faulted()
	wantFailed(t, f, ids[0])
	r.letGo()
	logs.panicLogged(t, backendPanicMessage, "a successor's opener")
	r.files.mu.Lock()
	r.files.panicOpen["b"] = false
	r.files.mu.Unlock()
	r.playsOn("c")
}

// TestPlaybackLetsGoOfTheDeviceWhenItsTeardownPanics pins a panic in a
// release's teardown: recovered and logged once, and the teardown goes on,
// so every file and every sink is closed, the device let go, and the release
// acknowledged, the engine playing on. The panic comes in the close of an
// occurrence's content, and in the note of a sink's close, once its device
// is closed.
func TestPlaybackLetsGoOfTheDeviceWhenItsTeardownPanics(t *testing.T) {
	for _, c := range []struct {
		name  string
		where string
		opts  func(*rigOptions)
		set   func(r *rig)
	}{
		{name: "closing an occurrence's content", where: "the close of an occurrence's decoder and content", set: func(r *rig) {
			r.files.mu.Lock()
			r.files.panicClose["a"] = true
			r.files.mu.Unlock()
		}},
		{name: "telling of a sink's close", where: "the teardown", opts: func(o *rigOptions) {
			var once atomic.Bool
			o.notify = func(n playback.Note) {
				if n.Kind == playback.NoteClosed && once.CompareAndSwap(false, true) {
					panic(panicValue)
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &lines{}
			opts := []func(*rigOptions){withLog(logs)}
			if c.opts != nil {
				opts = append(opts, c.opts)
			}
			r := newRig(t, opts...)
			r.files.add("a", wav(48000, 24, pattern(1, 200000, 24)))
			if c.set != nil {
				c.set(r)
			}
			r.insert("a")
			r.play()
			r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
			if err := r.eng.Stop(bg); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if _, unconfirmed := r.eng.Snapshot().ReleaseUnconfirmed(); unconfirmed {
				t.Error("the release whose teardown panicked was not acknowledged, its device let go")
			}
			r.letGo()
			logs.panicLogged(t, backendPanicMessage, c.where)
			r.playsOn("b")
		})
	}
}

// TestPlaybackFaultsOnAPanicInACallToASink pins a panic on the goroutine a
// command to a sink runs on (call), here a Pause: recovered there and logged
// once, the call answered with the failure, and the session's fault, so the
// engine releases the session and the device is let go.
func TestPlaybackFaultsOnAPanicInACallToASink(t *testing.T) {
	logs := &lines{}
	r := newRig(t, withLog(logs))
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	ids := r.insert("a")
	r.play()
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	var once atomic.Bool
	r.k.OnCommand(func(_ alsapcm.SinkConfig, op string) {
		if op == "pause" && once.CompareAndSwap(false, true) {
			panic(panicValue)
		}
	})
	// The pause is answered with the call's failure, or, when the fault is
	// taken first, as one the session's release overtook.
	if err := r.eng.Pause(bg); err != nil && engine.ReasonOf(err) != engine.ReasonBackend {
		t.Errorf("the pause whose call panicked answered %v, want nil or %s", err, engine.ReasonBackend)
	}
	f, _ := r.faulted()
	wantFailed(t, f, ids[0])
	r.letGo()
	logs.panicLogged(t, backendPanicMessage, "a call to a sink")
	r.playsOn("b")
}

// TestPlaybackFailsAStartWhoseOpenPanics pins a panic in the open of a
// session's first occurrence, here in a read of its content as it is opened,
// on the engine's call: recovered by the backend and logged once, and the
// session's fault, which the Start answers with, so the engine stops playback
// with the backend's failure and releases the session. The device is never
// opened, and the engine plays on.
func TestPlaybackFailsAStartWhoseOpenPanics(t *testing.T) {
	logs := &lines{}
	r := newRig(t, withLog(logs))
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	r.files.mu.Lock()
	r.files.panicRead["a"] = 100
	r.files.mu.Unlock()
	r.insert("a")
	if err := r.eng.Play(bg); engine.ReasonOf(err) != engine.ReasonBackend || strings.Contains(err.Error(), panicValue) {
		t.Fatalf("the play whose start panicked answered %v, want %s naming no value", err, engine.ReasonBackend)
	}
	r.waitStopped(engine.ReasonBackend)
	if v := r.k.View(); v.Opens != 0 {
		t.Errorf("the device was opened %d times for a start whose content's open panicked, want never", v.Opens)
	}
	logs.panicLogged(t, backendPanicMessage, "the backend's start")
	r.files.mu.Lock()
	delete(r.files.panicRead, "a")
	r.files.mu.Unlock()
	r.playsOn("b")
}

// TestPlaybackLetsGoOfTheSessionsLockWhenAPanicComesUnderIt pins a panic
// while a goroutine holds the session's lock: here the engine's Pause, which
// finds no device to pause (a's sink playing out its last frames for b's
// transport) and arms the pause's own release (Config.AfterFunc) under the
// lock. The hold is let go of as the panic goes, so the call's recover
// records the session's fault, the engine releases the session, the release
// is confirmed and the device let go.
func TestPlaybackLetsGoOfTheSessionsLockWhenAPanicComesUnderIt(t *testing.T) {
	logs := &lines{}
	var once atomic.Bool
	r := newRig(t, withLog(logs), func(o *rigOptions) {
		o.pauseRelease = time.Second
		o.afterFunc = func(d time.Duration, f func()) func() bool {
			if once.CompareAndSwap(false, true) {
				panic(panicValue)
			}
			return time.AfterFunc(d, f).Stop
		}
	})
	r.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	r.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
	ids := r.insert("a", "b")
	r.play()
	// a, 1000 frames, is shorter than the hardware buffer: the change of
	// transport drains it, and the kernel's drain waits for time.
	r.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	// The pause answers with the call's failure, or, when the fault is taken
	// first, as one the session's release overtook.
	if err := r.eng.Pause(bg); err != nil && engine.ReasonOf(err) != engine.ReasonBackend {
		t.Errorf("the pause that panicked answered %v, want nil or %s", err, engine.ReasonBackend)
	}
	f, s := r.faulted()
	wantFailed(t, f, ids[0])
	if _, unconfirmed := s.ReleaseUnconfirmed(); unconfirmed {
		t.Error("the release of the session a panic under its lock faulted is not confirmed")
	}
	r.letGo()
	logs.panicLogged(t, backendPanicMessage, "the backend's pause")
	r.playsOn("c")
}

// TestPlaybackFaultsWhenItsSinksWriterPanics pins a panic on a sink's
// writer, through the backend: the sink ends typed (alsapcm.ReasonPanicked)
// with its device closed, the panic logged once, by the sink, and the
// reporter reports the session's fault as the backend's own, never the
// device's, so the engine stops playback and plays on.
func TestPlaybackFaultsWhenItsSinksWriterPanics(t *testing.T) {
	logs := &lines{}
	r := newRig(t, withLog(logs))
	r.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	ids := r.insert("a")
	r.play()
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.PanicAtNextClockRead(panicValue)
	// Room for the writer's next write, after which it measures, which reads
	// the clock.
	r.k.Grant(50 * time.Millisecond)
	f, _ := r.faulted()
	wantFailed(t, f, ids[0])
	r.letGo()
	logs.panicLogged(t, sinkPanicMessage, "the sink's writer")
	r.playsOn("b")
}

// TestReleaseAllLetsGoOfEverySessionTheBackendHolds pins ReleaseAll, which
// serve's player calls for an engine whose Run a panic ended: every session
// the backend holds is released as its Release would, so the device is let
// go, every file closed and every goroutine joined, and a second call has
// nothing to release. Its wait is bounded by its context: a sink the kernel
// holds keeps its teardown, and ReleaseAll says how many sessions still hold
// something; once the kernel lets go, the device is let go too.
func TestReleaseAllLetsGoOfEverySessionTheBackendHolds(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		d := newDirect(t, nil)
		d.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
		if err := d.start(1, "a"); err != nil {
			t.Fatalf("start: %v", err)
		}
		ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
		defer cancel()
		if err := d.be.ReleaseAll(ctx); err != nil {
			t.Fatalf("ReleaseAll: %v", err)
		}
		if v := d.k.View(); v.Open || v.Opens != 1 || v.Closes != 1 {
			t.Errorf("after ReleaseAll the node is open %v after %d opens and %d closes; want closed once", v.Open, v.Opens, v.Closes)
		}
		// Release acknowledges that resources are closed; spawn's deferred
		// counter decrement can still be pending. Join it before counting.
		if err := d.be.Wait(ctx); err != nil {
			t.Fatalf("after ReleaseAll: %v", err)
		}
		if st := d.be.Stats(); st.Sessions != 0 || st.Goroutines != 0 {
			t.Errorf("after ReleaseAll the backend holds %+v; want no session and no goroutine", st)
		}
		d.files.allClosed(t)
		if err := d.be.ReleaseAll(ctx); err != nil {
			t.Errorf("a second ReleaseAll, with nothing held: %v", err)
		}
		d.k.CheckClosedEach(t)
	})
	t.Run("bounded by its context", func(t *testing.T) {
		d := newDirect(t, nil)
		parked, release := d.k.ParkNextWrite()
		t.Cleanup(release)
		d.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
		if err := d.start(1, "a"); err != nil {
			t.Fatalf("start: %v", err)
		}
		receiveIn(t, parked, "the writer's write to stick in the kernel")
		ended, cancel := context.WithCancel(bg)
		cancel()
		if err := d.be.ReleaseAll(ended); err == nil || !strings.Contains(err.Error(), "1 session") {
			t.Errorf("ReleaseAll with the device held in the kernel: %v, want it to say one session still holds something", err)
		}
		release()
		ctx, done := context.WithTimeout(bg, alsapcm.Watchdog())
		defer done()
		if err := d.be.Wait(ctx); err != nil {
			t.Fatalf("once the kernel let go: %v", err)
		}
		if v := d.k.View(); v.Open {
			t.Error("the device is still open once the kernel let go")
		}
	})
}
