package alsapcm_test

import (
	"bytes"
	"context"
	"errors"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The player's DSD by the effective policy (B728 B7, the cutover) through
// the engine's Run, the backend and the real native and DoP sinks over the
// fake kernel: what the policy admits is what opens, what plays is recorded
// in the player's DSD state, a change of the set is reconciled per
// occurrence, and nothing falls back to another transport.

// dsdRig is a rig whose backend's DSD is the effective policy (Config.DSD)
// over a box and a state the test sets: the test DAC at the fake's card,
// known to the profile database with a prefill of 1 ms, which the kernel's
// buffer holds, so a native or DoP start answers with no time played; every
// native or DoP open on the fake kernel, which takes S16_LE, S24_3LE, S32_LE
// and the three native formats; and each session's DSD monitor on a channel
// of the test's.
type dsdRig struct {
	*rig
	box     *playbacktest.Box
	store   *playbacktest.MemStore
	pol     *playback.DSDPolicy
	monitor chan time.Time

	mu    sync.Mutex
	opens []alsapcm.DSDConfig // every native or DoP open asked, in order
	pcm   int                 // every PCM open asked
	// cleanup, when set, is where each native or DoP sink opened from now on
	// fails its teardown (FakeKernel.OpenNativeSinkWithCleanupError): "close"
	// leaves its release unconfirmed.
	cleanup string
}

// card is the fake node's card, where the box has the test DAC.
var card = alsapcm.FakeEndpoint().Card

func newDSDRig(t *testing.T, st dsdstate.State, opts ...func(*rigOptions)) *dsdRig {
	t.Helper()
	return newDSDRigWith(t, st, nil, opts...)
}

// newDSDRigWith is newDSDRig with edit applied to the policy before the rig
// starts (B728 B11: a DAC no profile knows).
func newDSDRigWith(t *testing.T, st dsdstate.State, edit func(*playback.DSDPolicy), opts ...func(*rigOptions)) *dsdRig {
	t.Helper()
	d := &dsdRig{box: playbacktest.NewBox(card), store: playbacktest.NewMemStore(st), monitor: make(chan time.Time)}
	d.pol = playbacktest.Policy(card, d.box, d.store)
	d.pol.Profiles = playbacktest.KnownProfiles(&dacprofile.Timing{DSDPrefillMs: 1})
	d.pol.Tick = func() (<-chan time.Time, func()) { return d.monitor, func() {} }
	if edit != nil {
		edit(d.pol)
	}
	opts = append(opts, func(o *rigOptions) {
		o.native = true
		o.edit = func(r *rig, cfg *playback.Config) {
			cfg.DSD = d.pol
			cfg.OpenNative = func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				d.mu.Lock()
				d.opens = append(d.opens, c)
				cleanup := d.cleanup
				d.mu.Unlock()
				if cleanup != "" {
					return r.k.OpenNativeSinkWithCleanupError(ctx, c, cleanup)
				}
				return r.k.OpenNativeSink(ctx, c)
			}
			open := cfg.OpenSink
			cfg.OpenSink = func(ctx context.Context, c alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				d.mu.Lock()
				d.pcm++
				d.mu.Unlock()
				return open(ctx, c)
			}
		}
	})
	d.rig = newRig(t, opts...)
	d.k.HardwareFormats(2, 10, 32, 49, 50, 52)
	return d
}

// openFormats are the formats of every native or DoP open asked, in order.
func (d *dsdRig) openFormats() []format.ALSAFormat {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []format.ALSAFormat
	for _, c := range d.opens {
		out = append(out, c.Format)
	}
	return out
}

func (d *dsdRig) pcmOpens() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pcm
}

// monitorTick has the session's DSD monitor read the policy once: the second
// send is the barrier, taken once the first one's refresh is done. It is for
// a refresh that leaves the session playing; one that ends it takes
// monitorTickThatRevokes.
func (d *dsdRig) monitorTick() {
	d.t.Helper()
	for range 2 {
		select {
		case d.monitor <- time.Time{}:
		case <-time.After(alsapcm.Watchdog()):
			alsapcm.LogGoroutines(d.t)
			d.t.Fatalf("no DSD monitor took a tick within %v", alsapcm.Watchdog())
		}
	}
}

// monitorTickThatRevokes has the session's DSD monitor read the policy once,
// for a refresh that revokes the session's DSD: one send, bounded by the
// watchdog. The revocation ends the session, and its monitor may then leave
// on the session's end before it takes a second send, which nothing would
// take: monitorTick's barrier there waited out the watchdog and failed the
// test, a flake of B728 B7's TestAGainChangeRevokesDoPWithoutConsent. The
// caller waits on the stop the refresh makes, never on the monitor.
func (d *dsdRig) monitorTickThatRevokes() {
	d.t.Helper()
	select {
	case d.monitor <- time.Time{}:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(d.t)
		d.t.Fatalf("no DSD monitor took a tick within %v", alsapcm.Watchdog())
	}
}

// playedOut plays the queue, waits until every frame of refs is in the
// sink, then lets the DAC play d of what the sink holds: the prefill's 1 ms
// and then content, so a content the sink holds whole plays exactly d less
// the prefill.
func (d *dsdRig) playedOut(dur time.Duration, refs ...string) {
	d.t.Helper()
	d.play()
	for _, ref := range refs {
		d.files.fed(d.t, ref)
	}
	d.k.Play(d.t, dur)
}

// waitState waits until the player's DSD state satisfies cond.
func (d *dsdRig) waitState(what string, cond func(dsdstate.State) bool) dsdstate.State {
	d.t.Helper()
	waitOn(d.t, what, d.store.Changed, func() bool { return cond(d.store.State()) })
	return d.store.State()
}

// waitEnded waits for playback to stop, for whatever reason.
func (d *dsdRig) waitEnded() *engine.Snapshot {
	d.t.Helper()
	return d.waitSnap("playback to stop", func(s *engine.Snapshot) bool {
		return s.State() == engine.Stopped && s.LastStop().Reason != ""
	})
}

// fill is the tests' DSD content, each channel a pattern of its own, as the
// DSF file holds it: least significant bit first.
func fill(ch, i int) byte { return byte(i*7 + ch*101 + 1) }

// dsd is seconds of DSD at bitRate.
func dsd(bitRate int, seconds float64) []byte { return playbacktest.DSF(bitRate, seconds, fill) }

// The plan's timing the tests count with: the profile's prefill, the policy's
// tail (policy.DefaultTail), and DSD_U32_BE's frames a second at DSD64.
const (
	dsdPrefill   = time.Millisecond
	dsdTail      = 100 * time.Millisecond
	dsd64U32Rate = playbacktest.DSD64 / 32
)

// TestDriverAcceptedComesFromARealOpen (B728 B7; the design's section 1.1):
// the sink's open committed exactly the plan's format, rate and channels and
// passed the check after open, so the tuple's driver_accepted is recorded,
// under the running kernel and the unit's firmware; an open the DAC refused
// records nothing.
func TestDriverAcceptedComesFromARealOpen(t *testing.T) {
	t.Run("an open the DAC took", func(t *testing.T) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.files.add("a", dsd(playbacktest.DSD64, 0.5))
		d.insert("a")
		d.play()
		st := d.waitState("the open's driver_accepted", func(st dsdstate.State) bool {
			return playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64) != nil
		})
		e := playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64)
		if e.Kernel != playbacktest.Kernel || !e.DriverAcceptedAt.Equal(playbacktest.T0) || !e.StreamingObservedAt.IsZero() {
			t.Errorf("the evidence recorded: %+v; want driver_accepted under %s at %v, nothing streamed", e, playbacktest.Kernel, playbacktest.T0)
		}
		if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
			t.Errorf("the opens asked %v, want one DSD_U32_BE", got)
		}
	})
	t.Run("an open the DAC refused records nothing", func(t *testing.T) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.k.HardwareFormats(2, 10, 32)
		d.files.add("a", dsd(playbacktest.DSD64, 0.5))
		d.insert("a")
		_ = d.eng.Play(bg)
		d.waitEnded()
		d.rig.stop()
		if n := d.store.Writes(); n != 0 {
			t.Errorf("an open the DAC refused wrote the state %d times: %+v", n, d.store.State())
		}
		if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
			t.Errorf("the opens asked %v, want one DSD_U32_BE", got)
		}
	})
}

// TestStreamingObservedNeedsFiveSecondsWithoutAFault (B728 B7; the design's
// sections 1.1 and 6.2): a native or DoP occurrence whose own content the
// kernel established played for 5 s with no fault records its tuple's
// streaming_observed and its transport's first use observed, once; 4.9 s
// records neither, and neither does an occurrence whose decode failed before
// its 5 s played out.
func TestStreamingObservedNeedsFiveSecondsWithoutAFault(t *testing.T) {
	observed := func(st dsdstate.State) bool {
		e := playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64)
		return e != nil && !e.StreamingObservedAt.IsZero()
	}
	// framesOf is dur of DSD_U32_BE frames at DSD64.
	framesOf := func(dur time.Duration) uint64 { return uint64(dur) * dsd64U32Rate / uint64(time.Second) }
	t.Run("five seconds and a tenth", func(t *testing.T) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.files.add("a", dsd(playbacktest.DSD64, 5.5))
		d.insert("a")
		d.playedOut(dsdPrefill+5100*time.Millisecond, "a")
		d.tick()
		st := d.waitState("streaming_observed", observed)
		e := playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64)
		if !e.StreamingObservedAt.Equal(playbacktest.T0) {
			t.Errorf("streaming_observed at %v, want %v", e.StreamingObservedAt, playbacktest.T0)
		}
		d.waitState("native's first use observed", func(st dsdstate.State) bool {
			f := playbacktest.FirstUseOf(st, dsdstate.TransportNative)
			return f != nil && f.ObservedAt.Equal(playbacktest.T0)
		})
		if f := playbacktest.FirstUseOf(d.store.State(), dsdstate.TransportDoP); f == nil || !f.ObservedAt.IsZero() {
			t.Errorf("DoP's first use %+v: native's streaming observed DoP's", f)
		}
	})
	t.Run("four seconds and nine tenths", func(t *testing.T) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.files.add("a", dsd(playbacktest.DSD64, 5.5))
		d.insert("a")
		d.playedOut(dsdPrefill+4900*time.Millisecond, "a")
		d.tick()
		d.tick()
		if err := d.eng.Stop(bg); err != nil {
			t.Fatal(err)
		}
		d.rig.stop()
		if got := d.k.View().Consumed; got < framesOf(dsdPrefill+4900*time.Millisecond) {
			t.Fatalf("the DAC played %d frames, short of the 4.9 s the test grants", got)
		}
		st := d.store.State()
		if observed(st) {
			t.Errorf("4.9 s of content recorded streaming_observed: %+v", playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64))
		}
		if f := playbacktest.FirstUseOf(st, dsdstate.TransportNative); f == nil || !f.ObservedAt.IsZero() {
			t.Errorf("native's first use %+v after 4.9 s, want it unobserved", f)
		}
	})
	t.Run("a decode that failed before five seconds played out", func(t *testing.T) {
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		d.files.add("a", playbacktest.DSFCut(playbacktest.DSD64, 8, 5.5, fill))
		d.insert("a")
		// The sink takes all 5.5 s the file holds before the DAC plays any of
		// it: the decode fails at its end, the content closed, and only then
		// does the DAC play the rest out.
		d.play()
		d.files.fed(t, "a")
		if v := d.k.View(); v.Consumed != 0 {
			t.Fatalf("the DAC played %d frames before the decode failed", v.Consumed)
		}
		d.k.Free()
		d.waitEvents("the decode's fault", func(evs []event) bool { return count(evs, "fault", 0) > 0 })
		d.waitEnded()
		d.rig.stop()
		if got := d.k.View().Consumed; got < framesOf(dsdPrefill+5100*time.Millisecond) {
			t.Fatalf("the DAC played %d frames: the 5.5 s held did not play past 5 s", got)
		}
		if st := d.store.State(); observed(st) {
			t.Errorf("an occurrence whose decode failed recorded streaming_observed: %+v", playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64))
		}
	})
}

// TestTheOpenedNodeMustBeTheIdentityThePolicyWasComputedFor (B728 B7; the
// design's sections 3.4 and 3.5): the check after open requires the node's
// unit to be the DAC the set the occurrence was admitted under was computed
// for, its model and its firmware, never its port: another model, or the
// same model on another firmware, swapped in between the admission and the
// open, is refused before a frame, and nothing it did is recorded.
func TestTheOpenedNodeMustBeTheIdentityThePolicyWasComputedFor(t *testing.T) {
	for _, c := range []struct {
		name string
		unit func() alsa.UnitModel
	}{
		{"another model", func() alsa.UnitModel {
			m := playbacktest.DACUnit()
			m.USB.ID.Product, m.Product = 0x5679, "Other DAC"
			return m
		}},
		{"another firmware", func() alsa.UnitModel {
			m := playbacktest.DACUnit()
			m.BCDDevice = "0200"
			return m
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
			d.files.add("a", dsd(playbacktest.DSD64, 0.5))
			held, release := d.unit.holdNextAdmit()
			t.Cleanup(release)
			d.insert("a")
			go func() { _ = d.eng.Play(bg) }()
			select {
			case <-held:
			case <-time.After(alsapcm.Watchdog()):
				t.Fatalf("no check after open within %v", alsapcm.Watchdog())
			}
			d.box.SetUnit(card, c.unit())
			release()
			s := d.waitEnded()
			d.rig.stop()
			if v := d.k.View(); v.GivenBytes != 0 || v.Opens != 1 {
				t.Errorf("the DAC was given %d bytes over %d opens; want the one open refused before a frame", v.GivenBytes, v.Opens)
			}
			if a := s.LastAttempt(); !a.Failed || !strings.Contains(a.Detail, "not the DAC the effective DSD set was computed for") {
				t.Errorf("the start: %+v; want it refused by the check after open", a)
			}
			if n := d.store.Writes(); n != 0 {
				t.Errorf("a refused node wrote the state %d times: %+v", n, d.store.State())
			}
		})
	}
}

// TestAnUnusedRateCapChangeRevokesNothing (B728 B7; the design's section
// 3.5): a cap the choice takes that changes only a rate nothing plays at
// publishes a new set and keeps every occurrence: the one heard plays on,
// the successor in its sink joins it, and nothing is re-planned, revoked or
// cut.
func TestAnUnusedRateCapChangeRevokesNothing(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	d.files.add("a", dsd(playbacktest.DSD64, 0.5))
	d.files.add("b", dsd(playbacktest.DSD64, 0.5))
	ids := d.insert("a", "b")
	d.playedOut(dsdPrefill+100*time.Millisecond, "a", "b")
	d.tick()
	d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[0] })
	before := d.be.Policy()
	capped := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped.DACs[0].Choice.MaxBitRate, capped.DACs[0].Choice.Revision = playbacktest.DSD128, 2
	d.store.Set(capped)
	d.monitorTick()
	after := d.be.Policy()
	if after.Generation != before.Generation+1 {
		t.Fatalf("the cap moved the set from generation %d to %d, want the next", before.Generation, after.Generation)
	}
	var rates []int
	for _, p := range after.Tuples {
		rates = append(rates, p.Tuple.BitRate)
	}
	if !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128}) {
		t.Errorf("the capped set's tuples are at %v, want DSD64 and DSD128", rates)
	}
	d.k.Free()
	s := d.waitEnded()
	evs := d.rec.events()
	if n := count(evs, "replan", 0) + count(evs, "fault", 0); n != 0 || len(s.Faults()) != 0 {
		t.Errorf("an unused cap re-planned or faulted: %s; faults %+v", kinds(evs), s.Faults())
	}
	if n := count(evs, "audible", 0); n != 2 {
		t.Errorf("%d occurrences heard, want a and b: %s", n, kinds(evs))
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want a and b in one sink", v.Opens)
	}
	for _, c := range d.rec.callList() {
		if c.kind == "cut" {
			t.Errorf("an unused cap asked a cut: %+v", c)
		}
	}
}

// TestAChangedChoiceReplansThePreparedSuccessorAndSkipsNothing (B728 B7;
// the design's section 3.5): a choice changed from native to DoP while a
// PCM track plays leaves the prepared DSD successor's plan out of the set
// before any of it reached the kernel, and the engine hands it off again
// through its edit path: it plays as DoP, its entry is heard, nothing is
// skipped, and no native sink is opened for it.
func TestAChangedChoiceReplansThePreparedSuccessorAndSkipsNothing(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	d.files.add("a", wav(44100, 16, pattern(1, 22050, 16)))
	d.files.add("b", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("a", "b")
	d.play()
	prepared := d.waitCall("b handed off", func(c call) bool { return c.kind == "prepare" && c.next != nil && c.next.Entry == ids[1] })
	d.waitEvents("b prepared", func(evs []event) bool { return count(evs, "prepared", prepared.next.ID) > 0 })
	d.store.Set(playbacktest.ChoiceState(dsdstate.ModeDoP))
	d.be.RefreshDSD(true)
	d.waitEvents("b handed off again", func(evs []event) bool { return count(evs, "replan", prepared.next.ID) > 0 })
	again := d.waitCall("b handed off again", func(c call) bool {
		return c.kind == "prepare" && c.next != nil && c.next.Entry == ids[1] && c.next.ID != prepared.next.ID
	})
	d.k.Free()
	s := d.waitEnded()
	evs := d.rec.events()
	if count(evs, "audible", again.next.ID) != 1 || count(evs, "fault", 0) != 0 {
		t.Errorf("b's new occurrence was not heard, or a fault: %s", kinds(evs))
	}
	if sk := s.Skips(); len(sk) != 0 {
		t.Errorf("the change skipped %+v", sk)
	}
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.S32LE}) {
		t.Errorf("the DSD opens asked %v, want one DoP S32_LE", got)
	}
	d.mu.Lock()
	kind := d.opens[0].Kind
	d.mu.Unlock()
	if kind != format.KindDoP {
		t.Errorf("b opened as %v, want DoP", kind)
	}
}

// TestNoTransportFallsBackAtRunTime (B728 B7; R11): one set per occurrence,
// never a fallback at run time: under auto, a known DAC whose set plays
// native DSD at DSD64, DoP heard, confirmed and acknowledged beside it,
// refuses the native open at run time, and the occurrence fails there: no
// DoP open, no PCM open, nothing given to the DAC.
func TestNoTransportFallsBackAtRunTime(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeAuto))
	d.k.HardwareFormats(2, 10, 32)
	d.files.add("a", dsd(playbacktest.DSD64, 0.5))
	d.insert("a")
	_ = d.eng.Play(bg)
	d.waitEnded()
	p := d.be.Policy()
	if len(p.Tuples) == 0 || p.Tuples[0].Tuple.Transport != "" || p.Tuples[0].Tuple.Format != string(format.DSDU32BE) {
		t.Fatalf("the set published %+v; want native DSD_U32_BE at DSD64", p.Tuples)
	}
	d.rig.stop()
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
		t.Errorf("the opens asked %v, want the native one alone", got)
	}
	if n := d.pcmOpens(); n != 0 {
		t.Errorf("%d PCM opens after the native open failed", n)
	}
	if v := d.k.View(); v.GivenBytes != 0 {
		t.Errorf("the DAC was given %d bytes", v.GivenBytes)
	}
}

// TestAGainChangeRevokesDoPWithoutConsent (B728 B7; the design's sections
// 3.4 and 7.2): DoP plays through a feature unit at 0 dB; the volume leaving
// 0 dB is seen by the session's 100 ms monitor, which reads the gain check
// with the policy, and DoP stops at once, a safety change no consent is
// asked for: the state is not written, and the set published refuses DoP
// with the gain's word.
func TestAGainChangeRevokesDoPWithoutConsent(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeDoP))
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	d.box.SetUnit(card, unit)
	d.box.SetMixer(card, playbacktest.VolumeMixer())
	d.box.SetControls(card, playbacktest.VolumeControls(255)...)
	d.files.add("a", dsd(playbacktest.DSD64, 2))
	ids := d.insert("a")
	d.playedOut(dsdPrefill+200*time.Millisecond, "a")
	d.tick()
	d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[0] })
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.S32LE}) {
		t.Fatalf("the opens asked %v, want DoP's S32_LE", got)
	}
	d.waitState("the open's driver_accepted", func(st dsdstate.State) bool {
		return playbacktest.Evidence(st, dsdstate.TransportDoP, "S32_LE", playbacktest.DSD64) != nil
	})
	writes := d.store.Writes()
	d.box.SetControls(card, playbacktest.VolumeControls(200)...)
	d.monitorTickThatRevokes()
	s := d.tickUntilStopped(engine.ReasonFault)
	if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
		t.Errorf("the gain change's faults %+v, want DoP revoked", f)
	}
	p := d.be.Policy()
	if len(p.Tuples) != 0 || p.Reason != "dop_gain_not_unity" {
		t.Errorf("the set published %+v (%q), want no tuple and dop_gain_not_unity", p.Tuples, p.Reason)
	}
	d.rig.stop()
	if n := d.store.Writes(); n != writes {
		t.Errorf("the revocation wrote the state (%d writes, %d before): no consent is asked or recorded", n, writes)
	}
	if c, _ := d.store.State().DAC(playbacktest.DACKey()); c.Choice == nil || c.Choice.Mode != dsdstate.ModeDoP {
		t.Errorf("the choice after the revocation: %+v, want DoP as the user left it", c.Choice)
	}
}

// TestAGainOffUnityAtTheOpenRefusesDoP (B728 B7; the design's section 3.4):
// the check after open of a DoP occurrence reads the gain check afresh at
// the node's card, so a volume that leaves 0 dB between the admission and the
// open refuses the open before a frame, and nothing it did is recorded.
func TestAGainOffUnityAtTheOpenRefusesDoP(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeDoP))
	unit := playbacktest.DACUnit()
	unit.Descriptors = playbacktest.VolumeDescriptors()
	d.box.SetUnit(card, unit)
	d.box.SetMixer(card, playbacktest.VolumeMixer())
	d.box.SetControls(card, playbacktest.VolumeControls(255)...)
	d.files.add("a", dsd(playbacktest.DSD64, 0.5))
	held, release := d.unit.holdNextAdmit()
	t.Cleanup(release)
	d.insert("a")
	go func() { _ = d.eng.Play(bg) }()
	select {
	case <-held:
	case <-time.After(alsapcm.Watchdog()):
		t.Fatalf("no check after open within %v", alsapcm.Watchdog())
	}
	d.box.SetControls(card, playbacktest.VolumeControls(200)...)
	release()
	s := d.waitEnded()
	d.rig.stop()
	if v := d.k.View(); v.GivenBytes != 0 || v.Opens != 1 {
		t.Errorf("the DAC was given %d bytes over %d opens; want the one open refused before a frame", v.GivenBytes, v.Opens)
	}
	if a := s.LastAttempt(); !a.Failed || !strings.Contains(a.Detail, "DoP refused at the open") || !strings.Contains(a.Detail, "dop_gain_not_unity") {
		t.Errorf("the start: %+v; want DoP refused at the open for the gain", a)
	}
	if n := d.store.Writes(); n != 0 {
		t.Errorf("a refused node wrote the state %d times: %+v", n, d.store.State())
	}
}

// nativeWordsOf is what a DSD_U32_BE DAC is given for content of bits a
// channel as the DSF file holds it (fill): the prefill's frames of 0x69, then
// frames of a left and a right 32-bit word, each byte the file's with its
// bits reversed (canonical DSD: the earliest bit at bit 7), then the tail's
// frames of 0x69; worked out apart from the decoder and the sink.
func nativeWordsOf(bitsPerChannel int, prefill, tail int) []byte {
	out := bytes.Repeat([]byte{0x69}, 8*prefill)
	for w := range bitsPerChannel / 32 {
		for ch := range 2 {
			for i := range 4 {
				out = append(out, bits.Reverse8(fill(ch, w*4+i)))
			}
		}
	}
	return append(out, bytes.Repeat([]byte{0x69}, 8*tail)...)
}

// TestAMigratedOwnerSetupPlaysAsBefore (B728 B7; binding note 1): the state
// the migration left for an owner's native setup and record plays native
// DSD at the rates the record approved, with no new acknowledgement, and the
// DAC is given exactly the native words of the content; a DoP setup plays
// DoP the same way.
func TestAMigratedOwnerSetupPlaysAsBefore(t *testing.T) {
	for _, c := range []struct {
		name      string
		st        dsdstate.State
		want      format.ALSAFormat
		transport string
	}{
		{"native", playbacktest.MigratedState(dsdstate.ModeNative, false, []int{playbacktest.DSD64, playbacktest.DSD128}, nil), format.DSDU32BE, dsdstate.TransportNative},
		{"dop", playbacktest.MigratedState(dsdstate.ModeDoP, true, nil, []int{playbacktest.DSD64}), format.S32LE, dsdstate.TransportDoP},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDSDRig(t, c.st)
			d.files.add("a", dsd(playbacktest.DSD64, 0.25))
			d.insert("a")
			d.k.Free()
			d.play()
			s := d.waitEnded()
			if s.LastStop().Reason == engine.ReasonFault || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
				t.Fatalf("the migrated setup did not play: stop %+v, faults %+v, skips %+v", s.LastStop(), s.Faults(), s.Skips())
			}
			if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{c.want}) {
				t.Errorf("the opens asked %v, want %s", got, c.want)
			}
			if n := count(d.rec.events(), "audible", 0); n != 1 || s.TrackCount() != 1 {
				t.Errorf("%d occurrences heard, %d tracks counted: want the one", n, s.TrackCount())
			}
			d.rig.stop()
			if c.want == format.DSDU32BE {
				// 0.25 s of DSD64 is 22,050 words a channel; the prefill's 1 ms
				// is 89 frames (88.2 rounded up), the tail's 100 ms 8,820.
				if got, want := d.k.Given(), nativeWordsOf(playbacktest.DSD64/4, 89, 8820); !bytes.Equal(got, want) {
					t.Errorf("the DAC was given %d bytes that are not the content's native words (%d bytes)", len(got), len(want))
				}
			}
			if e := playbacktest.Evidence(d.store.State(), c.transport, string(c.want), playbacktest.DSD64); e == nil || e.DriverAcceptedAt.IsZero() {
				t.Errorf("the migrated setup's open recorded %+v", e)
			}
		})
	}
}

// TestPCMOnlyPlaysAsItDidAndReadsNoBox (B728 B7): a session that plays PCM
// alone reads nothing of the policy: no box read, no state write, no DSD
// open; its samples reach the DAC as they did.
func TestPCMOnlyPlaysAsItDidAndReadsNoBox(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	samples := pattern(1, 4410, 16)
	d.files.add("a", wav(44100, 16, samples))
	d.insert("a")
	d.k.Free()
	d.play()
	d.waitEnded()
	d.rig.stop()
	if n := d.box.Reads(); n != 0 {
		t.Errorf("a PCM session read the box %d times", n)
	}
	if n := d.store.Writes(); n != 0 {
		t.Errorf("a PCM session wrote the DSD state %d times", n)
	}
	if got := d.openFormats(); len(got) != 0 {
		t.Errorf("a PCM session asked DSD opens %v", got)
	}
	d.heardAll(format.S16LE, 16, samples)
}

// TestAProbeDueWhileTheDeviceIsHeldWaits (B728 B7; binding note 4): a probe
// due while a session holds the device, playing or paused, opens nothing and
// stays due; once the release is confirmed the next ask runs it, with B4's
// preconditions, and records what the DAC accepted, and a probe that ran is
// not due again for the same unit.
func TestAProbeDueWhileTheDeviceIsHeldWaits(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative))
	cond := playback.ProbeConditions{Available: true, Fresh: true}
	d.files.add("a", dsd(playbacktest.DSD64, 2))
	d.insert("a")
	d.playedOut(dsdPrefill+100*time.Millisecond, "a")
	refines := d.k.View().Refines
	if !d.be.ProbeDSD(bg, cond) {
		t.Fatal("a probe due while a session plays answered not pending")
	}
	if v := d.k.View(); v.Refines != refines || v.Opens != 1 {
		t.Fatalf("the probe opened the node while a session held it: %d refines (%d before), %d opens", v.Refines, refines, v.Opens)
	}
	if err := d.eng.Pause(bg); err != nil {
		t.Fatal(err)
	}
	d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	if !d.be.ProbeDSD(bg, cond) {
		t.Fatal("a probe due while a session is paused answered not pending")
	}
	if v := d.k.View(); v.Refines != refines || v.Opens != 1 {
		t.Fatalf("the probe opened the node while a paused session held it: %d refines (%d before), %d opens", v.Refines, refines, v.Opens)
	}
	if err := d.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	d.waitSnap("stopped and released", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	if d.be.ProbeDSD(bg, cond) {
		t.Fatal("a probe due once the device is free answered pending")
	}
	if v := d.k.View(); v.Refines == refines {
		t.Fatal("the probe did not run once the device was free")
	}
	d.waitState("the probe's driver_accepted", func(st dsdstate.State) bool {
		return playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD128) != nil
	})
	if d.be.ProbeDSD(bg, cond) {
		t.Error("a probe that ran is still due for the same unit")
	}
}

// policyLog keeps the lines of warning the effective policy says.
type policyLog struct {
	mu    sync.Mutex
	warns []string
}

func (l *policyLog) Info(string, ...any) {}

func (l *policyLog) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

// count is the warnings that say what.
func (l *policyLog) count(what string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		if strings.Contains(w, what) {
			n++
		}
	}
	return n
}

// TestALandedMigrationFencesWhatPlaybackRecords (B728 B7; binding note 10):
// the migration's write reaches the file and its barrier then fails, so the
// file holds the migrated state and the store does not; every later write
// is refused (dsdstate.ErrLandedNotHeld): the driver acceptance a real open
// records and what the silent probe accepted are not written over the file,
// which keeps the state that landed, the store holds none of it, and the
// fence is said once.
func TestALandedMigrationFencesWhatPlaybackRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dsdstate.FileName)
	// The seams first, so their restores run once the rig has stopped.
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	var failed atomic.Bool
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if st, err := dsdstate.Decode(b); err == nil && st.MigratedFrom != nil && failed.CompareAndSwap(false, true) {
			return errors.New("the test's barrier fails once the migration's write is there")
		}
		return nil
	}))
	store, err := dsdstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	chosen := playbacktest.ChoiceState(dsdstate.ModeNative)
	if err := store.Update(func(st *dsdstate.State) error { *st = chosen.Clone(); return nil }); err != nil {
		t.Fatalf("the choice's write: %v", err)
	}
	bound := dsdstate.Bound{Key: playbacktest.DACKey()}
	if _, err := store.Migrate(dir, dsdstate.Legacy{DSDMode: dsdstate.ModeNative}, &bound); !errors.Is(err, atomicwrite.ErrLanded) {
		t.Fatalf("the migration's write: %v, want it landed and not known to be durable", err)
	}
	landed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := newDSDRig(t, dsdstate.Defaults())
	log := &policyLog{}
	d.pol.Store, d.pol.Log = store, log
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.insert("a")
	d.k.Free()
	d.play()
	d.waitEnded()
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
		t.Fatalf("the opens asked %v, want the store's native choice played", got)
	}
	if d.be.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}) {
		t.Fatal("a probe due with no session answered pending")
	}
	d.rig.stop()
	if v := d.k.View(); v.Refines == 0 {
		t.Fatal("the probe asked the kernel nothing")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, landed) {
		t.Errorf("the file after playback and the probe: %v; want the state that landed, written over by nothing", err)
	}
	if e := playbacktest.Evidence(store.State(), dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64); e != nil || store.State().MigratedFrom != nil {
		t.Errorf("the store holds evidence %+v and marker %+v, want neither", e, store.State().MigratedFrom)
	}
	if n := log.count("not written to the player's DSD state in this run"); n != 1 {
		t.Errorf("the fence was said %d times, want once: %q", n, log.warns)
	}
}
