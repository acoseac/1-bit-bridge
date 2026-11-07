package playback

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The test DAC of these in-package tests (playbacktest's, which this package
// cannot import): its unit, its altsets (native DSD_U32_BE at the three
// rates, and S24_3LE for a conversion's target), its firmware and its kernel.
const (
	choiceBCD    = "0100"
	choiceKernel = "6.12.47-v8-16k+"
)

var choiceT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func choiceUSB() alsa.USBIdentity {
	return alsa.USBIdentity{ID: alsa.USBID{Vendor: 0x1234, Product: 0x5678}, Serial: usbid.New("TESTDAC-0001"), Port: "platform-xhci-hcd.0/usb2-1", Interface: 0, DevNum: 2}
}

// choiceBox is the box with the test DAC at card 5.
type choiceBox struct{}

func (choiceBox) UnitModel(int) (alsa.UnitModel, error) {
	return alsa.UnitModel{USB: choiceUSB(), Manufacturer: "Test", Product: "Test DAC", BCDDevice: choiceBCD, Config: 1}, nil
}
func (choiceBox) CardUnit(int) (alsa.USBIdentity, bool) { return choiceUSB(), true }
func (choiceBox) PlaybackAltsets(int, int) ([]alsa.Altset, error) {
	two, one, two2, three, b24, b32 := 2, 1, 2, 3, 24, 32
	return []alsa.Altset{
		{Interface: &one, Altset: &two2, Formats: []string{"S24_3LE"}, Channels: &two, Rates: []int{44100, 88200, 176400}, Bits: &b24},
		{Interface: &one, Altset: &three, Formats: []string{"SPECIAL", "DSD_U32_BE"}, Channels: &two, Rates: []int{88200, 176400, 352800}, Bits: &b32},
	}, nil
}
func (choiceBox) USBMixer(int) ([]byte, error) { return nil, errors.New("no table") }
func (choiceBox) ReadControls(int) (alsactl.Snapshot, error) {
	return alsactl.Snapshot{}, errors.New("no controls")
}

// choiceStore is the player's DSD state in memory.
type choiceStore struct {
	mu sync.Mutex
	st dsdstate.State
}

func (c *choiceStore) State() dsdstate.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Clone()
}

func (c *choiceStore) Update(fn func(*dsdstate.State) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.st.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	c.st = next
	return nil
}

// nativeChosen is a state whose test DAC chose native, its first use
// acknowledged at every rate under its firmware.
func nativeChosen(t *testing.T) dsdstate.State {
	t.Helper()
	key, err := dsdstate.KeyOf(choiceUSB(), "Test DAC")
	if err != nil {
		t.Fatal(err)
	}
	return dsdstate.State{DefaultMode: dsdstate.ModeAuto, DACs: []dsdstate.DAC{{
		Key:      key,
		Choice:   &dsdstate.Choice{Mode: dsdstate.ModeNative, Revision: 1, SetAt: choiceT0, SetBy: dsdstate.ByApp},
		FirstUse: []dsdstate.FirstUse{{Transport: dsdstate.TransportNative, BCDDevice: choiceBCD, AcknowledgedAt: choiceT0}},
	}}}
}

// What a choice write asks of the sessions (B728 B9; binding note 3): the
// classification it makes of each DSD occurrence against the set it would
// publish is the one reconciliation acts by, and an occurrence admitted
// beside a write in flight is held where the write would change its plan.

// choiceSink is a sink that answers its position alone.
type choiceSink struct{ pos alsapcm.Position }

func (c choiceSink) Write(context.Context, []byte) (int, error) { panic("no write") }
func (c choiceSink) Cut(int64) (bool, error)                    { panic("no cut") }
func (c choiceSink) Pause() error                               { panic("no pause") }
func (c choiceSink) Resume() error                              { panic("no resume") }
func (c choiceSink) Drain(context.Context) error                { panic("no drain") }
func (c choiceSink) Drop() error                                { return nil }
func (c choiceSink) Close() error                               { return nil }
func (c choiceSink) Params() alsapcm.Params                     { return alsapcm.Params{} }
func (c choiceSink) Position() alsapcm.Position                 { return c.pos }
func (c choiceSink) Changed() <-chan struct{}                   { return nil }

// replans records what reconciliation asked of the engine.
type replans struct {
	engine.Events
	mu  sync.Mutex
	ids []engine.OccurrenceID
}

func (r *replans) Replan(_ engine.SessionID, o engine.OccurrenceID, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, o)
}

func (r *replans) asked() []engine.OccurrenceID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.OccurrenceID(nil), r.ids...)
}

// plainEvents is an Events that hands nothing off again: an engine with no
// re-plan support.
type plainEvents struct{ engine.Events }

// setOf is the effective set over st for the test DAC on the box, computed
// as a refresh computes one.
func setOf(t *testing.T, b *Backend, st dsdstate.State) *dsdSet {
	t.Helper()
	r, why := b.readBox()
	if r == nil {
		t.Fatalf("the box did not read: %s", why)
	}
	d, _ := r.setFor(st)
	return d
}

// choiceSessionBackend is a backend over the test DAC whose state chose native
// at every rate, its set published.
func choiceSessionBackend(t *testing.T) (*Backend, *choiceStore) {
	t.Helper()
	store := &choiceStore{st: nativeChosen(t)}
	pol := &DSDPolicy{Store: store, Kernel: choiceKernel, Box: choiceBox{}, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: choiceUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return choiceT0 }}
	b, err := New(Config{Resolver: nativeFilesForRefusal{}, Locator: refuseLocator{}, DSD: pol})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b, store
}

// openedOcc is an occurrence of DSD64 whose open has ended, admitted under
// the set b publishes.
func openedOcc(b *Backend, id engine.OccurrenceID) *occ {
	done := make(chan struct{})
	close(done)
	cur := b.dsd.Load()
	return &occ{id: id, done: done, admitted: cur, policyPlan: cur.set.At(dsdstate.DSD64).Plan,
		nativeFormat: format.NativeDSD{BitRateHz: dsdstate.DSD64, Channels: 2}}
}

// TestTheWriteAsksWhatReconciliationDoes (binding note 3; the design's
// sections 3.5 and 7.2): whether a write would interrupt what plays is the
// backend's whole classification of what reconciliation would do with each
// DSD occurrence against the set the write would publish, policy.Reconcile
// and the rest: a re-plan of a session's first occurrence awaiting its first
// submission, or of an occurrence of a session being released, is a
// revocation, and so is a re-plan where the engine hands nothing off again;
// an occurrence played out, still opening, dropped or PCM is not judged. And
// reconciliation acts as the write was told it would.
func TestTheWriteAsksWhatReconciliationDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setUp shapes the session and its occurrence.
		setUp func(s *session, o *occ)
		// plain is an engine with no re-plan support.
		plain bool
		// sameSet is a write that keeps native at every rate.
		sameSet    bool
		interrupts bool
		// want is what reconciliation then does: "keep", "replan", "revoke"
		// or "" for nothing.
		want string
	}{
		{"a session's first occurrence awaiting its first submission", func(s *session, o *occ) { o.leading = true }, false, false, true, "revoke"},
		{"a successor not yet submitted", func(s *session, o *occ) {}, false, false, false, "replan"},
		{"a successor whose frames are in its sink", func(s *session, o *occ) {
			o.out, o.written = &output{sink: choiceSink{}}, 4096
		}, false, false, true, "replan"},
		{"a successor a frame of which is the kernel's", func(s *session, o *occ) {
			o.out, o.written = &output{sink: choiceSink{pos: alsapcm.Position{Written: 8}}}, 4096
		}, false, false, true, "revoke"},
		{"the occurrence heard", func(s *session, o *occ) {
			o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
		}, false, false, true, "revoke"},
		{"a session being released", func(s *session, o *occ) { s.releasing = true }, false, false, true, "revoke"},
		{"an engine that hands nothing off again", func(s *session, o *occ) {}, true, false, true, "revoke"},
		{"an occurrence the new set keeps", func(s *session, o *occ) {
			o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
		}, false, true, false, "keep"},
		{"an occurrence played out", func(s *session, o *occ) {
			o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 4096}}, drained: true}, 4096, true
		}, false, false, false, ""},
		{"an occurrence still opening", func(s *session, o *occ) { o.done = make(chan struct{}) }, false, false, false, ""},
		{"an occurrence dropped", func(s *session, o *occ) { o.dropped = true }, false, false, false, ""},
		{"a PCM occurrence", func(s *session, o *occ) { o.admitted = nil }, false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, store := choiceSessionBackend(t)
			cur := b.dsd.Load()
			mode := dsdstate.ModeConvert
			if tc.sameSet {
				mode = dsdstate.ModeNative
			}
			st := store.State()
			st.DACs[0].Choice.Mode, st.DACs[0].Choice.MaxBitRate = mode, 0
			next := setOf(t, b, st)
			if tc.sameSet {
				st.DACs[0].Choice.MaxBitRate = dsdstate.DSD256
				next = setOf(t, b, st)
			}
			s := b.session(1)
			defer s.cancel()
			ev := &replans{}
			s.ev = ev
			if tc.plain {
				s.ev = plainEvents{}
			}
			o := openedOcc(b, 2)
			tc.setUp(s, o)
			s.chain = []*occ{o}
			if got := b.writeInterrupts(cur, next); got != tc.interrupts {
				t.Errorf("the write would interrupt: %v, want %v", got, tc.interrupts)
			}
			s.reconcileDSD(next)
			got := ""
			switch {
			case s.nativeRevoked:
				got = "revoke"
			case len(ev.asked()) == 1:
				got = "replan"
			case o.admitted == next:
				got = "keep"
			}
			if got != tc.want {
				t.Errorf("reconciliation did %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAWriteCountsWhatItInterruptsNotWhatTheBoxDoes: what a write would
// interrupt is what the set it would publish interrupts and the set the state
// gives without it does not: a change of the box that revokes an occurrence
// anyway (a safety change, which asks no one) is no interruption of the
// write's.
func TestAWriteCountsWhatItInterruptsNotWhatTheBoxDoes(t *testing.T) {
	b, store := choiceSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	s.ev = &replans{}
	o := openedOcc(b, 2)
	o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
	s.chain = []*occ{o}
	st := store.State()
	st.DACs[0].Choice.Mode = dsdstate.ModeOff
	off := setOf(t, b, st)
	st.DACs[0].Choice.Mode = dsdstate.ModeConvert
	convert := setOf(t, b, st)
	if !b.writeInterrupts(b.dsd.Load(), convert) {
		t.Error("a write that takes native away from the occurrence heard would not interrupt it")
	}
	if b.writeInterrupts(off, convert) {
		t.Error("a write was counted as interrupting an occurrence the set without it revokes anyway")
	}
}

// TestAnOccurrenceWhoseRateTheWriteChangesWaitsAtTheEndOfItsOpen (binding note
// 3): while a write is pending, the step that ends an occurrence's open takes
// it only where the set published and the set pending play its rate alike;
// one the write would change waits, outside the session's lock, for the
// write to settle, so the write's classification either judged it or it sees
// the write; one the write leaves alone does not wait.
func TestAnOccurrenceWhoseRateTheWriteChangesWaitsAtTheEndOfItsOpen(t *testing.T) {
	b, store := choiceSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	st := store.State()
	st.DACs[0].Choice.MaxBitRate = dsdstate.DSD128
	capped := setOf(t, b, st)
	p := &pendingChoice{set: capped, done: make(chan struct{})}
	b.pending.Store(p)
	// DSD64 plays natively in both.
	o := openedOcc(b, 2)
	s.lockSettled(context.Background(), o)
	s.mu.Unlock()
	// DSD256 does not: it waits.
	o = openedOcc(b, 3)
	o.nativeFormat.BitRateHz = dsdstate.DSD256
	o.policyPlan = b.dsd.Load().set.At(dsdstate.DSD256).Plan
	waiting := make(chan engine.OccurrenceID, 1)
	b.cfg.Seams = &Seams{ChoiceAwaited: func(id engine.OccurrenceID) { waiting <- id }}
	settled := make(chan struct{})
	go func() {
		s.lockSettled(context.Background(), o)
		s.mu.Unlock()
		close(settled)
	}()
	select {
	case id := <-waiting:
		if id != o.id {
			t.Fatalf("occurrence %d waits, want %d", id, o.id)
		}
	case <-settled:
		t.Fatal("an occurrence whose rate the pending write changes did not wait for it")
	case <-time.After(30 * time.Second):
		t.Fatal("an occurrence whose rate the pending write changes neither waited nor ended its open")
	}
	// The session's lock stays free while it waits.
	locked := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(30 * time.Second):
		t.Fatal("an occurrence waiting for a write holds its session's lock")
	}
	b.pending.CompareAndSwap(p, nil)
	close(p.done)
	select {
	case <-settled:
	case <-time.After(30 * time.Second):
		t.Fatal("the occurrence still waits once the write settled")
	}
}

// TestTheProducerHoldsAnOccurrenceAPendingWriteWouldReplan (binding note 3): an
// opened occurrence not yet in a sink, at a rate a pending write would change,
// is held back from the sink until the write settles, so the write's
// classification (a re-plan, which interrupts nothing) still holds when it
// publishes.
func TestTheProducerHoldsAnOccurrenceAPendingWriteWouldReplan(t *testing.T) {
	b, store := choiceSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	st := store.State()
	st.DACs[0].Choice.Mode = dsdstate.ModeConvert
	p := &pendingChoice{set: setOf(t, b, st), done: make(chan struct{})}
	b.pending.Store(p)
	o := openedOcc(b, 2)
	o.plan = &engine.NativePlan{Tuple: engine.NativeTuple{Format: "DSD_U32_BE", BitRate: dsdstate.DSD64}, PolicyID: "n"}
	s.chain = []*occ{o}
	if st := s.next(); st.kind != stepWait || st.wait != p.done {
		t.Fatalf("the producer's step beside the pending write: %+v, want a wait for the write", st)
	}
	b.pending.CompareAndSwap(p, nil)
	close(p.done)
	if st := s.next(); st.kind == stepWait {
		t.Fatalf("the producer still waits once the write settled: %+v", st)
	}
}

// TestTheChoiceWritesLockOrderHolds (binding note 3): a choice write takes the
// writes' turn, then the refreshes' lock, then each session's; a refresh the
// refreshes' lock, then each session's; the state's store is taken alone, its
// writes with no lock of the backend's held; under -race and together, writes,
// refreshes, evidence writes, reconciliations and reads all finish.
func TestTheChoiceWritesLockOrderHolds(t *testing.T) {
	b, store := choiceSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	s.ev = &replans{}
	o := openedOcc(b, 2)
	o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
	s.chain = []*occ{o}
	key := b.dsdPub.Load().view.DAC.Key
	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			modes := []string{dsdstate.ModeNative, dsdstate.ModeConvert, dsdstate.ModeAuto}
			for j := range 25 {
				rev := b.Policy().DSD.DAC.Choice.Revision
				b.ChooseDSD(context.Background(), ChoiceRequest{DACKey: key, Interrupt: true,
					Choice: policy.Choice{BaseRevision: rev, Mode: modes[(i+j)%3], FirstUseAcknowledged: true, By: dsdstate.ByApp}})
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				b.RefreshDSD(true)
				_, _ = b.Facts()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			_ = store.Update(func(st *dsdstate.State) error {
				for i := range st.DACs {
					st.DACs[i].Evidence = []dsdstate.Evidence{{Transport: dsdstate.TransportNative, Format: "DSD_U32_BE", BitRate: dsdstate.DSD64,
						BCDDevice: choiceBCD, StreamingObservedAt: choiceT0}}
				}
				return nil
			})
			s.mu.Lock()
			_ = s.occurrenceFactsLocked(o)
			s.mu.Unlock()
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("writes, refreshes and evidence writes together did not finish: a lock order deadlocked")
	}
}
