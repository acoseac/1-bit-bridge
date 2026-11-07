package playback_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The effective DSD policy's work off the supervisor's loop (B762): the
// silent probe and the refresh asked for and run on the backend's goroutines,
// one probe at a time with one ask kept, its answers taken only while they are
// current, a Start beside it, and the goroutine that runs it counted from the
// ask.

// watchdog bounds each wait on a goroutine of the backend's, and askWithin an
// ask's return, which takes a lock and starts a goroutine: watchdogs, never a
// timing a test depends on.
const (
	watchdog  = time.Minute
	askWithin = 10 * time.Second
)

// The frame rates at which DSD_U32_BE carries DSD64 and DSD128.
const (
	dsd64Frames  = playbacktest.DSD64 / 32
	dsd128Frames = playbacktest.DSD128 / 32
)

// ready is the probe's conditions as a fresh snapshot of an available DAC
// gives them.
var ready = playback.ProbeConditions{Available: true, Fresh: true}

// within waits for ch at most the watchdog, and returns what came.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	return withinFor(t, ch, watchdog, what)
}

// withinFor waits for ch at most d.
func withinFor[T any](t *testing.T, ch <-chan T, d time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("waited %v for %s", d, what)
	}
	var zero T
	return zero
}

// heldProbe is the silent probe as the kernel answers it here (Config.Probe):
// each call counted, its number sent on came as it comes, a call the test
// holds waiting there with the node open until let go, and each answering
// DSD_U32_BE accepted at the frame rate rates gives it (the last for the calls
// past them), every other candidate refused; a call the test makes busy
// finds the node another's (EBUSY), opens nothing and asks nothing.
type heldProbe struct {
	came  chan int
	rates []int

	mu    sync.Mutex
	gates map[int]chan struct{}
	busy  map[int]bool
	calls int
	open  int // calls under way: the node open
}

func newHeldProbe(rates ...int) *heldProbe {
	return &heldProbe{came: make(chan int, 16), rates: rates, gates: map[int]chan struct{}{}, busy: map[int]bool{}}
}

// busyAt makes call n find the node busy.
func (h *heldProbe) busyAt(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.busy[n] = true
}

// hold has call n wait until let.
func (h *heldProbe) hold(n int) (let func()) {
	g := make(chan struct{})
	h.mu.Lock()
	h.gates[n] = g
	h.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(g) }) }
}

func (h *heldProbe) probe(ep alsapcm.Endpoint, c alsapcm.ProbeConfig) (alsapcm.ProbeReport, error) {
	h.mu.Lock()
	n := h.calls
	h.calls++
	h.open++
	g, busy := h.gates[n], h.busy[n]
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.open--
		h.mu.Unlock()
	}()
	h.came <- n
	if g != nil {
		<-g
	}
	if busy {
		rep := alsapcm.ProbeReport{Endpoint: ep, Results: make([]alsapcm.ProbeResult, len(c.Candidates))}
		for i, cand := range c.Candidates {
			rep.Results[i] = alsapcm.ProbeResult{Candidate: cand, Answer: alsapcm.ProbeNotProbed}
		}
		return rep, &alsapcm.Error{Reason: alsapcm.ReasonBusy, Op: "probe", Endpoint: ep, Detail: "the test's node is another's"}
	}
	rate := h.rates[min(n, len(h.rates)-1)]
	rep := alsapcm.ProbeReport{Endpoint: ep, Opened: true, Results: make([]alsapcm.ProbeResult, len(c.Candidates))}
	for i, cand := range c.Candidates {
		answer := alsapcm.ProbeRefused
		if cand.Format == format.DSDU32BE && cand.RateHz == rate {
			answer = alsapcm.ProbeAccepted
		}
		rep.Results[i] = alsapcm.ProbeResult{Candidate: cand, Answer: answer}
	}
	return rep, nil
}

// counts are the probe's calls, and those under way.
func (h *heldProbe) counts() (calls, open int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.open
}

// boundAt is the DAC the binding resolves to, which a test moves: a replug
// enumerates the test DAC anew, an unplug leaves none. A read the test holds
// (holdNext) waits until let go.
type boundAt struct {
	mu   sync.Mutex
	bd   playback.BoundDAC
	ok   bool
	next chan struct{} // the next read waits on it, once
	came chan struct{} // closed as that read comes
	boom bool          // the next read panics, once
}

func newBoundAt() *boundAt {
	return &boundAt{bd: playback.BoundDAC{Card: 5, Identity: playbacktest.DACUSB(2), Enumeration: "test/2"}, ok: true}
}

func (b *boundAt) get() (playback.BoundDAC, bool) {
	b.mu.Lock()
	next, came, boom := b.next, b.came, b.boom
	b.next, b.came, b.boom = nil, nil, false
	b.mu.Unlock()
	if boom {
		panic("a read of the binding")
	}
	if next != nil {
		close(came)
		<-next
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bd, b.ok
}

// holdNext holds the next read until let, and says when it came.
func (b *boundAt) holdNext() (came <-chan struct{}, let func()) {
	next, c := make(chan struct{}), make(chan struct{})
	b.mu.Lock()
	b.next, b.came = next, c
	b.mu.Unlock()
	var once sync.Once
	return c, func() { once.Do(func() { close(next) }) }
}

// panicNext makes the next read panic.
func (b *boundAt) panicNext() {
	b.mu.Lock()
	b.boom = true
	b.mu.Unlock()
}

// replug puts the test DAC back in the box as enumeration devnum.
func (b *boundAt) replug(box *playbacktest.Box, devnum int) {
	m := playbacktest.DACUnit()
	m.USB = playbacktest.DACUSB(devnum)
	box.SetUnit(5, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bd, b.ok = playback.BoundDAC{Card: 5, Identity: m.USB, Enumeration: fmt.Sprintf("test/%d", devnum)}, true
}

// unplug takes it out of the box.
func (b *boundAt) unplug(box *playbacktest.Box) {
	box.SetUnit(5, alsa.UnitModel{})
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ok = false
}

// probedAt is the unit a probe of the test DAC at enumeration devnum keeps
// its answers under.
func probedAt(devnum int) alsapcm.ProbeUnit {
	return alsapcm.ProbeUnit{Model: playbacktest.DACKey().Canonical(), Firmware: playbacktest.DACBCD, Kernel: playbacktest.Kernel, Enumeration: fmt.Sprintf("test/%d", devnum)}
}

// accepted are the frame rates at which the probe's cache keeps DSD_U32_BE
// accepted under u, and kept how many answers it keeps there at all.
func accepted(pol *playback.DSDPolicy, u alsapcm.ProbeUnit) (rates []int, kept int) {
	for _, c := range alsapcm.ProbeCandidates() {
		r := pol.Probes.Answer(u, c)
		if r.Answer != alsapcm.ProbeNotProbed {
			kept++
		}
		if r.Answer == alsapcm.ProbeAccepted && c.Format == format.DSDU32BE {
			rates = append(rates, c.RateHz)
		}
	}
	return rates, kept
}

// probeEvidence is the driver_accepted the player's DSD state holds for the
// test DAC's DSD_U32_BE at bitRate, nil for none.
func probeEvidence(st dsdstate.State, bitRate int) *dsdstate.Evidence {
	e := playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", bitRate)
	if e == nil || e.DriverAcceptedAt.IsZero() {
		return nil
	}
	return e
}

// wavOf is a canonical WAV of frames silent 16-bit stereo frames at 44.1
// kHz: content the PCM path opens and decodes.
func wavOf(frames int) []byte {
	data := frames * 4
	b := append(make([]byte, 0, 44+data), "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+data))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint32(b, 44100)
	b = binary.LittleEndian.AppendUint32(b, 44100*4)
	b = binary.LittleEndian.AppendUint16(b, 4)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(data))
	return append(b, make([]byte, data)...)
}

// wavContent opens every reference as a tenth of a second of that WAV.
type wavContent struct{}

func (wavContent) Open(context.Context, engine.Reference) (playback.Content, error) {
	w := wavOf(4410)
	return playback.Content{Data: bytes.NewReader(w), Size: int64(len(w))}, nil
}

// quietEvents is a session's engine side that takes every event.
type quietEvents struct{}

func (quietEvents) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join)      {}
func (quietEvents) Refused(engine.SessionID, engine.OccurrenceID, engine.Unplayable) {}
func (quietEvents) Submitted(engine.SessionID, engine.OccurrenceID)                  {}
func (quietEvents) Audible(engine.SessionID, engine.OccurrenceID)                    {}
func (quietEvents) Position(engine.SessionID, engine.Position)                       {}
func (quietEvents) Fault(engine.SessionID, engine.Fault)                             {}
func (quietEvents) Finished(engine.SessionID)                                        {}
func (quietEvents) DeviceReleased(engine.SessionID)                                  {}
func (quietEvents) Released(engine.SessionID)                                        {}

// startOf is the engine's Start of session id, its first occurrence a WAV.
func startOf(t *testing.T, id engine.SessionID) engine.StartRequest {
	t.Helper()
	ref, err := engine.NewReference("a.wav")
	if err != nil {
		t.Fatal(err)
	}
	return engine.StartRequest{Session: id, First: engine.Occurrence{ID: 1, Entry: 1, Ref: ref}, Events: quietEvents{}}
}

// startTimer stands in for the timers the backend arms (Config.AfterFunc):
// armed is closed as a Start's wait for a probe arms its bound (ProbeWait),
// and none fires until the test expires it, so a Start waits for the probe it
// came beside.
type startTimer struct {
	once  sync.Once
	armed chan struct{}

	mu   sync.Mutex
	fire func()
}

func (s *startTimer) afterFunc(d time.Duration, f func()) func() bool {
	if d == playback.ProbeWait {
		s.mu.Lock()
		s.fire = f
		s.mu.Unlock()
		s.once.Do(func() { close(s.armed) })
	}
	return func() bool { return true }
}

// expire passes the bound of the Start's wait for the probe.
func (s *startTimer) expire() {
	s.mu.Lock()
	f := s.fire
	s.mu.Unlock()
	f()
}

// sinkOpens is the backend's opens of the DAC here (Config.OpenSink): each
// counted, and each made while a call of the probe has the node open, and each
// refused, since no test here opens a device.
type sinkOpens struct {
	probe *heldProbe

	mu            sync.Mutex
	opens, beside int
}

func (s *sinkOpens) open(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
	_, open := s.probe.counts()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if open > 0 {
		s.beside++
	}
	return nil, errors.New("the test's DAC opens nothing")
}

func (s *sinkOpens) counts() (opens, beside int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.beside
}

// probeRig is a backend whose DSD is the effective policy over the test DAC
// at card 5 (playbacktest), its binding resolved by bound, its silent probe
// probe, its content a WAV and its DAC's opens counted and refused.
type probeRig struct {
	b     *playback.Backend
	pol   *playback.DSDPolicy
	box   *playbacktest.Box
	store *playbacktest.MemStore
	bound *boundAt
	timer *startTimer
	sinks *sinkOpens
	log   *loggingtest.Recorder
}

func newProbeRig(t *testing.T, probe *heldProbe, edits ...func(*playback.Config)) *probeRig {
	t.Helper()
	r := &probeRig{box: playbacktest.NewBox(5), store: playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)),
		bound: newBoundAt(), timer: &startTimer{armed: make(chan struct{})}, sinks: &sinkOpens{probe: probe}, log: loggingtest.NewRecorder()}
	r.pol = playbacktest.Policy(5, r.box, r.store)
	r.pol.Bound = r.bound.get
	cfg := playback.Config{Resolver: wavContent{}, Locator: cardFive{}, DSD: r.pol, Probe: probe.probe,
		AfterFunc: r.timer.afterFunc, OpenSink: r.sinks.open, Log: r.log.Logger()}
	for _, edit := range edits {
		edit(&cfg)
	}
	b, err := playback.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.b = b
	return r
}

// heldOnce is a seam's hold of the first goroutine that reaches it, until
// let: came is closed as that one comes, and every later one passes.
type heldOnce struct {
	came, gate chan struct{}
	first      atomic.Bool
	letOnce    sync.Once
}

func newHeldOnce() *heldOnce { return &heldOnce{came: make(chan struct{}), gate: make(chan struct{})} }

func (h *heldOnce) reach() {
	if h.first.CompareAndSwap(false, true) {
		close(h.came)
		<-h.gate
	}
}

func (h *heldOnce) let() { h.letOnce.Do(func() { close(h.gate) }) }

// idle waits until no goroutine of the backend's runs: what a probe's end
// wrote, and the refresh it asked for, are done.
func (r *probeRig) idle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := r.b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// ask is the supervisor's ask for the probe, which returns at once: a held
// probe holds nothing of the asker.
func (r *probeRig) ask(t *testing.T, ctx context.Context, cond playback.ProbeConditions) {
	t.Helper()
	asked := make(chan struct{})
	go func() {
		defer close(asked)
		r.b.RequestProbeDSD(ctx, cond)
	}()
	withinFor(t, asked, askWithin, "the ask for the probe to return")
}

// TestAStaleProbeKeepsNothingAndStaysOwed (B762): the silent probe's
// answers are taken in one step once it ends, and only while they are still
// current: a choice written for the DAC while it ran (ForgetDSDProbe), or the
// DAC enumerated anew, drops them, so the probe's cache keeps nothing under
// the unit it probed, no driver_accepted is written into the player's DSD
// state, and the probe is still owed; the next ask runs it, and keeps its
// answers. A probe that met a busy node (MPD's) answers nothing for the cache
// to refuse, so a choice written while it ran is known by the probe's
// generation alone: it leaves the probe asked anew, which runs. And the
// cache's own epoch, moved while the probe ran with neither the generation
// nor the binding moved, is known by the cache's refusal of the report alone
// (round 2: every case before moved one of the two, so the refusal was never
// asked alone).
func TestAStaleProbeKeepsNothingAndStaysOwed(t *testing.T) {
	for _, c := range []struct {
		name   string
		busy   bool // the held call meets a busy node
		during func(r *probeRig)
		now    int // the enumeration the next ask probes
	}{
		{"a choice written for the DAC while it ran", false, func(r *probeRig) { r.b.ForgetDSDProbe(playbacktest.DACKey()) }, 2},
		{"a choice written while a probe that met a busy node ran", true, func(r *probeRig) { r.b.ForgetDSDProbe(playbacktest.DACKey()) }, 2},
		{"the DAC enumerated anew while it ran", false, func(r *probeRig) { r.bound.replug(r.box, 3) }, 3},
		{"the probe cache's epoch moved alone while it ran", false, func(r *probeRig) { r.pol.Probes.Forget(playbacktest.DACKey().Canonical()) }, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames, dsd128Frames)
			if c.busy {
				probe.busyAt(0)
			}
			let := probe.hold(0)
			defer let()
			r := newProbeRig(t, probe)
			owed := make(chan bool, 1)
			go func() { owed <- r.b.ProbeDSD(context.Background(), ready) }()
			within(t, probe.came, "the probe's call")
			c.during(r)
			let()
			if !within(t, owed, "the stale probe's end") {
				t.Error("a probe whose answers were dropped answered nothing owed")
			}
			r.idle(t)
			if rates, kept := accepted(r.pol, probedAt(2)); kept != 0 {
				t.Errorf("the probe's cache keeps %d answers (DSD_U32_BE at %v) from a stale probe", kept, rates)
			}
			if e := probeEvidence(r.store.State(), playbacktest.DSD64); e != nil {
				t.Errorf("the player's DSD state holds %+v from a stale probe", e)
			}
			if !r.b.ProbeDSDPending() {
				t.Error("the probe is not owed once a stale one's answers were dropped")
			}
			// The probe owed runs at the next ask, under the unit bound now, and
			// keeps what it asked.
			if r.b.ProbeDSD(context.Background(), ready) {
				t.Error("the probe owed answered owed again once it ran")
			}
			if calls, _ := probe.counts(); calls != 2 {
				t.Fatalf("the probe was called %d times; want the stale call and the one owed", calls)
			}
			r.idle(t)
			if rates, _ := accepted(r.pol, probedAt(c.now)); !slices.Equal(rates, []int{dsd128Frames}) {
				t.Errorf("the cache keeps DSD_U32_BE accepted at %v under the unit bound now, want the probe owed's %d alone", rates, dsd128Frames)
			}
			if probeEvidence(r.store.State(), playbacktest.DSD128) == nil || probeEvidence(r.store.State(), playbacktest.DSD64) != nil {
				t.Errorf("the state holds DSD128 %v and DSD64 %v: want the probe owed's answer alone",
					probeEvidence(r.store.State(), playbacktest.DSD128), probeEvidence(r.store.State(), playbacktest.DSD64))
			}
			if r.b.ProbeDSDPending() {
				t.Error("a probe is still owed once the one owed ran")
			}
		})
	}
}

// TestAStartBesideAHeldProbeOpensOnlyOnceItEndsAndVoidsItsAnswers (B762; the
// design's section 5): a Start that comes while the silent probe holds the
// device slot waits for it (ProbeWait), and opens the DAC only once the probe
// has closed its node, so the device is never opened twice. The probe's
// answers are taken only while no Start has taken the slot since the probe
// began, counted where the Start takes the slot, never at its coming (round
// 2): taken after the Start took it, they are dropped, neither kept in the
// cache nor written into the player's DSD state, and the probe is owed, which
// runs once the session has let the device go; taken before, they are kept,
// and the session then opens the DAC.
func TestAStartBesideAHeldProbeOpensOnlyOnceItEndsAndVoidsItsAnswers(t *testing.T) {
	t.Run("the Start takes the slot before the answers are taken", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames, dsd128Frames)
		let := probe.hold(0)
		defer let()
		probed := newHeldOnce()
		defer probed.let()
		r := newProbeRig(t, probe, func(c *playback.Config) { c.Seams = &playback.Seams{Probed: probed.reach} })
		ctx := context.Background()
		r.ask(t, ctx, ready)
		within(t, probe.came, "the probe's call")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(ctx, startOf(t, 1)) }()
		within(t, r.timer.armed, "the Start's wait for the probe")
		if opens, _ := r.sinks.counts(); opens != 0 {
			t.Fatalf("the Start opened the DAC %d times while the probe held it", opens)
		}
		let()
		within(t, probed.came, "the probe's goroutine once its call returned")
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		probed.let()
		r.idle(t)
		if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
			t.Errorf("the DAC was opened %d times, %d of them while the probe had its node open; want once, after it", opens, beside)
		}
		if rates, kept := accepted(r.pol, probedAt(2)); kept != 0 {
			t.Errorf("the probe's cache keeps %d answers (DSD_U32_BE at %v) from a probe a session took the slot after", kept, rates)
		}
		if e := probeEvidence(r.store.State(), playbacktest.DSD64); e != nil {
			t.Errorf("the player's DSD state holds %+v from a probe a session took the slot after", e)
		}
		if !r.b.ProbeDSDPending() {
			t.Error("the probe is not owed once its answers were dropped")
		}
		// The session lets go of the device; the probe owed runs then.
		if err := r.b.Release(ctx, 1); err != nil {
			t.Fatal(err)
		}
		r.ask(t, ctx, ready)
		if n := within(t, probe.came, "the probe owed"); n != 1 {
			t.Fatalf("call %d of the probe came, want its second", n)
		}
		r.idle(t)
		if probeEvidence(r.store.State(), playbacktest.DSD128) == nil || r.b.ProbeDSDPending() {
			t.Errorf("once the session let the device go the probe owed recorded %v, still owed %v", probeEvidence(r.store.State(), playbacktest.DSD128), r.b.ProbeDSDPending())
		}
	})
	t.Run("the answers are taken before the Start takes the slot", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames)
		let := probe.hold(0)
		defer let()
		woke := newHeldOnce()
		defer woke.let()
		r := newProbeRig(t, probe, func(c *playback.Config) { c.Seams = &playback.Seams{StartWoke: woke.reach} })
		ctx := context.Background()
		r.ask(t, ctx, ready)
		within(t, probe.came, "the probe's call")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(ctx, startOf(t, 1)) }()
		within(t, r.timer.armed, "the Start's wait for the probe")
		let()
		within(t, woke.came, "the Start woken as the probe ended")
		// The Start's goroutine is the test's own: the backend's are the
		// probe's, its write and the refresh it asked for.
		r.idle(t)
		if rates, _ := accepted(r.pol, probedAt(2)); !slices.Equal(rates, []int{dsd64Frames}) {
			t.Errorf("the cache keeps DSD_U32_BE accepted at %v, want the probe's %d: a Start that had not taken the slot voided it", rates, dsd64Frames)
		}
		if probeEvidence(r.store.State(), playbacktest.DSD64) == nil || r.b.ProbeDSDPending() {
			t.Errorf("the probe recorded %v and is owed %v: a Start that had not taken the slot voided it", probeEvidence(r.store.State(), playbacktest.DSD64), r.b.ProbeDSDPending())
		}
		woke.let()
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
			t.Errorf("the DAC was opened %d times, %d of them while the probe had its node open; want once, after it", opens, beside)
		}
		if err := r.b.Release(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAStartRefusedBusyBesideAProbeLeavesItsAnswers (B762 round 2): a Start
// that waits for the probe holding the slot past ProbeWait answers the device
// busy, holding nothing, as since B4, and never took the slot: the probe's
// answers, taken once it ends, are kept, nothing is owed, and the next Start
// opens the DAC with no second probe of it. Counted at its coming, that Start
// voided the probe's answers and made a probe owed, which a DAC whose probe
// outlasts ProbeWait held past the next Start's bound again, round after
// round (the review of round 1).
func TestAStartRefusedBusyBesideAProbeLeavesItsAnswers(t *testing.T) {
	probe := newHeldProbe(dsd64Frames, dsd128Frames)
	let := probe.hold(0)
	defer let()
	r := newProbeRig(t, probe)
	ctx := context.Background()
	r.ask(t, ctx, ready)
	within(t, probe.came, "the probe's call")
	refused := make(chan error, 1)
	go func() { refused <- r.b.Start(ctx, startOf(t, 1)) }()
	within(t, r.timer.armed, "the Start's wait for the probe")
	r.timer.expire()
	if err := within(t, refused, "the Start past its wait"); !errors.Is(err, engine.ErrDeviceBusy) {
		t.Fatalf("the Start past its wait for the probe answered %v, want the device busy", err)
	}
	if err := r.b.Release(ctx, 1); err != nil {
		t.Fatal(err)
	}
	let()
	r.idle(t)
	if rates, _ := accepted(r.pol, probedAt(2)); !slices.Equal(rates, []int{dsd64Frames}) {
		t.Errorf("the cache keeps DSD_U32_BE accepted at %v, want the probe's %d: a Start that gave its wait up voided it", rates, dsd64Frames)
	}
	if probeEvidence(r.store.State(), playbacktest.DSD64) == nil {
		t.Error("the probe's driver_accepted was not written: a Start that gave its wait up voided it")
	}
	if r.b.ProbeDSDPending() {
		t.Error("a probe is owed after one whose answers were kept")
	}
	// The next Start opens the DAC, and the next ask probes nothing.
	if err := r.b.Start(ctx, startOf(t, 2)); err == nil {
		t.Fatal("the second Start answered nil: the test's DAC refuses every open")
	}
	if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
		t.Errorf("the DAC was opened %d times, %d of them beside the probe; want the second Start's open alone", opens, beside)
	}
	if err := r.b.Release(ctx, 2); err != nil {
		t.Fatal(err)
	}
	r.ask(t, ctx, ready)
	r.idle(t)
	if calls, _ := probe.counts(); calls != 1 {
		t.Errorf("the DAC was probed %d times, want once: the second Start found a probe owed", calls)
	}
}

// TestAStartAndTheProbesAnswersAreOrderedByOneStep (B762 round 3): a Start
// that waited for the probe, held once woken (StartWoke), takes the device
// slot through the real hold (b.holder, b.starts) while the probe's goroutine
// is held at one side of the step that decides its answers (takeProbe: the
// Starts compared and the cache's Record, one step under the backend's lock).
// A Start that has taken the slot by the time the step decides, the
// goroutine held just before it (ProbeDeciding), voids the answers: no
// Record in the probe's cache, no driver_accepted written, and a probe owed.
// One that takes the slot after it, the goroutine held just after
// (ProbeDecided), finds the answers recorded before it opens the DAC, and
// they stand. Round 2 compared the Starts apart from the Record, so a Start
// that took the slot between the two left the answers kept beside its open
// of the DAC (both reviews of round 2): the red test held the goroutine
// between them, a moment the step no longer has, and the seam moved to just
// before it.
func TestAStartAndTheProbesAnswersAreOrderedByOneStep(t *testing.T) {
	t.Run("the Start takes the slot just before the step decides", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames, dsd128Frames)
		let := probe.hold(0)
		defer let()
		woke, deciding := newHeldOnce(), newHeldOnce()
		defer woke.let()
		defer deciding.let()
		r := newProbeRig(t, probe, func(c *playback.Config) {
			c.Seams = &playback.Seams{StartWoke: woke.reach, ProbeDeciding: deciding.reach}
		})
		ctx := context.Background()
		r.ask(t, ctx, ready)
		within(t, probe.came, "the probe's call")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(ctx, startOf(t, 1)) }()
		within(t, r.timer.armed, "the Start's wait for the probe")
		let()
		within(t, deciding.came, "the probe's goroutine at the step that takes its answers")
		// The probe has let the slot go: the Start, let go of, takes it and
		// opens the DAC, which refuses every open here, while the step has not
		// decided.
		woke.let()
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		deciding.let()
		r.idle(t)
		if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
			t.Errorf("the DAC was opened %d times, %d of them while the probe had its node open; want once, after it", opens, beside)
		}
		if rates, kept := accepted(r.pol, probedAt(2)); kept != 0 {
			t.Errorf("the probe's cache keeps %d answers (DSD_U32_BE at %v) decided after a Start took the slot", kept, rates)
		}
		if e := probeEvidence(r.store.State(), playbacktest.DSD64); e != nil {
			t.Errorf("the player's DSD state holds %+v from answers decided after a Start took the slot", e)
		}
		if !r.b.ProbeDSDPending() {
			t.Error("no probe is owed once answers decided after a Start took the slot were dropped")
		}
		if err := r.b.Release(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the Start takes the slot just after the step decides", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames, dsd128Frames)
		let := probe.hold(0)
		defer let()
		woke, decided := newHeldOnce(), newHeldOnce()
		defer woke.let()
		defer decided.let()
		// What the probe's cache keeps as the Start opens the DAC.
		var rig *probeRig
		var atOpen []int
		r := newProbeRig(t, probe, func(c *playback.Config) {
			c.Seams = &playback.Seams{StartWoke: woke.reach, ProbeDecided: decided.reach}
			open := c.OpenSink
			c.OpenSink = func(ctx context.Context, sc alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				atOpen, _ = accepted(rig.pol, probedAt(2))
				return open(ctx, sc)
			}
		})
		rig = r
		ctx := context.Background()
		r.ask(t, ctx, ready)
		within(t, probe.came, "the probe's call")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(ctx, startOf(t, 1)) }()
		within(t, r.timer.armed, "the Start's wait for the probe")
		let()
		within(t, decided.came, "the probe's goroutine past the step that decided its answers")
		woke.let()
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		if !slices.Equal(atOpen, []int{dsd64Frames}) {
			t.Errorf("as the Start opened the DAC the probe's cache kept DSD_U32_BE accepted at %v, want the probe's %d recorded before", atOpen, dsd64Frames)
		}
		decided.let()
		r.idle(t)
		if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
			t.Errorf("the DAC was opened %d times, %d of them while the probe had its node open; want once, after it", opens, beside)
		}
		if rates, _ := accepted(r.pol, probedAt(2)); !slices.Equal(rates, []int{dsd64Frames}) {
			t.Errorf("the cache keeps DSD_U32_BE accepted at %v, want the probe's %d: answers decided before the Start took the slot", rates, dsd64Frames)
		}
		if probeEvidence(r.store.State(), playbacktest.DSD64) == nil {
			t.Error("the probe's driver_accepted was not written: answers decided before the Start took the slot stand")
		}
		if r.b.ProbeDSDPending() {
			t.Error("a probe is owed after one whose answers were decided before the Start took the slot")
		}
		if err := r.b.Release(ctx, 1); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAStartAStopAndThePublishTogether (B762 round 3): a Start that waits for
// the probe holding the device slot and the probe's publish once its call
// returns, and in every other run the Start's session's Release (the
// engine's stop), let go together with no seam ordering them, many times,
// under -race; in the runs between, the stop comes once the Start has
// answered, so the Start meets the publish alone and opens the DAC.
// Whichever order they fall in, the DAC is opened only once the probe has
// closed its node; the answers are kept whole or dropped whole (the probe's
// cache, the state's driver_accepted and the probe owed agree); and answers
// kept were decided before the Start took the slot, so a Start that opened
// the DAC found them already recorded.
func TestAStartAStopAndThePublishTogether(t *testing.T) {
	const runs = 40
	var keptRuns, openedRuns int
	defer func() { t.Logf("of %d runs, %d kept the answers and %d opened the DAC", runs, keptRuns, openedRuns) }()
	for i := range runs {
		stopTogether := i%2 == 1
		probe := newHeldProbe(dsd64Frames)
		let := probe.hold(0)
		defer let()
		var rig *probeRig
		var mu sync.Mutex
		var atOpen []int
		opened := false
		r := newProbeRig(t, probe, func(c *playback.Config) {
			open := c.OpenSink
			c.OpenSink = func(ctx context.Context, sc alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				rates, _ := accepted(rig.pol, probedAt(2))
				mu.Lock()
				atOpen, opened = rates, true
				mu.Unlock()
				return open(ctx, sc)
			}
		})
		rig = r
		ctx := context.Background()
		r.ask(t, ctx, ready)
		within(t, probe.came, "the probe's call")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(ctx, startOf(t, 1)) }()
		within(t, r.timer.armed, "the Start's wait for the probe")
		gate := make(chan struct{})
		released := make(chan error, 1)
		go func() {
			<-gate
			let()
		}()
		if stopTogether {
			go func() {
				<-gate
				released <- r.b.Release(ctx, 1)
			}()
		}
		close(gate)
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		if !stopTogether {
			released <- r.b.Release(ctx, 1)
		}
		if err := within(t, released, "the session's Release"); err != nil {
			t.Fatal(err)
		}
		r.idle(t)
		rates, _ := accepted(r.pol, probedAt(2))
		kept := slices.Equal(rates, []int{dsd64Frames})
		written := probeEvidence(r.store.State(), playbacktest.DSD64) != nil
		owed := r.b.ProbeDSDPending()
		if kept != written || kept == owed || (!kept && len(rates) != 0) {
			t.Fatalf("run %d: the cache keeps %v, driver_accepted written %v, a probe owed %v: want the answers kept whole or dropped whole", i, rates, written, owed)
		}
		if _, beside := r.sinks.counts(); beside != 0 {
			t.Fatalf("run %d: the DAC was opened %d times while the probe had its node open", i, beside)
		}
		mu.Lock()
		if kept && opened && !slices.Equal(atOpen, []int{dsd64Frames}) {
			t.Errorf("run %d: the Start opened the DAC with the cache keeping %v, and the answers stand: they were decided after it took the slot", i, atOpen)
		}
		if kept {
			keptRuns++
		}
		if opened {
			openedRuns++
		}
		mu.Unlock()
	}
}

// TestOneProbeAtATimeAndAReplugIsProbedOnceTheHeldOneEnds (B762): the
// supervisor's ask for the silent probe returns at once, and one probe runs
// at a time. A DAC unplugged and plugged in again while an older
// enumeration's probe is held in the kernel is owed a probe, the ask kept
// with its conditions, which runs as soon as the held one's goroutine has
// returned, its answers dropped: where the DAC is still bound and the device
// free, the new enumeration is probed then and its answers kept; where it is
// gone by then, nothing is probed; where a session has come to the device by
// then, the probe stays owed and runs once the session has let it go.
func TestOneProbeAtATimeAndAReplugIsProbedOnceTheHeldOneEnds(t *testing.T) {
	for _, c := range []struct {
		name string
		// then runs once the replug's ask is kept, before the held probe ends.
		then func(t *testing.T, r *probeRig) (started <-chan error)
	}{
		{"the DAC still there and free", nil},
		{"the DAC gone by then", func(t *testing.T, r *probeRig) <-chan error {
			r.bound.unplug(r.box)
			r.ask(t, context.Background(), playback.ProbeConditions{})
			return nil
		}},
		{"a session at the device by then", func(t *testing.T, r *probeRig) <-chan error {
			started := make(chan error, 1)
			go func() { started <- r.b.Start(context.Background(), startOf(t, 1)) }()
			within(t, r.timer.armed, "the Start's wait for the probe")
			return started
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames, dsd128Frames)
			let := probe.hold(0)
			defer let()
			r := newProbeRig(t, probe)
			ctx := context.Background()
			r.ask(t, ctx, ready)
			within(t, probe.came, "the probe's call")
			r.bound.replug(r.box, 3)
			r.ask(t, ctx, ready)
			// The ask is kept, and starts nothing: the backend runs the held
			// probe's goroutine and the probe's own call, nothing more. A second
			// goroutine counts from the ask, or, gone already, has had its probe
			// refused beside the held one, and no probe follows the held one.
			if n := r.b.Stats().Goroutines; n != 2 {
				t.Fatalf("%d goroutines of the backend's run once the replug's ask returned; want the held probe's and its call's alone", n)
			}
			var started <-chan error
			if c.then != nil {
				started = c.then(t, r)
			}
			if calls, _ := probe.counts(); calls != 1 {
				t.Fatalf("the probe was called %d times while the first was held; want one at a time", calls)
			}
			let()
			switch {
			case c.then == nil:
				if n := within(t, probe.came, "the replugged DAC's probe"); n != 1 {
					t.Fatalf("call %d of the probe came, want its second", n)
				}
				r.idle(t)
				if rates, kept := accepted(r.pol, probedAt(2)); kept != 0 {
					t.Errorf("the cache keeps %d answers (DSD_U32_BE at %v) of the older enumeration", kept, rates)
				}
				if rates, _ := accepted(r.pol, probedAt(3)); !slices.Equal(rates, []int{dsd128Frames}) {
					t.Errorf("the cache keeps DSD_U32_BE accepted at %v for the replugged DAC, want %d alone", rates, dsd128Frames)
				}
				if probeEvidence(r.store.State(), playbacktest.DSD128) == nil || probeEvidence(r.store.State(), playbacktest.DSD64) != nil {
					t.Errorf("the state holds DSD128 %v and DSD64 %v: want the replugged DAC's answer alone",
						probeEvidence(r.store.State(), playbacktest.DSD128), probeEvidence(r.store.State(), playbacktest.DSD64))
				}
				if r.b.ProbeDSDPending() {
					t.Error("a probe is still owed once the replugged DAC was probed")
				}
			case started == nil:
				r.idle(t)
				if calls, _ := probe.counts(); calls != 1 {
					t.Errorf("the probe was called %d times with the DAC gone, want the held call alone", calls)
				}
				if _, kept := accepted(r.pol, probedAt(2)); kept != 0 || probeEvidence(r.store.State(), playbacktest.DSD64) != nil {
					t.Errorf("the held probe's answers were kept with its DAC gone: %d in the cache, %v in the state", kept, probeEvidence(r.store.State(), playbacktest.DSD64))
				}
				if r.b.ProbeDSDPending() {
					t.Error("a probe is owed of a DAC that is gone")
				}
			default:
				if err := within(t, started, "the Start"); err == nil {
					t.Fatal("the Start answered nil: the test's DAC refuses every open")
				}
				r.idle(t)
				if calls, _ := probe.counts(); calls != 1 {
					t.Fatalf("the probe was called %d times while a session held the device, want the held call alone", calls)
				}
				if opens, beside := r.sinks.counts(); opens != 1 || beside != 0 {
					t.Errorf("the DAC was opened %d times, %d of them beside the probe's node", opens, beside)
				}
				if !r.b.ProbeDSDPending() {
					t.Fatal("the replugged DAC's probe is not owed while a session holds the device")
				}
				if err := r.b.Release(ctx, 1); err != nil {
					t.Fatal(err)
				}
				r.ask(t, ctx, ready)
				if n := within(t, probe.came, "the probe owed"); n != 1 {
					t.Fatalf("call %d of the probe came, want its second", n)
				}
				r.idle(t)
				if rates, _ := accepted(r.pol, probedAt(3)); !slices.Equal(rates, []int{dsd128Frames}) || r.b.ProbeDSDPending() {
					t.Errorf("once the session let go the cache keeps %v for the replugged DAC (owed %v), want %d", rates, r.b.ProbeDSDPending(), dsd128Frames)
				}
			}
		})
	}
}

// TestTheProbesGoroutineIsCountedFromItsAsk (B762): the goroutine that runs
// the supervisor's ask for the probe is the backend's own from the ask on,
// wherever it waits (here on a read of the binding, before the probe's own
// goroutine exists): the backend's wait (Wait) does not see it gone, nor do
// the writes of the player's DSD state (DSDWrites), since it may record what
// the DAC accepted. Its context's end drops the probe: the DAC is not asked,
// and nothing is kept.
func TestTheProbesGoroutineIsCountedFromItsAsk(t *testing.T) {
	probe := newHeldProbe(dsd64Frames)
	r := newProbeRig(t, probe)
	came, let := r.bound.holdNext()
	defer let()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ask(t, ctx, ready)
	within(t, came, "the probe's read of the binding")
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := r.b.Wait(short)
	stop()
	if err == nil {
		t.Error("the backend's wait saw no goroutine while the probe's ran")
	}
	select {
	case <-r.b.DSDWrites():
		t.Error("the writes of the player's DSD state read none under way while the probe's goroutine, which may write one, ran")
	default:
	}
	cancel()
	let()
	r.idle(t)
	select {
	case <-r.b.DSDWrites():
	default:
		t.Error("the writes read one under way once the probe's goroutine had returned")
	}
	if calls, _ := probe.counts(); calls != 0 {
		t.Errorf("a probe whose context ended asked the DAC %d times", calls)
	}
	if _, kept := accepted(r.pol, probedAt(2)); kept != 0 || r.store.Writes() != 0 {
		t.Errorf("a probe whose context ended kept %d answers and wrote the state %d times", kept, r.store.Writes())
	}
}

// TestAPanicInTheProbesWorkIsRecoveredAndTheNextAskRuns (B762): a panic in
// the probe's run on its goroutine is recovered and said, with where and its
// stack, as each goroutine of the backend's recovers its own, and the next
// ask runs the probe.
func TestAPanicInTheProbesWorkIsRecoveredAndTheNextAskRuns(t *testing.T) {
	probe := newHeldProbe(dsd64Frames)
	r := newProbeRig(t, probe)
	r.bound.panicNext()
	ctx := context.Background()
	r.ask(t, ctx, ready)
	r.idle(t)
	if lines := r.log.Lines("player: a panic was recovered in the backend; the session faults and lets go of the device"); len(lines) != 1 {
		t.Fatalf("the panic was said %q, want once", lines)
	}
	r.ask(t, ctx, ready)
	within(t, probe.came, "the probe after the panic")
	r.idle(t)
	if probeEvidence(r.store.State(), playbacktest.DSD64) == nil {
		t.Error("the probe asked after a panic recorded nothing")
	}
}

// heldControls is the box with the card's controls read through a read the
// test holds (an open or an ioctl of the control node the kernel holds): once
// armed, the next read waits until let go.
type heldControls struct {
	*playbacktest.Box
	mu   sync.Mutex
	next chan struct{}
	came chan struct{}
}

func (h *heldControls) ReadControls(card int) (alsactl.Snapshot, error) {
	h.mu.Lock()
	next, came := h.next, h.came
	h.next, h.came = nil, nil
	h.mu.Unlock()
	if next != nil {
		close(came)
		<-next
	}
	return h.Box.ReadControls(card)
}

// holdNext holds the next read until let, and says when it came.
func (h *heldControls) holdNext() (came <-chan struct{}, let func()) {
	next, c := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.next, h.came = next, c
	h.mu.Unlock()
	var once sync.Once
	return c, func() { once.Do(func() { close(next) }) }
}

// TestARefreshAskedWhileOneIsHeldRunsOnceItEnds (B762): the supervisor's
// ask for the refresh returns at once while a refresh is held reading the
// card's controls; a choice written meanwhile, asked again, is read once the
// held refresh has published, the sets published one at a time in the order
// their readings were taken: the held reading's, then the one asked while it
// was held, never the held one's last.
func TestARefreshAskedWhileOneIsHeldRunsOnceItEnds(t *testing.T) {
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	box := &heldControls{Box: playbacktest.NewBox(5)}
	b := policyBackend(t, box.Box, store, func(p *playback.DSDPolicy) { p.Box = box })
	b.RefreshDSD(true)
	first := b.Policy().Generation
	came, let := box.holdNext()
	defer let()
	ctx := context.Background()
	store.Set(playbacktest.ChoiceState(dsdstate.ModeDoP))
	refresh := func() {
		t.Helper()
		asked := make(chan struct{})
		go func() {
			defer close(asked)
			b.RequestRefreshDSD(ctx, true)
		}()
		withinFor(t, asked, askWithin, "the ask for the refresh to return")
	}
	refresh()
	within(t, came, "the refresh's read of the card's controls")
	capped := playbacktest.ChoiceState(dsdstate.ModeNative)
	capped.DACs[0].Choice.MaxBitRate = playbacktest.DSD128
	store.Set(capped)
	refresh()
	if g := b.Policy().Generation; g != first {
		t.Fatalf("generation %d was published while the refresh was held, want %d", g, first)
	}
	let()
	wctx, cancel := context.WithTimeout(ctx, watchdog)
	defer cancel()
	if err := b.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	p := b.Policy()
	if rates, transports := ratesOf(p); p.Generation != first+2 || !slices.Equal(rates, []int{playbacktest.DSD64, playbacktest.DSD128}) || slices.ContainsFunc(transports, func(s string) bool { return s != "" }) {
		t.Errorf("generation %d publishes %v (%q), want %d: the held reading's DoP set, then the capped native one asked while it was held", p.Generation, rates, transports, first+2)
	}
}
