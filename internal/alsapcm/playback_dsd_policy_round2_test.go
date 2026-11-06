package alsapcm_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// Round 2 of B728 B7's review, through the engine's Run, the backend and the
// real native and DoP sinks over the fake kernel: a played-out occurrence
// reconciliation leaves alone, a session's first occurrence admitted again
// once its open ends, the silent probe's answers bound to the unit it
// captured, and the reporter and reconciliation under one lock.

// awaitHeld waits for held to close, under the watchdog.
func awaitHeld(t *testing.T, what string, held <-chan struct{}) {
	t.Helper()
	select {
	case <-held:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
	}
}

// publishedRates are the rates p publishes a tuple at, and each one's
// transport ("" native).
func publishedRates(p playback.PlayerPolicy) (rates []int, transports []string) {
	for _, tu := range p.Tuples {
		rates = append(rates, tu.Tuple.BitRate)
		transports = append(transports, tu.Tuple.Transport)
	}
	return rates, transports
}

// TestAPlayedOutDoPOccurrenceStopsNothingAfterIt (B728 B7 round 2): a DoP
// occurrence whose sink has played everything it was given and closed, the
// session gone on to a native successor in a sink of its own, sounds no more,
// so no change of the set can make it unsafe: the volume leaving 0 dB while
// the successor's node is checked takes DoP out of the set, and reconciliation
// judges the successor alone, which the set still plays. It is heard, and
// nothing faults or is skipped. A DoP occurrence still sounding stops at once
// (TestAGainChangeRevokesDoPWithoutConsent).
func TestAPlayedOutDoPOccurrenceStopsNothingAfterIt(t *testing.T) {
	// auto, as the migration leaves an owner's record that approved DoP at
	// DSD64 and native DSD at DSD128: DSD64 plays DoP, DSD128 native.
	d := newDSDRig(t, playbacktest.MigratedState(dsdstate.ModeAuto, true, []int{playbacktest.DSD128}, []int{playbacktest.DSD64}))
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	d.box.SetUnit(card, unit)
	d.box.SetMixer(card, playbacktest.VolumeMixer())
	d.box.SetControls(card, playbacktest.VolumeControls(255)...)
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.insert("a", "b")
	d.play()
	waitOn(t, "a's DoP sink open", d.rec.ch, func() bool { return d.rec.openedSinks() == 1 })
	// a's node passed its check after open: the next one held is b's, once a
	// has played out and its sink is closed.
	held, release := d.unit.holdNextAdmit()
	t.Cleanup(release)
	d.k.Free()
	awaitHeld(t, "the check after open of b's node", held)
	d.box.SetControls(card, playbacktest.VolumeControls(200)...)
	d.monitorTick()
	if rates, transports := publishedRates(d.be.Policy()); !slices.Equal(rates, []int{playbacktest.DSD128}) || transports[0] != "" {
		t.Fatalf("the set once the volume left 0 dB publishes %v (%q), want native at DSD128 alone", rates, transports)
	}
	release()
	s := d.waitEnded()
	d.rig.stop()
	evs := d.rec.events()
	if n := count(evs, "audible", 0); n != 2 || count(evs, "fault", 0) != 0 || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
		t.Errorf("%d occurrences heard, want a and b, with no fault or skip: %s; faults %+v, skips %+v", n, kinds(evs), s.Faults(), s.Skips())
	}
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.S32LE, format.DSDU32BE}) {
		t.Errorf("the opens asked %v, want a's DoP S32_LE then b's native DSD_U32_BE", got)
	}
}

// TestAChangeWhileTheFirstOccurrenceReadsItsFirstChunk (B728 B7 round 2): a
// session's first DSD occurrence is admitted as its open begins, and a refresh
// can publish the next set while the open reads its first chunk, where no
// reconciliation reaches it: it is admitted again under the set published once
// its open ends, as a successor is, before anything is located or opened. A
// change that keeps its plan (a cap above its rate) plays it; one that takes
// the plan away (DSD off by choice) refuses it with the new set's word, the
// DAC's node never opened.
func TestAChangeWhileTheFirstOccurrenceReadsItsFirstChunk(t *testing.T) {
	capped := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped.DACs[0].Choice.MaxBitRate, capped.DACs[0].Choice.Revision = playbacktest.DSD128, 2
	for _, c := range []struct {
		name    string
		next    dsdstate.State
		refused engine.Reason
	}{
		{"a change that keeps its plan", capped, ""},
		{"a change that takes its plan away", playbacktest.ChoiceState(dsdstate.ModeOff), "dsd_off_by_choice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
			d.files.add("a", dsd(playbacktest.DSD64, 0.25))
			// The DSF header ends at byte 92: a read past it is the first
			// chunk's, which the open makes once the occurrence is admitted.
			parked, unstall := d.files.stall("a", 92)
			t.Cleanup(unstall)
			ids := d.insert("a")
			go func() { _ = d.eng.Play(bg) }()
			awaitHeld(t, "the first chunk's read", parked)
			admitted := d.be.Policy().Generation
			d.store.Set(c.next)
			d.be.RefreshDSD(true)
			if g := d.be.Policy().Generation; g != admitted+1 {
				t.Fatalf("the change published generation %d after %d, want the next", g, admitted)
			}
			d.k.Free()
			unstall()
			s := d.waitEnded()
			d.rig.stop()
			if c.refused == "" {
				evs := d.rec.events()
				if a := s.LastAttempt(); a.Failed || len(s.Faults()) != 0 || len(s.Skips()) != 0 || count(evs, "audible", 0) != 1 {
					t.Errorf("the plan the change kept did not play: attempt %+v, faults %+v, skips %+v, events %s", a, s.Faults(), s.Skips(), kinds(evs))
				}
				if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
					t.Errorf("the opens asked %v, want one DSD_U32_BE", got)
				}
				return
			}
			if sk, ok := s.Skipped(ids[0]); !ok || sk.Reason != c.refused {
				t.Errorf("the entry skipped %+v (%v), want %s", sk, ok, c.refused)
			}
			if got := d.openFormats(); len(got) != 0 {
				t.Errorf("the opens asked %v: the set published once the open ended refuses the plan, so no node is opened", got)
			}
			if v := d.k.View(); v.GivenBytes != 0 {
				t.Errorf("the DAC was given %d bytes", v.GivenBytes)
			}
		})
	}
}

// TestAProbeRecordsNothingOfAUnitSwappedInBeforeItsOpen (B728 B7 round 2): the
// silent probe records what the DAC accepted under the unit it captured (its
// model, its firmware and its enumeration), so the node it opens must be that
// unit: another one swapped into the bound place between the capture and the
// open passes the binding's own check, which names a place, and is asked
// nothing, and neither the probe's cache nor the player's DSD state holds an
// answer the captured unit never gave.
func TestAProbeRecordsNothingOfAUnitSwappedInBeforeItsOpen(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	refines := d.k.View().Refines
	held, release := d.unit.holdNextAdmit()
	t.Cleanup(release)
	done := make(chan bool, 1)
	go func() { done <- d.be.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}) }()
	awaitHeld(t, "the probe's check after open", held)
	other := playbacktest.DACUnit()
	other.USB.ID.Product, other.Product = 0x5679, "Other DAC"
	d.box.SetUnit(card, other)
	release()
	select {
	case <-done:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the probe did not answer within %v", alsapcm.Watchdog())
	}
	d.rig.stop()
	if v := d.k.View(); v.Refines != refines {
		t.Errorf("the probe asked the unit swapped in %d candidates", v.Refines-refines)
	}
	if e := playbacktest.Evidence(d.store.State(), dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64); e != nil {
		t.Errorf("the captured unit's model holds %+v, an answer the unit swapped in gave", e)
	}
	captured := alsapcm.ProbeUnit{Model: playbacktest.DACKey().Canonical(), Firmware: playbacktest.DACBCD, Kernel: playbacktest.Kernel, Enumeration: "test/2"}
	for _, cand := range alsapcm.ProbeCandidates() {
		if r := d.pol.Probes.Answer(captured, cand); r.Answer != alsapcm.ProbeNotProbed {
			t.Errorf("the probe's cache holds %+v for the captured unit", r)
		}
	}
}

// TestReconciliationAndTheReporterTakeOneLock (B728 B7 round 2; Gemini on
// reportStep): the reporter reads a heard occurrence's set and its observed
// mark under the session's lock, which reportStep holds from its hold to the
// sends after it, and reconciliation repoints the set under that lock: run at
// once, a monitor keeping the occurrence through a change of the set at every
// tick and the reporter measuring at every tick, -race finds no access of
// either outside it.
func TestReconciliationAndTheReporterTakeOneLock(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	d.files.add("a", dsd(playbacktest.DSD64, 2))
	ids := d.insert("a")
	d.playedOut(dsdPrefill+100*time.Millisecond, "a")
	d.tick()
	d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[0] })
	before := d.be.Policy().Generation
	capped := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped.DACs[0].Choice.MaxBitRate = playbacktest.DSD128
	const rounds = 100
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range rounds {
			st := playbacktest.ChoiceState(dsdstate.ModeNative)
			if i%2 == 0 {
				st = capped
			}
			d.store.Set(st)
			select {
			case d.monitor <- time.Time{}:
			case <-stop:
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			select {
			case d.ticks <- time.Time{}:
			case <-stop:
				return
			}
		}
	}()
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(alsapcm.Watchdog()):
		close(stop)
		<-finished
		alsapcm.LogGoroutines(t)
		t.Fatalf("the monitor and the reporter did not each take %d ticks within %v", rounds, alsapcm.Watchdog())
	}
	// A barrier: the last tick's refresh is done.
	d.monitorTick()
	if g := d.be.Policy().Generation; g <= before {
		t.Fatalf("the set stayed at generation %d: no change reached reconciliation", g)
	}
	if evs := d.rec.events(); count(evs, "fault", 0)+count(evs, "replan", 0) != 0 {
		t.Errorf("a cap above the occurrence's rate re-planned or faulted it: %s", kinds(evs))
	}
	if err := d.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	d.rig.stop()
}
