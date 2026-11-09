package alsapcm_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A write of the bound DAC's DSD choice (B728 B9; the design's section 7.2)
// through the engine's Run, the backend and the real sinks over the fake
// kernel: what a write would interrupt, what one that interrupts nothing
// leaves playing, and an occurrence that starts beside a write in flight.

// choiceReq is a write of mode over base for the DAC d's backend publishes,
// by the app.
func choiceReq(t *testing.T, d *dsdRig, mode string, base uint64, edit ...func(*playback.ChoiceRequest)) playback.ChoiceRequest {
	t.Helper()
	p := d.be.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		t.Fatalf("the backend publishes no DAC: %+v", p.DSD)
	}
	r := playback.ChoiceRequest{DACKey: p.DSD.DAC.Key, Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}}
	for _, e := range edit {
		e(&r)
	}
	return r
}

// wantOutcome fails t unless res's outcome is want.
func wantOutcome(t *testing.T, res playback.ChoiceResult, want playback.ChoiceOutcome) {
	t.Helper()
	if res.Outcome != want {
		t.Fatalf("the write's outcome is %s, want %s", res.Outcome, want)
	}
}

// TestOnlyAWriteThatInterruptsNeedsTheGoAhead (B728 B9; the design's sections
// 3.5 and 7.2, check 8): a write that changes only what nothing plays (a cap
// above the rate heard, the choice written again) lands without interrupt
// and every occurrence plays on, the successor in its sink joining the one
// heard; one that would revoke the occurrence heard is refused without it,
// would_interrupt, changing nothing, and with it lands and stops DSD at once.
func TestOnlyAWriteThatInterruptsNeedsTheGoAhead(t *testing.T) {
	// heard is a rig whose a and b, DSD64 natively, are in one sink, a heard.
	heard := func(t *testing.T) (*dsdRig, []uint32) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.files.add("a", dsd(playbacktest.DSD64, 0.5))
		d.files.add("b", dsd(playbacktest.DSD64, 0.5))
		ids := d.insert("a", "b")
		d.playedOut(dsdPrefill+100*time.Millisecond, "a", "b")
		d.tick()
		d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[0] })
		// What the open records, so no write of playback's lands while a test
		// reads the state.
		d.waitState("the open's driver_accepted", func(st dsdstate.State) bool {
			return playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64) != nil
		})
		return d, ids
	}
	// playsOn fails t unless a and b were both heard in one sink, nothing
	// re-planned, faulted, cut or skipped.
	playsOn := func(t *testing.T, d *dsdRig) {
		t.Helper()
		d.k.Free()
		s := d.waitEnded()
		evs := d.rec.events()
		if n := count(evs, "replan", 0) + count(evs, "fault", 0); n != 0 || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
			t.Errorf("a write that interrupts nothing re-planned, faulted or skipped: %s; faults %+v, skips %+v", kinds(evs), s.Faults(), s.Skips())
		}
		if n := count(evs, "audible", 0); n != 2 {
			t.Errorf("%d occurrences heard, want a and b: %s", n, kinds(evs))
		}
		if v := d.k.View(); v.Opens != 1 {
			t.Errorf("%d opens, want a and b in one sink", v.Opens)
		}
		for _, c := range d.rec.callList() {
			if c.kind == "cut" {
				t.Errorf("a write that interrupts nothing asked a cut: %+v", c)
			}
		}
	}
	t.Run("a cap above the rate heard", func(t *testing.T) {
		d, _ := heard(t)
		before := d.be.Policy()
		res := answer(t, d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, func(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = playbacktest.DSD128 })))
		wantOutcome(t, res, playback.ChoiceWritten)
		if res.Policy.Generation != before.Generation+1 {
			t.Errorf("the cap published generation %d after %d, want the next", res.Policy.Generation, before.Generation)
		}
		playsOn(t, d)
	})
	t.Run("the choice written again, while the successor plays", func(t *testing.T) {
		d, _ := heard(t)
		before := d.be.Policy()
		res := answer(t, d.choose(choiceReq(t, d, dsdstate.ModeNative, 1)))
		wantOutcome(t, res, playback.ChoiceWritten)
		if res.Policy.Generation != before.Generation || !reflect.DeepEqual(res.Policy.Tuples, before.Tuples) {
			t.Errorf("a write that leaves what plays as it was moved the set: generation %d after %d", res.Policy.Generation, before.Generation)
		}
		if got := res.Policy.DSD.DAC.Choice.Revision; got != 2 {
			t.Errorf("the answer's revision %d, want 2", got)
		}
		playsOn(t, d)
	})
	t.Run("a change of what is heard", func(t *testing.T) {
		d, _ := heard(t)
		state, writes, published := d.store.State(), d.store.Writes(), d.be.Policy()
		res := answer(t, d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1)))
		wantOutcome(t, res, playback.ChoiceWouldInterrupt)
		if d.store.Writes() != writes || !reflect.DeepEqual(d.store.State(), state) {
			t.Errorf("a write refused as would_interrupt changed the state: %+v", d.store.State())
		}
		if got := d.be.Policy(); got.Generation != published.Generation || len(got.Tuples) != len(published.Tuples) {
			t.Errorf("a write refused as would_interrupt moved the set: generation %d after %d", got.Generation, published.Generation)
		}
		if evs := d.rec.events(); count(evs, "fault", 0) != 0 {
			t.Fatalf("a refused write stopped what plays: %s", kinds(evs))
		}
		res = answer(t, d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1, func(r *playback.ChoiceRequest) { r.Interrupt = true })))
		wantOutcome(t, res, playback.ChoiceWritten)
		s := d.tickUntilStopped(engine.ReasonFault)
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the interrupting write's faults %+v, want native revoked", f)
		}
		if got := res.Policy.Converted; len(got) != 3 || len(res.Policy.Tuples) != 0 {
			t.Errorf("the answer publishes tuples %+v and converted %+v, want the three rates converted", res.Policy.Tuples, got)
		}
	})
}

// TestAnOccurrenceBesideAWriteIsNotRevoked (B728 B9; binding note 3): an
// occurrence that starts while a write is in flight is admitted under what
// the write leaves published, and never revoked for it: one starting once
// the state is durable and before the write publishes plays the new set,
// converted, without waiting for the write; one starting while a write that
// then fails is under way waits for it, outside its session's lock, and plays
// the set as it was; one starting while a write that lands and is not known
// to be durable is under way waits, and is then refused before anything
// plays as dsd_choice_uncertain, which the intersection of the two states
// publishes at its rate. The first is taken both ways round, each by
// barriers alone (round 13): heard while the DAC holds it, its write parked,
// then playing on as the write publishes; and played out, its session ended
// and the track it heard cleared, before the case looks, as CI's macOS run
// took it (run 42558744822), the case then reading only what stays. A
// case's write is parked in its seam (parkedWrite) and let go by its cleanup
// whatever the case did.
func TestAnOccurrenceBesideAWriteIsNotRevoked(t *testing.T) {
	// inTheWindow is a rig whose write of convert is parked once its state's
	// write is durable and before it publishes, the write's answer, and a
	// count of any occurrence's waits for a pending write.
	inTheWindow := func(t *testing.T) (*dsdRig, *parkedWrite, <-chan playback.ChoiceResult, *atomic.Int32) {
		t.Helper()
		p, awaited := newParkedWrite(), &atomic.Int32{}
		d := besideAWrite(t, &playback.Seams{
			ChoiceWritten: func(err error) {
				if err == nil {
					p.park()
				}
			},
			ChoiceAwaited: func(engine.OccurrenceID) { awaited.Add(1) },
		})
		t.Cleanup(p.letGo)
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		p.reached(t, res, "the window between its state's write and its publish")
		return d, p, res, awaited
	}
	// admittedInTheWindow fails t unless the occurrence was admitted under the
	// new set, converted in one PCM sink, without waiting for the write, which
	// is still parked: what it played, it played before the write published.
	admittedInTheWindow := func(t *testing.T, d *dsdRig, res <-chan playback.ChoiceResult, awaited *atomic.Int32) {
		t.Helper()
		if got := d.openFormats(); len(got) != 0 || d.pcmOpens() != 1 {
			t.Errorf("the occurrence opened DSD %v and %d PCM sinks, want it converted, in one PCM sink: the new set", got, d.pcmOpens())
		}
		if n := awaited.Load(); n != 0 {
			t.Errorf("the occurrence waited %d times for the parked write, want none: admitted in the window, it waits for nothing", n)
		}
		select {
		case r := <-res:
			t.Fatalf("the write was answered (%s) while it was parked", r.Outcome)
		default:
		}
	}
	t.Run("after the state's write and before the publish", func(t *testing.T) {
		d, p, res, awaited := inTheWindow(t)
		// The DAC plays only what the case grants: the occurrence is heard 50 ms
		// in and held there, so its session cannot end before the case looks.
		d.play()
		d.k.Await(t, "the occurrence's sink to fill the hardware buffer and wait on the DAC", runningAndWaiting)
		d.k.Play(t, 50*time.Millisecond)
		d.tick()
		s := d.waitSnap("a heard, its Details known", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.HasDetails })
		if tr, _ := s.Track(); tr.Details.Lossless || tr.Details.Codec != "DSD" {
			t.Errorf("the track heard has Details %+v, want it converted: DSD, not lossless", tr.Details)
		}
		admittedInTheWindow(t, d, res, awaited)
		// The write publishes while the occurrence plays, and revokes nothing.
		p.letGo()
		wantOutcome(t, answer(t, res), playback.ChoiceWritten)
		d.k.Free()
		s = d.waitStopped(engine.ReasonEndOfQueue)
		notRevoked(t, d, s)
		if got := d.openFormats(); len(got) != 0 || d.pcmOpens() != 1 {
			t.Errorf("the occurrence opened DSD %v and %d PCM sinks by its end, want its one PCM sink", got, d.pcmOpens())
		}
	})
	t.Run("after the state's write and before the publish, played out before the case looks", func(t *testing.T) {
		d, p, res, awaited := inTheWindow(t)
		// The ordering CI's macOS run took, made the case's: the occurrence
		// plays out and its session ends, the engine stopped and the track it
		// heard cleared at the release, before the case looks. The case reads
		// what stays: the tracks the engine heard, the backend's events and
		// Details, the sinks opened.
		d.k.Free()
		d.play()
		s := d.waitStopped(engine.ReasonEndOfQueue)
		if n := s.TrackCount(); n != 1 {
			t.Errorf("the engine heard %d tracks, want a", n)
		}
		o := d.firstOcc()
		if n := count(d.rec.events(), "audible", o); n != 1 {
			t.Errorf("the backend reported occurrence %d heard %d times, want once: %s", o, n, kinds(d.rec.events()))
		}
		if det, ok := d.detailsOf(o); !ok || det.Lossless || det.Codec != "DSD" {
			t.Errorf("the occurrence's Details %+v (reported: %v), want it converted: DSD, not lossless", det, ok)
		}
		notRevoked(t, d, s)
		admittedInTheWindow(t, d, res, awaited)
		p.letGo()
		wantOutcome(t, answer(t, res), playback.ChoiceWritten)
	})
	t.Run("while a write that fails is under way", func(t *testing.T) {
		p, waiting := newParkedWrite(), make(chan engine.OccurrenceID, 1)
		d := besideAWrite(t, &playback.Seams{
			ChoicePending: p.park,
			ChoiceAwaited: func(o engine.OccurrenceID) {
				select {
				case waiting <- o:
				default:
				}
			},
		})
		t.Cleanup(p.letGo)
		d.store.Fail(errors.New("no space left on device"))
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		p.reached(t, res, "its pending window")
		d.k.Free()
		go func() { _ = d.eng.Play(bg) }()
		awaitedBy(t, waiting, res)
		p.letGo()
		wantOutcome(t, answer(t, res), playback.ChoicePersistFailed)
		s := d.waitEnded()
		notRevoked(t, d, s)
		if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) || d.pcmOpens() != 0 {
			t.Errorf("the occurrence opened DSD %v and %d PCM sinks, want one native open: the set as it was", got, d.pcmOpens())
		}
		if n := count(d.rec.events(), "audible", 0); n != 1 {
			t.Errorf("%d occurrences heard, want a", n)
		}
	})
	t.Run("while a write that lands is under way", func(t *testing.T) {
		p, waiting := newParkedWrite(), make(chan engine.OccurrenceID, 1)
		d := besideAWrite(t, &playback.Seams{
			ChoicePending: p.park,
			ChoiceAwaited: func(o engine.OccurrenceID) {
				select {
				case waiting <- o:
				default:
				}
			},
		})
		t.Cleanup(p.letGo)
		d.store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		p.reached(t, res, "its pending window")
		go func() { _ = d.eng.Play(bg) }()
		awaitedBy(t, waiting, res)
		p.letGo()
		wantOutcome(t, answer(t, res), playback.ChoicePersistUncertain)
		s := d.waitEnded()
		notRevoked(t, d, s)
		skips := s.Skips()
		if len(skips) != 1 {
			t.Fatalf("skips %+v, want a refused", skips)
		}
		for _, sk := range skips {
			if sk.Reason != policy.ReasonChoiceUncertain {
				t.Errorf("a was refused as %s, want dsd_choice_uncertain", sk.Reason)
			}
		}
		if got := d.openFormats(); len(got) != 0 || d.pcmOpens() != 0 || d.k.View().Opens != 0 {
			t.Errorf("a refused occurrence opened DSD %v and %d PCM sinks", got, d.pcmOpens())
		}
	})
}

// TestACaseEndingWithItsWriteParkedLetsItGo (B728 B9 rounds 13 and 14): a
// case of TestAnOccurrenceBesideAWriteIsNotRevoked's that ends while its
// write is parked in its seam, as one that fails there ends, lets the write
// go in its cleanup at once: the case ends, its rig stops as every rig does,
// and the write is answered. CI's macOS run of that test's first case waited
// out the package's 30 minutes there (run 42558744822): its cleanup asked the
// seam's Once, which the parked write held until that cleanup let it go,
// after. The case runs on a goroutine of this test's, which follows it phase
// by phase, each within a bound of its own, W the watchdog (round 14: one
// bound of 2W from the case's start timed its setup and its body as the
// cleanup's):
//
//   - its body, its rig's setup and its write's park, until a mark its
//     cleanups run first: within the sum of the body's own waits (available,
//     insert's one ref and reached, W each; newDSDRig waits on nothing else)
//     and W of margin for its steps that wait on nothing, 4W. A case that ends
//     before the mark, its rig skipped or its body failed before its write
//     parked, keeps its skip or its failure;
//   - the property: the write's gate closed by letGo within W of the mark,
//     which nothing else closes. Past that this test lets the write go
//     itself, through park's escape, abort, which no Once guards;
//   - its shutdown: the rest of its cleanups, its rig's stop, which waits for
//     the engine's Run and the backend's goroutines, W each (newDSDRig and the
//     fake kernel register none), within 2W and W of margin, 3W. A case that
//     has not ended by then cannot be ended, since t.FailNow cannot unblock
//     its goroutine, so this test panics, naming the phase that holds it.
func TestACaseEndingWithItsWriteParkedLetsItGo(t *testing.T) {
	w := alsapcm.Watchdog()
	bodyBound, shutdownBound := 4*w, 3*w
	p := newParkedWrite()
	var res <-chan playback.ChoiceResult
	var passed bool
	marked, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		passed = t.Run("its write parked after the state's write", func(t *testing.T) {
			d := besideAWrite(t, &playback.Seams{ChoiceWritten: func(err error) {
				if err == nil {
					p.park()
				}
			}})
			t.Cleanup(p.letGo)
			// The body's end, registered after letGo so that it runs first.
			t.Cleanup(func() { close(marked) })
			res = d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
			p.reached(t, res, "the window between its state's write and its publish")
			// The case ends here, its write parked, as one that fails here does.
		})
	}()
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	aborted := false
	abort := func() {
		if !aborted {
			aborted = true
			close(p.abort)
		}
	}
	select {
	case <-marked:
	case <-done:
	case <-time.After(bodyBound):
		alsapcm.LogGoroutines(t)
		t.Errorf("the case's body, its rig's setup and its write's park, did not end within %v, its waits' bounds and one of margin", bodyBound)
		abort()
	}
	if closed(marked) {
		select {
		case <-p.out:
		case <-time.After(w):
			alsapcm.LogGoroutines(t)
			t.Errorf("the cleanup letting the parked write go did not within %v of the case's body's end: p.out is still open", w)
			abort()
		}
	}
	// Ended before its mark, the case keeps its result: its rig skipped, or
	// its body failed before its write parked.
	select {
	case <-done:
	case <-time.After(shutdownBound):
		alsapcm.LogGoroutines(t)
		stuck := "the rig's stop has not returned: the engine's Run or the backend's goroutines, after letGo closed p.out"
		switch {
		case !closed(marked):
			stuck = "the mark was never seen: the case's body has not ended"
		case !closed(p.out):
			stuck = "letGo has not returned: p.out is still open, and abort let the write go"
		}
		panic(fmt.Sprintf("the case did not end within %v of its write's release: %s; t.FailNow cannot end a case whose goroutine is stuck", shutdownBound, stuck))
	}
	if !passed {
		return
	}
	if res == nil {
		t.Skip("the case's rig did not start, so no write was made: its skip says why")
	}
	wantOutcome(t, answer(t, res), playback.ChoiceWritten)
}

// besideAWrite is TestAnOccurrenceBesideAWriteIsNotRevoked's rig: one over a
// native choice whose seams are seams, a DSD64 track "a" queued.
func besideAWrite(t *testing.T, seams *playback.Seams) *dsdRig {
	t.Helper()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = seams })
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.insert("a")
	d.be.RefreshDSD(true)
	return d
}

// notRevoked fails t if s stopped for a fault, a revocation among them.
func notRevoked(t *testing.T, d *dsdRig, s *engine.Snapshot) {
	t.Helper()
	if f := s.Faults(); len(f) != 0 || count(d.rec.events(), "fault", 0) != 0 {
		t.Errorf("an occurrence beside a write faulted: %+v", f)
	}
}

// parkedWrite is a choice's write parked in one of its seams
// (Seams.ChoiceWritten or ChoicePending) until the test lets it go: the
// seam's first call (park) closes in and waits until letGo. A case makes it
// before its rig, whose seam parks in it, and registers letGo as its cleanup
// once the rig is built, so that the write is let go before the rig's stop,
// whatever the case did. The write waits outside the Once that marks it
// parked, and letGo has a Once of its own, so letting it go asks nothing of
// the seam (B728 B9 round 13: the first case's cleanup asked the seam's Once
// first, which the parked write held until that cleanup let it go, after).
// abort is the write's escape that no Once guards, closed by
// TestACaseEndingWithItsWriteParkedLetsItGo alone when the cleanup it pins
// has not let the write go (round 14).
type parkedWrite struct {
	parked, freed  sync.Once
	in, out, abort chan struct{}
}

func newParkedWrite() *parkedWrite {
	return &parkedWrite{in: make(chan struct{}), out: make(chan struct{}), abort: make(chan struct{})}
}

// park is the seam's: its first call parks the write, until letGo or abort.
func (p *parkedWrite) park() {
	first := false
	p.parked.Do(func() {
		first = true
		close(p.in)
	})
	if first {
		select {
		case <-p.out:
		case <-p.abort:
		}
	}
}

// reached waits for the write to park, failing t where it is answered first
// or does not park within the watchdog.
func (p *parkedWrite) reached(t *testing.T, res <-chan playback.ChoiceResult, what string) {
	t.Helper()
	reached(t, p.in, res, what)
}

// letGo lets the parked write go on, and every later call of its seam.
func (p *parkedWrite) letGo() { p.freed.Do(func() { close(p.out) }) }

// reached waits for a write to come to the seam that closes at, failing t
// where it ends first or comes to none within the watchdog.
func reached(t *testing.T, at <-chan struct{}, res <-chan playback.ChoiceResult, what string) {
	t.Helper()
	select {
	case <-at:
	case r := <-res:
		t.Fatalf("the write ended (%s) without coming to %s", r.Outcome, what)
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the write never came to %s within %v", what, alsapcm.Watchdog())
	}
}

// awaitedBy waits until an occurrence waits for the pending write (waiting,
// its ChoiceAwaited seam's), failing t where the write is answered first or
// no occurrence waits for it within the watchdog.
func awaitedBy(t *testing.T, waiting <-chan engine.OccurrenceID, res <-chan playback.ChoiceResult) {
	t.Helper()
	select {
	case <-waiting:
	case r := <-res:
		t.Fatalf("the write ended (%s) before the occurrence beside it waited", r.Outcome)
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("an occurrence whose rate the pending write changes did not wait for it within %v", alsapcm.Watchdog())
	}
}
