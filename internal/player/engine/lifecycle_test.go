package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestACommandCancelledBeforeAdmissionDoesNotRun pins admission: a context
// that has ended before the owner takes the command, or ends as it takes
// it, leaves the queue as it was.
func TestACommandCancelledBeforeAdmissionDoesNotRun(t *testing.T) {
	h := newHarness(t)
	cancelAt := make(chan context.CancelFunc, 1)
	h.tweak = func(c *Config) {
		c.seams.admitted = func(cmd *command) {
			select {
			case cancel := <-cancelAt:
				cancel()
			default:
			}
		}
	}
	h.boot()
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_, err := h.eng.InsertAfter(ctx, 0, ref("a"), nil)
	h.refused("an insert with an ended context", err, ReasonCancelled)

	ctx, cancel = context.WithCancel(bg)
	cancelAt <- cancel
	_, err = h.eng.InsertAfter(ctx, 0, ref("b"), nil)
	h.refused("an insert whose context ended as the owner took it", err, ReasonCancelled)
	if sn := h.snap(); sn.Len() != 0 {
		t.Fatalf("a command that was not admitted ran: %v", sn.IDs())
	}
}

// TestAHungReservationWriteHoldsOnlyTheEditsThatNeedIt pins admission
// while a reservation write hangs: the insert that needs it waits, and an
// output report, a Pause and a Stop are answered at once; past
// ReserveTimeout the waiting insert is refused with ReasonReservation, and
// so is the next one at once, until the write lands.
func TestAHungReservationWriteHoldsOnlyTheEditsThatNeedIt(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	g, calls := h.hangReservations()
	h.available()
	h.must("play a", h.eng.PlayID(bg, ids[0]))
	s := h.be.nth(1)
	s.audible(s.first.ID)
	h.waitSnap("a audible", func(sn *Snapshot) bool { _, ok := sn.Audible(); return ok })

	mark := h.appliedMark()
	insert := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); return err })
	h.waitApplied("admitted insert", mark)
	h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
	h.outGen++
	report := OutputReport{Generation: h.outGen, Available: true}
	h.must("an output report", h.result("report", async(func() error { return h.eng.ReportOutput(bg, report) })))
	h.must("pause", h.result("pause", async(func() error { return h.eng.Pause(bg) })))
	if sn := h.snap(); sn.State() != Paused {
		t.Fatalf("after the pause: %s", sn.State())
	}
	h.must("stop", h.result("stop", async(func() error { return h.eng.Stop(bg) })))
	if sn := h.snap(); sn.State() != Stopped {
		t.Fatalf("after the stop: %s", sn.State())
	}
	select {
	case err := <-insert:
		t.Fatalf("the insert answered %v while its reservation write hangs", err)
	default:
	}

	h.clk.Advance(ReserveTimeout)
	h.refused("the insert past the bound", h.result("insert", insert), ReasonReservation)
	h.refused("an insert while the write still hangs", h.result("insert", async(func() error {
		_, err := h.eng.InsertAfter(bg, 0, ref("y"), nil)
		return err
	})), ReasonReservation)

	mark = h.appliedMark()
	g.open()
	h.waitApplied("handled reservation", mark)
	if _, err := h.eng.InsertAfter(bg, 0, ref("z"), nil); err != nil {
		t.Fatalf("an insert once the write landed: %v", err)
	}
	if got := h.snap().Len(); got != 3 {
		t.Fatalf("the queue holds %d entries, want a, b and z", got)
	}
}

// TestAWaitingCommandWaitsOnlyForItsOwnLane pins the parked commands'
// lanes: an insert parked on a hung reservation write, ahead of a Play
// parked on a Pause under way, does not hold the Play once the Pause has
// answered, and still waits itself.
func TestAWaitingCommandWaitsOnlyForItsOwnLane(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	reservation, calls := h.hangReservations()
	h.available()
	h.must("play a", h.eng.PlayID(bg, ids[0]))
	s := h.be.nth(1)
	s.audible(s.first.ID)
	h.waitSnap("a audible", func(sn *Snapshot) bool { _, ok := sn.Audible(); return ok })

	mark := h.appliedMark()
	insert := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); return err })
	h.waitApplied("admitted insert", mark)
	h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
	held := newGate()
	h.be.script(func() {
		h.be.onPause = func(ctx context.Context, _ *fakeSession) error { return held.wait(ctx, h.be, false) }
	})
	pause := async(func() error { return h.eng.Pause(bg) })
	h.be.waitFor("the pause", func() bool { return h.be.countLocked("pause") == 1 })
	mark = h.appliedMark()
	play := async(func() error { return h.eng.Play(bg) })
	h.waitApplied("admitted play", mark)
	held.open()
	h.must("pause", h.result("pause", pause))
	h.must("the play that waited for the pause", h.result("play", play))
	if sn := h.snap(); sn.State() != Playing {
		t.Fatalf("after the pause and the play: %s", sn.State())
	}
	select {
	case err := <-insert:
		t.Fatalf("the insert answered %v while its reservation write hangs", err)
	default:
	}
	reservation.open()
	h.must("the insert once its write landed", h.result("insert", insert))
}

// TestEnteringMaintenanceDoesNotWaitForAHungReservationWrite pins the
// maintenance commands' admission: they never wait behind a parked edit,
// and the parked edit is refused with ReasonMaintenance as the hold begins.
func TestEnteringMaintenanceDoesNotWaitForAHungReservationWrite(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	g, calls := h.hangReservations()
	mark := h.appliedMark()
	insert := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); return err })
	h.waitApplied("admitted insert", mark)
	h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
	holds := make(chan Hold, 1)
	enter := async(func() error {
		hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
		holds <- hold
		return err
	})
	h.must("enter", h.result("enter", enter))
	h.refused("the parked insert", h.result("insert", insert), ReasonMaintenance)
	hold := <-holds
	h.must("exit", h.result("exit", async(func() error { return h.eng.ExitMaintenance(bg, hold.Lease) })))
	g.open()
}

// TestAStopDuringASlowOpenIsTakenAtOnce pins Stop's admission behind
// transport commands that wait: a Pause waiting for a start still opening,
// and a Play behind it, do not hold the Stop; they are answered as it runs
// (the Pause with nil, playback being stopped as it asked; the Play
// overtaken, never having run), and the session is released while its
// Start still opens.
func TestAStopDuringASlowOpenIsTakenAtOnce(t *testing.T) {
	h := startHarness(t)
	h.insert("a", "b")
	h.available()
	held := newGate()
	h.be.script(func() {
		h.be.onStart = func(ctx context.Context, s *fakeSession) error {
			if err := held.wait(ctx, h.be, false); err != nil {
				return err
			}
			return s.open()
		}
	})
	play := async(func() error { return h.eng.Play(bg) })
	s := h.be.nth(1)
	mark := h.appliedMark()
	pause := async(func() error { return h.eng.Pause(bg) })
	h.waitApplied("admitted pause", mark)
	again := async(func() error { return h.eng.Play(bg) })
	h.waitApplied("admitted play", mark)
	mark = h.appliedMark()
	stop := async(func() error { return h.eng.Stop(bg) })
	h.waitApplied("stop", mark)
	h.must("the pause the stop overtook", h.result("pause", pause))
	h.refused("the play the stop overtook", h.result("play", again), ReasonOvertaken)
	h.be.waitFor("the release", func() bool { return s.releaseAsked })
	held.open()
	h.must("stop", h.result("stop", stop))
	h.refused("the play whose start the stop released", h.result("play", play), ReasonSuperseded)
	if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 || h.be.count("pause") != 0 {
		t.Fatalf("%s with %d sessions and %d pauses", sn.State(), h.be.starts(), h.be.count("pause"))
	}
}

// TestAStopOvertakesTheTransportCommandsWaitingWithoutRunningThem pins what
// a Stop answers the transport commands waiting behind a Pause under way:
// none of them ran, so each answers ReasonOvertaken and the selection is
// the one before them, a PlayID's, a PlayIndex's, a Next's and a
// Previous's included; the Pause answers nil.
func TestAStopOvertakesTheTransportCommandsWaitingWithoutRunningThem(t *testing.T) {
	h := startHarness(t)
	ids, _ := h.playing(1, "a", "b", "c", "d")
	held := newGate()
	h.be.script(func() {
		h.be.onPause = func(ctx context.Context, _ *fakeSession) error { return held.wait(ctx, h.be, false) }
	})
	pause := async(func() error { return h.eng.Pause(bg) })
	h.be.waitFor("the pause", func() bool { return h.be.countLocked("pause") == 1 })
	waiting := map[string]<-chan error{}
	for _, c := range []struct {
		op  string
		run func() error
	}{
		{"play", func() error { return h.eng.Play(bg) }},
		{"play id", func() error { return h.eng.PlayID(bg, ids[3]) }},
		{"play index", func() error { return h.eng.PlayIndex(bg, 0) }},
		{"next", func() error { return h.eng.Next(bg) }},
		{"previous", func() error { return h.eng.Previous(bg) }},
	} {
		mark := h.appliedMark()
		waiting[c.op] = async(c.run)
		h.waitApplied("admitted "+c.op, mark)
	}
	before := h.snap().Generation()
	h.must("stop", h.eng.Stop(bg))
	for op, done := range waiting {
		h.refused("the "+op+" the stop overtook", h.result(op, done), ReasonOvertaken)
	}
	h.must("the pause the stop overtook", h.result("pause", pause))
	if sn := h.snap(); sn.Selected() != ids[1] || sn.State() != Stopped {
		t.Fatalf("after the stop: %s at %d, want stopped at b (%d), the selection before the commands that did not run", sn.State(), sn.Selected(), ids[1])
	}
	if h.snap().Generation() == before {
		t.Fatal("the stop changed nothing visible")
	}
	held.open()
}

// TestAnOutputReportOvertakesTheTransportCommandsWaiting pins a report
// that stops playback while a Play waits behind a Resume under way: the
// report is taken at once, and the waiting Play is answered with the
// report's reason instead of running after it, which would start playback
// on the unit that replaced the one it was sent for.
func TestAnOutputReportOvertakesTheTransportCommandsWaiting(t *testing.T) {
	h := startHarness(t)
	h.playing(0, "a", "b")
	h.must("pause", h.eng.Pause(bg))
	held := newGate()
	h.be.script(func() {
		h.be.onResume = func(ctx context.Context, _ *fakeSession) (ResumeResult, error) {
			if err := held.wait(ctx, h.be, false); err != nil {
				return 0, err
			}
			return ResumeContinued, nil
		}
	})
	resume := async(func() error { return h.eng.Play(bg) })
	h.be.waitFor("the resume", func() bool { return h.be.countLocked("resume") == 1 })
	mark := h.appliedMark()
	again := async(func() error { return h.eng.Play(bg) })
	h.waitApplied("admitted play", mark)
	h.outGen++
	report := OutputReport{Generation: h.outGen, Available: true, UnitChanged: true}
	h.must("the unit change", h.result("report", async(func() error { return h.eng.ReportOutput(bg, report) })))
	h.refused("the play waiting behind the resume", h.result("play", again), ReasonUnitChanged)
	held.open()
	h.refused("the play whose resume was under way", h.result("the resume", resume), ReasonUnitChanged)
	if sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped }); h.be.starts() != 1 {
		t.Fatalf("%s with %d sessions: a play waiting before the unit changed started one after", sn.State(), h.be.starts())
	}
}

// TestAReplyComesAfterItsSnapshot pins read-your-writes: when a command's
// reply is sent, the snapshot published already holds what the command
// did, so a caller that reads as soon as its command returns reads its own
// change.
func TestAReplyComesAfterItsSnapshot(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var stale []string
	var selections []uint32
	h.tweak = func(c *Config) {
		c.seams.replying = func(op string, r reply, s *Snapshot) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case op == "insert" && r.err == nil:
				if _, ok := s.Read(r.id); !ok {
					stale = append(stale, fmt.Sprintf("insert of %d", r.id))
				}
			case op == "set repeat" && !s.Repeat():
				stale = append(stale, "repeat on")
			case op == "next":
				selections = append(selections, s.Selected())
			}
		}
	}
	h.boot()
	ids := h.insert("a", "b", "c")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.available()
	h.must("next", h.eng.Next(bg))
	h.must("next", h.eng.Next(bg))
	mu.Lock()
	defer mu.Unlock()
	if want := []uint32{ids[1], ids[2]}; !slices.Equal(selections, want) {
		stale = append(stale, fmt.Sprintf("next selected %v as it answered, want %v", selections, want))
	}
	if len(stale) > 0 {
		t.Fatalf("replies sent before their snapshot: %v", stale)
	}
}

// TestAfterRunReturnsEveryCommandIsClosed pins the closed engine: with a
// context that never ends, every command answers ReasonClosed at once.
func TestAfterRunReturnsEveryCommandIsClosed(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	h.shutdown()
	// Each waits under the watchdog: a command that blocked after Run would
	// otherwise hang the test rather than fail it.
	for name, f := range map[string]func() error{
		"play": func() error { return h.eng.Play(context.Background()) },
		"stop": func() error { return h.eng.Stop(context.Background()) },
		"output": func() error {
			return h.eng.ReportOutput(context.Background(), OutputReport{Generation: 9, Available: true})
		},
		"insert": func() error {
			_, err := h.eng.InsertAfter(context.Background(), 0, ref("b"), nil)
			return err
		},
		"enter": func() error {
			_, err := h.eng.EnterMaintenance(context.Background(), h.snap().Generation(), "after")
			return err
		},
	} {
		h.refused(name+" after Run", h.result(name, async(f)), ReasonClosed)
	}
	if !h.snap().Closed() {
		t.Fatal("the last snapshot is not closed")
	}
	if got := h.snap().IDs(); len(got) != 1 {
		t.Fatalf("reads after Run: %v", got)
	}
}

// TestShutdownJoinsWithinItsBound pins Run's shutdown mid-command with a
// backend that never answers: a Stop waiting on a release that never comes
// when Run is cancelled, every mutation refused from then on, Run back
// within ReleaseTimeout and shutdownGrace of the clock, the call that never
// answered abandoned and logged, and every other goroutine joined.
//
// Run joins a goroutine by its last message, so from the shutdown on each
// goroutine that ends (the queue file's writer, a reservation write, a call
// that answers) is held at its end (the exiting seam) until the count has
// been read: one still counted after the message Run joined it by is
// counted, whatever the scheduler does (B526: a goroutine's deferred count
// ran after Run returned, and CI read two goroutines).
//
// The session's Prepare of b answers at once, but its answer can still be on
// its way when the shutdown begins, and the harness's drive fires the
// shutdown's grace at once: a call still under way then is abandoned beside
// the release. So the test runs as the scheduler leaves that Prepare, and with
// its answer held until the fake is closed or the test lets it go, the latest
// a scheduler could leave it (B605).
func TestShutdownJoinsWithinItsBound(t *testing.T) {
	for _, heldPrepare := range []bool{false, true} {
		name := "as it comes"
		if heldPrepare {
			name = "the Prepare answer held"
		}
		t.Run(name, func(t *testing.T) {
			var holding atomic.Bool
			var held atomic.Int64
			ended, release := make(chan struct{}, 1), make(chan struct{})
			h := newHarness(t)
			h.tweak = func(c *Config) {
				c.seams.exiting = func() {
					if !holding.Load() {
						return
					}
					held.Add(1)
					select {
					case ended <- struct{}{}:
					default:
					}
					<-release
				}
			}
			h.boot()
			var letGo sync.Once
			t.Cleanup(func() { letGo.Do(func() { close(release) }) })
			answer := newGate()
			if heldPrepare {
				h.be.script(func() {
					h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, _ *Occurrence) error {
						_ = answer.wait(ctx, h.be, false)
						return nil
					}
				})
			}
			_, s := h.playing(0, "a", "b")
			h.be.script(func() {
				h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
					<-h.be.cleanup
					return nil
				}
			})
			stop := async(func() error { return h.eng.Stop(bg) })
			h.be.waitFor("the release", func() bool { return s.releaseAsked })
			holding.Store(true)
			start := h.clk.Now()
			h.cancel()
			h.waitSnap("closed", func(sn *Snapshot) bool { return sn.Closed() })
			_, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
			h.refused("an insert during shutdown", err, ReasonClosed)
			h.drive(h.ran)
			h.down = true
			if took := h.clk.Now().Sub(start); took > ReleaseTimeout+shutdownGrace {
				t.Fatalf("Run took %v of the clock to return, more than %v", took, ReleaseTimeout+shutdownGrace)
			}
			_ = h.result("the stop", stop)
			if n := h.eng.abandoned.Load(); n != 1 {
				t.Fatalf("%d calls abandoned, want the release that never answered", n)
			}
			if n := h.eng.live.Load(); n != 1 {
				t.Fatalf("%d goroutines still running after Run (%d held at their end), want only the abandoned call", n, held.Load())
			}
			if h.logs.count("abandoned") != 1 {
				t.Fatalf("the abandoned call was not logged once: %v", h.logs.all())
			}
			// The writer at least ends once the shutdown has begun, and the seam
			// holds it at its end until it is let go here.
			select {
			case <-ended:
			case <-time.After(*watchdog):
				t.Fatalf("waited %v for a goroutine to end after the shutdown began", *watchdog)
			}
			letGo.Do(func() { close(release) })
			h.be.close()
			h.joined(h.eng)
		})
	}
}

// TestSnapshotsAreImmutable pins the copies: changing what an accessor
// returns changes nothing, and a snapshot taken before a change still shows
// what it showed.
func TestSnapshotsAreImmutable(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	s.fault(Fault{Reason: ReasonXrun, Occurrence: s.first.ID})
	sn := h.waitSnap("the fault", func(sn *Snapshot) bool { return len(sn.Faults()) == 1 })
	token, before := sn.IDArray()
	sn.IDs()[0] = 99
	e, _ := sn.Read(ids[0])
	e.Meta[0] = 'X'
	es, _ := sn.ReadList(ids)
	es[1].Meta[0] = 'Y'
	sn.Faults()[0].Reason = "changed"
	sn.Skips()[ids[0]] = Skip{Reason: "changed"}
	h.insert("c")
	h.must("delete a", h.eng.Delete(bg, ids[0]))
	if tok, got := sn.IDArray(); tok != token || !slices.Equal(got, before) {
		t.Fatalf("an old snapshot changed: token %d IDs %v, was %d %v", tok, got, token, before)
	}
	if e, _ := sn.Read(ids[0]); string(e.Meta) != "meta:a" {
		t.Fatalf("an old snapshot's metadata is %q", e.Meta)
	}
	if es, _ := sn.ReadList(ids); string(es[1].Meta) != "meta:b" {
		t.Fatalf("a ReadList copy reached the queue: %q", es[1].Meta)
	}
	if sn.Faults()[0].Reason != ReasonXrun || len(sn.Skips()) != 0 {
		t.Fatalf("faults %v skips %v", sn.Faults(), sn.Skips())
	}
	if e, _ := h.snap().Read(ids[1]); string(e.Meta) != "meta:b" {
		t.Fatalf("the live metadata is %q", e.Meta)
	}
}
