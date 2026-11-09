package alsapcm_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
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
		res := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeNative, 1, func(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = playbacktest.DSD128 }))
		wantOutcome(t, res, playback.ChoiceWritten)
		if res.Policy.Generation != before.Generation+1 {
			t.Errorf("the cap published generation %d after %d, want the next", res.Policy.Generation, before.Generation)
		}
		playsOn(t, d)
	})
	t.Run("the choice written again, while the successor plays", func(t *testing.T) {
		d, _ := heard(t)
		before := d.be.Policy()
		res := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeNative, 1))
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
		res := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeConvert, 1))
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
		res = d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeConvert, 1, func(r *playback.ChoiceRequest) { r.Interrupt = true }))
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
// the state is durable and before the write publishes plays the new set; one
// starting while a write that then fails is under way waits for it, outside
// its session's lock, and plays the set as it was; one starting while a write
// that lands and is not known to be durable is under way waits, and is then
// refused before anything plays as dsd_choice_uncertain, which the
// intersection of the two states publishes at its rate.
func TestAnOccurrenceBesideAWriteIsNotRevoked(t *testing.T) {
	t.Run("after the state's write and before the publish", func(t *testing.T) {
		p := newParkedWrite()
		d := besideAWrite(t, &playback.Seams{ChoiceWritten: func(err error) {
			if err == nil {
				p.park()
			}
		}})
		t.Cleanup(p.cleanup(t))
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		p.reached(t, res, "the window between its state's write and its publish")
		d.k.Free()
		d.play()
		s := d.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
		p.letGo()
		wantOutcome(t, answer(t, res), playback.ChoiceWritten)
		s = d.waitEnded()
		notRevoked(t, d, s)
		if got := d.openFormats(); len(got) != 0 || d.pcmOpens() != 1 {
			t.Errorf("the occurrence opened DSD %v and %d PCM sinks, want it converted, in one PCM sink: the new set", got, d.pcmOpens())
		}
	})
	t.Run("while a write that fails is under way", func(t *testing.T) {
		pending, release, waiting := make(chan struct{}), make(chan struct{}), make(chan engine.OccurrenceID, 1)
		var once sync.Once
		d := besideAWrite(t, &playback.Seams{
			ChoicePending: func() { once.Do(func() { close(pending); <-release }) },
			ChoiceAwaited: func(o engine.OccurrenceID) {
				select {
				case waiting <- o:
				default:
				}
			},
		})
		t.Cleanup(func() { closeOnce(release) })
		d.store.Fail(errors.New("no space left on device"))
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		reached(t, pending, res, "its pending window")
		d.k.Free()
		go func() { _ = d.eng.Play(bg) }()
		awaitedBy(t, waiting, res)
		closeOnce(release)
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
		pending, release, waiting := make(chan struct{}), make(chan struct{}), make(chan engine.OccurrenceID, 1)
		var once sync.Once
		d := besideAWrite(t, &playback.Seams{
			ChoicePending: func() { once.Do(func() { close(pending); <-release }) },
			ChoiceAwaited: func(o engine.OccurrenceID) {
				select {
				case waiting <- o:
				default:
				}
			},
		})
		t.Cleanup(func() { closeOnce(release) })
		d.store.Land(fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded))
		res := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
		reached(t, pending, res, "its pending window")
		go func() { _ = d.eng.Play(bg) }()
		awaitedBy(t, waiting, res)
		closeOnce(release)
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
// before its rig, whose seam parks in it, and registers its cleanup once the
// rig is built, so that the cleanup runs before the rig's stop.
type parkedWrite struct {
	once    sync.Once
	in, out chan struct{}
}

func newParkedWrite() *parkedWrite {
	return &parkedWrite{in: make(chan struct{}), out: make(chan struct{})}
}

// park is the seam's: its first call parks the write.
func (p *parkedWrite) park() { p.once.Do(func() { close(p.in); <-p.out }) }

// reached waits for the write to park, failing t where it is answered first
// or does not park within the watchdog.
func (p *parkedWrite) reached(t *testing.T, res <-chan playback.ChoiceResult, what string) {
	t.Helper()
	reached(t, p.in, res, what)
}

// letGo lets the parked write go on.
func (p *parkedWrite) letGo() { closeOnce(p.out) }

// cleanup is the case's cleanup of its write: the seam's Once asked first,
// within the watchdog, then the write let go.
func (p *parkedWrite) cleanup(t *testing.T) func() {
	return func() {
		asked := make(chan struct{})
		go func() {
			p.once.Do(func() {})
			close(asked)
		}()
		select {
		case <-asked:
		case <-time.After(alsapcm.Watchdog()):
			alsapcm.LogGoroutines(t)
			t.Errorf("the case's cleanup waited %v for its write's seam, whose Once the parked write holds until it is let go", alsapcm.Watchdog())
		}
		p.letGo()
	}
}

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

// closeOnce closes c unless it is closed.
func closeOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}
