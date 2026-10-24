package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// panicValue is what the tests' panics carry: a value a panic could carry in
// the bridge, a library path, which no line of the log may show.
const panicValue = "a value the panic carries: /srv/music/Secret/Track.flac"

// panicked requires one recovered panic in the log, naming where, with its
// stack, and the panic's value in no line of the log.
func (h *harness) panicked(where string) string {
	h.t.Helper()
	var lines []string
	for _, l := range h.logs.all() {
		if strings.Contains(l, panicMessage) {
			lines = append(lines, l)
		}
		if strings.Contains(l, panicValue) {
			h.t.Errorf("a line of the log carries the panic's value: %s", l)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "where="+where) || !strings.Contains(lines[0], "stack=") {
		h.t.Fatalf("the panic logged %q, want one line naming %q, with its stack", lines, where)
	}
	return lines[0]
}

// TestAPanicInABackendCallIsThatCallsFailure pins a panic on a backend
// call's goroutine: recovered there and logged once, and the call's failure,
// typed ReasonBackend, which the owner takes through the path an error the
// call returned takes: a Start's stops playback and releases the session, a
// Prepare's leaves what follows to a session of its own, a Cut's stops
// playback when its occurrence is heard, a Pause's and a Resume's answer
// their command, a Release's leaves the release unconfirmed until it is
// acknowledged, and a Discard's changes nothing. Each time the engine plays
// on.
func TestAPanicInABackendCallIsThatCallsFailure(t *testing.T) {
	panics := func() { panic(panicValue) }
	t.Run("start", func(t *testing.T) {
		h := startHarness(t)
		h.be.script(func() {
			h.be.onStart = func(context.Context, *fakeSession) error { panics(); return nil }
			// The panic leaves the fake's Start unfinished, so its release
			// acknowledges at once rather than wait for it.
			h.be.onRelease = func(_ context.Context, s *fakeSession) error { s.ack(); return nil }
		})
		h.insert("a")
		h.available()
		if err := h.eng.Play(bg); ReasonOf(err) != ReasonBackend {
			t.Fatalf("the play whose start panicked answered %v, want %s", err, ReasonBackend)
		}
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped && sn.LastStop().Reason != "" })
		if sn.LastStop().Reason != ReasonBackend || strings.Contains(sn.LastStop().Detail, panicValue) {
			t.Fatalf("last stop %+v, want %s naming no value", sn.LastStop(), ReasonBackend)
		}
		if n := h.be.count("release"); n != 1 {
			t.Fatalf("%d releases of the session whose start panicked, want 1", n)
		}
		h.panicked("the backend's start call")
		h.be.script(func() { h.be.onStart, h.be.onRelease = nil, nil })
		h.must("a play once the backend starts", h.eng.Play(bg))
		h.be.nth(2)
	})
	t.Run("prepare", func(t *testing.T) {
		h := startHarness(t)
		h.be.script(func() {
			h.be.onPrepare = func(context.Context, *fakeSession, OccurrenceID, *Occurrence) error { panics(); return nil }
		})
		ids, s := h.playing(0, "a", "b")
		h.waitApplied("handled call prepare", 0)
		h.be.script(func() { h.be.onPrepare = nil })
		if n := h.logs.count("the backend took no successor"); n != 1 {
			t.Fatalf("the panicking prepare logged %d times as a successor not taken, want once", n)
		}
		s.finished()
		if s2 := h.be.nth(2); s2.first.Entry != ids[1] {
			t.Fatalf("after the session played out the entry that follows started as %+v, want b", s2.first)
		}
		h.panicked("the backend's prepare call")
	})
	t.Run("cut", func(t *testing.T) {
		h := startHarness(t)
		ids, s, _, b := handoff(t, h, true, "a", "b", "c")
		h.be.script(func() {
			h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { panics(); return true, nil }
		})
		mark := h.appliedMark()
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		h.waitApplied("handled call cut", mark)
		s.audible(b.ID)
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if sn.LastStop().Reason != ReasonRemovedAudible {
			t.Fatalf("last stop %+v, want %s: a cut that panicked did not cut", sn.LastStop(), ReasonRemovedAudible)
		}
		h.panicked("the backend's cut call")
	})
	t.Run("pause", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a")
		h.be.script(func() { h.be.onPause = func(context.Context, *fakeSession) error { panics(); return nil } })
		if err := h.eng.Pause(bg); ReasonOf(err) != ReasonBackend || strings.Contains(err.Error(), panicValue) {
			t.Fatalf("the pause that panicked answered %v, want %s naming no value", err, ReasonBackend)
		}
		if st := h.snap().State(); st != Playing {
			t.Fatalf("after the pause that panicked the transport is %s, want playing", st)
		}
		h.panicked("the backend's pause call")
		h.be.script(func() { h.be.onPause = nil })
		h.must("a pause the backend takes", h.eng.Pause(bg))
	})
	t.Run("resume", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a")
		h.must("pause", h.eng.Pause(bg))
		h.be.script(func() {
			h.be.onResume = func(context.Context, *fakeSession) (ResumeResult, error) { panics(); return ResumeContinued, nil }
		})
		if err := h.eng.Play(bg); ReasonOf(err) != ReasonBackend {
			t.Fatalf("the resume that panicked answered %v, want %s", err, ReasonBackend)
		}
		sn := h.snap()
		if _, why := sn.LastResume(); sn.State() != Paused || why == "" || strings.Contains(why, panicValue) {
			t.Fatalf("after the resume that panicked %s, reason %q; want paused with a reason naming no value", sn.State(), why)
		}
		h.panicked("the backend's resume call")
		h.be.script(func() { h.be.onResume = nil })
		h.must("a resume the backend takes", h.eng.Play(bg))
	})
	t.Run("release", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.playing(0, "a")
		h.be.script(func() { h.be.onRelease = func(context.Context, *fakeSession) error { panics(); return nil } })
		stop := async(func() error { return h.eng.Stop(bg) })
		h.be.waitFor("the release", func() bool { return s.releaseAsked })
		h.waitApplied("handled call release", 0)
		h.clk.Advance(ReleaseTimeout)
		h.must("stop", h.result("stop", stop))
		if _, ok := h.snap().ReleaseUnconfirmed(); !ok {
			t.Fatal("a release that panicked was taken for confirmed")
		}
		h.panicked("the backend's release call")
		s.ack()
		s.ev.Released(s.id)
		h.waitSnap("the late acknowledgement", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return !ok })
		h.be.script(func() { h.be.onRelease = nil })
		h.must("a play once the release is acknowledged", h.eng.Play(bg))
		h.be.nth(2)
	})
	t.Run("discard", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a")
		h.available()
		h.be.script(func() { h.be.onDiscard = func(context.Context) error { panics(); return nil } })
		mark := h.appliedMark()
		h.report(OutputReport{Available: true, UnitChanged: true})
		h.waitApplied("handled call discard", mark)
		h.panicked("the backend's discard call")
		h.be.script(func() { h.be.onDiscard = nil })
		h.must("a play once the output is back", h.eng.Play(bg))
		h.be.nth(1)
	})
	t.Run("the runtime's own error is logged", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a")
		h.be.script(func() {
			h.be.onPause = func(context.Context, *fakeSession) error {
				var m map[string]int
				m["a"]++
				return nil
			}
		})
		if err := h.eng.Pause(bg); ReasonOf(err) != ReasonBackend {
			t.Fatalf("the pause that panicked answered %v, want %s", err, ReasonBackend)
		}
		if line := h.panicked("the backend's pause call"); !strings.Contains(line, "error=assignment to entry in nil map") {
			t.Fatalf("the runtime's own error is not in the line: %s", line)
		}
	})
}

// TestAPanicInAReservationWriteIsAFailedWrite pins a panic in a reservation
// write, the start's or one on a goroutine of its own: recovered and logged
// once, and a write that failed, so nothing it would cover is handed out:
// the edits that need it are refused with ReasonReservation, as after any
// failed write, until a write lands.
func TestAPanicInAReservationWriteIsAFailedWrite(t *testing.T) {
	t.Run("an edit's", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a", "b")
		// A start whose reservation failed makes the next edit write one of
		// its own, which panics.
		h.restartWith(func() { h.files.failReserve = errors.New("read-only file system") })
		h.files.mu.Lock()
		h.files.failReserve, h.files.panicReserve = nil, panicValue
		h.files.mu.Unlock()
		ids, tokens := h.files.durable()
		_, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
		h.refused("an insert whose reservation write panicked", err, ReasonReservation)
		if strings.Contains(err.Error(), panicValue) {
			t.Fatalf("the refusal names the panic's value: %v", err)
		}
		if ed := h.snap().Edits(); ed.Refused != ReasonReservation {
			t.Fatalf("edits %+v after the write that panicked, want refused %s", ed, ReasonReservation)
		}
		if i, k := h.files.durable(); i != ids || k != tokens {
			t.Fatalf("the write that panicked reserved %d and %d, past %d and %d", i, k, ids, tokens)
		}
		h.panicked("a reservation write")
		if _, err := h.eng.InsertAfter(bg, 0, ref("y"), nil); err != nil {
			t.Fatalf("an insert once a write lands: %v", err)
		}
		if ed := h.snap().Edits(); ed.Refused != "" {
			t.Fatalf("edits %+v once the reservation landed", ed)
		}
	})
	t.Run("the start's", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a")
		h.restartWith(func() { h.files.panicReserve = panicValue })
		if ed := h.snap().Edits(); ed.Refused != ReasonReservation {
			t.Fatalf("edits %+v after the start's write panicked, want refused %s", ed, ReasonReservation)
		}
		if tok := h.snap().Token(); tok != 0 {
			t.Fatalf("a start whose reservation panicked published token %d, want none", tok)
		}
		h.panicked("a reservation write")
		if _, err := h.eng.InsertAfter(bg, 0, ref("y"), nil); err != nil {
			t.Fatalf("an insert once a write lands: %v", err)
		}
	})
}

// TestAPanicInAQueueWriteIsAFailedWriteThatStaysDirty pins a panic in a
// write of the queue file: recovered on the writer's goroutine and logged
// once, and a write that failed: the state stays dirty, the write is
// retried after its backoff, and the newest state lands once the disk takes
// it.
func TestAPanicInAQueueWriteIsAFailedWriteThatStaysDirty(t *testing.T) {
	h := startHarness(t)
	h.files.mu.Lock()
	h.files.failQueue = func(n int) error {
		if n == 1 {
			panic(panicValue)
		}
		return nil
	}
	h.files.mu.Unlock()
	h.insert("a")
	h.clk.Advance(writeCoalesce)
	sn := h.waitSnap("the write that panicked", func(sn *Snapshot) bool { return sn.Persist().Failures == 1 })
	if p := sn.Persist(); p.Written >= p.Revision || p.Err == "" || strings.Contains(p.Err, panicValue) {
		t.Fatalf("after the write that panicked %+v, want dirty with a failure naming no value", p)
	}
	h.insert("b")
	h.clk.Advance(writeRetryMin)
	h.waitSnap("written", func(sn *Snapshot) bool { p := sn.Persist(); return p.Written == p.Revision && p.Err == "" })
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	if f := readQueueFile(t, data); len(f.Entries) != 2 {
		t.Fatalf("the file holds %d entries, want the newest state's 2", len(f.Entries))
	}
	h.panicked("the queue file's writer")
}

// TestAPanicInTheQueueWritersLoopEndsTheWriter pins a panic on the queue
// file writer's goroutine outside a write: recovered and logged once, and the
// writer's end, said as a failed write: the state stays dirty from then on,
// the engine plays on, and its shutdown waits for no flush.
func TestAPanicInTheQueueWritersLoopEndsTheWriter(t *testing.T) {
	h := newHarness(t)
	var once atomic.Bool
	h.tweak = func(cfg *Config) {
		cfg.seams.writerWaits = func(uint64) {
			if once.CompareAndSwap(false, true) {
				panic(panicValue)
			}
		}
	}
	h.boot()
	h.waitSnap("the writer's end", func(sn *Snapshot) bool { return sn.Persist().Err != "" })
	h.insert("a")
	h.clk.Advance(writeMaxAge)
	if p := h.snap().Persist(); p.Written >= p.Revision || strings.Contains(p.Err, panicValue) {
		t.Fatalf("after the writer ended %+v, want dirty with a failure naming no value", p)
	}
	if n := h.files.attempts(); n != 0 {
		t.Fatalf("%d queue writes after the writer ended, want none", n)
	}
	h.panicked("the queue file's writer")
	h.available()
	h.must("play", h.eng.Play(bg))
	h.shutdown()
}

// TestAPanicOnTheOwnerLeavesNothingWaitingOnIt pins a panic on the owner,
// Run's own goroutine, which goes on to Run's caller (serve's player
// recovers it and abandons the engine): on its way out the engine ends every
// backend call, its queue file writer writes the newest revision and exits,
// and Done closes, so a command answers closed at once, a backend's events,
// however many, return at once rather than wait on an owner that is gone,
// and every goroutine the engine started ends.
func TestAPanicOnTheOwnerLeavesNothingWaitingOnIt(t *testing.T) {
	h := newHarness(t)
	var armed atomic.Bool
	h.tweak = func(cfg *Config) {
		applied := cfg.seams.applied
		cfg.seams.applied = func(op string) {
			applied(op)
			if op == "set repeat" && armed.Load() {
				panic(panicValue)
			}
		}
	}
	h.build()
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	ran := make(chan any, 1)
	go func() {
		defer func() { ran <- recover() }()
		_ = h.eng.Run(ctx)
	}()
	held := newGate()
	var heldEnded atomic.Bool
	h.be.script(func() {
		h.be.onStart = func(ctx context.Context, s *fakeSession) error {
			err := held.wait(ctx, h.be, true)
			heldEnded.Store(errors.Is(err, context.Canceled))
			return err
		}
	})
	h.insert("a")
	h.available()
	play := async(func() error { return h.eng.Play(bg) })
	s := h.be.nth(1)
	armed.Store(true)
	h.must("repeat, whose step the owner panics after", h.eng.SetRepeat(bg, true))
	select {
	case r := <-ran:
		if r != panicValue {
			t.Fatalf("Run's caller recovered %v, want the owner's panic", r)
		}
	case <-time.After(*watchdog):
		t.Fatalf("the owner's panic did not reach Run's caller within %v", *watchdog)
	}
	select {
	case <-h.eng.Done():
	case <-time.After(*watchdog):
		t.Fatalf("Done did not close within %v of the owner's panic", *watchdog)
	}
	if err := h.result("the play waiting on the owner", play); ReasonOf(err) != ReasonClosed {
		t.Fatalf("the play waiting on the owner answered %v, want %s", err, ReasonClosed)
	}
	h.refused("a command after the owner's panic", h.eng.Stop(bg), ReasonClosed)
	posted := async(func() error {
		for range 3 * cap(h.eng.msgs) {
			s.position(s.first.ID, 4096)
		}
		return nil
	})
	h.must("events past the owner's buffer", h.result("events past the owner's buffer", posted))
	h.joined(h.eng)
	if !heldEnded.Load() {
		t.Error("the start the backend held did not see its context end")
	}
	data, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	if f := readQueueFile(t, data); !f.Repeat {
		t.Error("the queue file does not hold the last state the owner published (repeat on)")
	}
}
