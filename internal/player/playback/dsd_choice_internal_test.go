package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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

// choiceStore is the player's DSD state in memory. land, when set, is what
// its next Update answers once its change is made, as dsdstate.Store answers
// a write whose rename landed and whose barrier failed: the change is not
// held, and every later Update is fenced (dsdstate.ErrLandedNotHeld), which
// Landed reports.
type choiceStore struct {
	mu     sync.Mutex
	st     dsdstate.State
	land   error
	landed error
}

func (c *choiceStore) State() dsdstate.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Clone()
}

func (c *choiceStore) Update(fn func(*dsdstate.State) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.landed != nil {
		return dsdstate.ErrLandedNotHeld
	}
	next := c.st.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	if c.land != nil {
		c.landed, c.land = c.land, nil
		return c.landed
	}
	c.st = next
	return nil
}

// Landed is the write that landed and fences the store, nil while none did
// (dsdstate.Store.Landed).
func (c *choiceStore) Landed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.landed
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

// driftBox is the box with the test DAC whose playback altsets, once armed,
// lose the native DSD one at every other read, the first one first: a
// reading of the DAC that never agrees with the one before it.
type driftBox struct {
	choiceBox
	armed atomic.Bool
	reads atomic.Int32
}

func (d *driftBox) PlaybackAltsets(card, device int) ([]alsa.Altset, error) {
	alts, err := d.choiceBox.PlaybackAltsets(card, device)
	if d.armed.Load() && d.reads.Add(1)%2 == 1 {
		return alts[:1], err
	}
	return alts, err
}

// choiceSessionBackend is a backend over the test DAC whose state chose native
// at every rate, its set published, edit applied to the policy first.
func choiceSessionBackend(t *testing.T, edit ...func(*DSDPolicy)) (*Backend, *choiceStore) {
	t.Helper()
	store := &choiceStore{st: nativeChosen(t)}
	pol := &DSDPolicy{Store: store, Kernel: choiceKernel, Box: choiceBox{}, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: choiceUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return choiceT0 }}
	for _, e := range edit {
		e(pol)
	}
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

// heardSession is a session of b's whose one occurrence, DSD64 natively under
// the set b publishes, is heard: half its frames played.
func heardSession(t *testing.T, b *Backend) (*session, *occ) {
	t.Helper()
	s := b.session(1)
	t.Cleanup(s.cancel)
	s.ev = &replans{}
	o := openedOcc(b, 2)
	o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
	s.chain = []*occ{o}
	return s, o
}

// TestTheWritesChecksAreAnsweredInTheirOrder (B728 B9 round 2, point 3; the
// design's section 7.2): what a write would interrupt is its eighth check,
// answered inside the state's one write, after the state's own checks made on
// the state that write is made from, and only where those and the write's
// reading still hold; the classification it answers by is made before, with no
// lock of the store's held. A write that would interrupt the occurrence heard,
// the revision moved in its window by another writer, is stale; beside an
// earlier write that landed and is not known to be durable, which fences every
// write until the next start, one that would interrupt is persist_uncertain, no
// check of the state's made, and so is one whose reading of the DAC never
// agrees with the set published, which would otherwise go round to its bound.
// Neither stops what plays.
func TestTheWritesChecksAreAnsweredInTheirOrder(t *testing.T) {
	write := func(b *Backend, c policy.Choice) ChoiceOutcome {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.By = dsdstate.ByApp
		return b.ChooseDSD(ctx, ChoiceRequest{DACKey: b.dsdPub.Load().view.DAC.Key, Choice: c}).Outcome
	}
	t.Run("a revision moved in the write's window", func(t *testing.T) {
		b, store := choiceSessionBackend(t)
		s, _ := heardSession(t, b)
		var once sync.Once
		b.cfg.Seams = &Seams{ChoicePending: func() {
			once.Do(func() {
				if err := store.Update(func(st *dsdstate.State) error {
					st.DACs[0].Choice.Revision, st.DACs[0].Choice.SetBy = 2, dsdstate.ByMigration
					return nil
				}); err != nil {
					t.Error(err)
				}
			})
		}}
		if got := write(b, policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeConvert}); got != ChoiceStale {
			t.Fatalf("a write that would interrupt, its revision moved in its window: %s, want stale", got)
		}
		if s.nativeRevoked {
			t.Error("a stale write stopped what plays")
		}
	})
	t.Run("beside an earlier write that landed", func(t *testing.T) {
		b, store := choiceSessionBackend(t)
		s, _ := heardSession(t, b)
		store.mu.Lock()
		store.land = fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)
		store.mu.Unlock()
		// The cap at DSD256 plays what native at every rate plays: it interrupts
		// nothing, and lands.
		if got := write(b, policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, MaxBitRate: dsdstate.DSD256}); got != ChoicePersistUncertain {
			t.Fatalf("the first write: %s, want persist_uncertain", got)
		}
		if got := write(b, policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeConvert}); got != ChoicePersistUncertain {
			t.Fatalf("a write that would interrupt, beside the fence: %s, want persist_uncertain", got)
		}
		if s.nativeRevoked {
			t.Error("a fenced write stopped what plays")
		}
	})
	t.Run("beside an earlier write that landed, the DAC's reading drifting", func(t *testing.T) {
		db := &driftBox{}
		b, store := choiceSessionBackend(t, func(p *DSDPolicy) { p.Box = db })
		store.mu.Lock()
		store.land = fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)
		store.mu.Unlock()
		if got := write(b, policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeNative, MaxBitRate: dsdstate.DSD256}); got != ChoicePersistUncertain {
			t.Fatalf("the first write: %s, want persist_uncertain", got)
		}
		db.armed.Store(true)
		if got := write(b, policy.Choice{BaseRevision: 1, Mode: dsdstate.ModeConvert}); got != ChoicePersistUncertain {
			t.Fatalf("a write beside the fence, its reading of the DAC drifting: %s, want persist_uncertain, whatever the DAC reads: no write is made", got)
		}
	})
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

// TestTheProducerHoldsAnOccurrenceAPendingWriteWouldReplan (binding note 3;
// B728 B9 round 2): an opened occurrence not yet in a sink, at a rate a
// pending write would change, is held back from every sink until the write
// settles, so the write's classification (a re-plan, which interrupts
// nothing) still holds when it publishes. With no sink to join, the producer
// takes the transport's step (transition), which drains the sink before it
// and opens none for the occurrence until the write settles; out of the sink
// it would join it waits, the reporter waking it, while that sink holds more
// than its floor of lookahead, and at the floor takes the transport's step,
// so the sink plays out rather than run dry; once the write settled it joins.
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
	o.transport = nativeTransport(*o.plan)
	s.chain = []*occ{o}
	if st := s.next(); st.kind != stepTransition {
		t.Fatalf("the producer's step for an occurrence with no sink to join, beside the pending write: %+v, want the transport's", st)
	}
	// The sink it would join, DSD64's 88,200 frames a second, holding lookahead
	// of it: its floor is 100 ms, 8,820 frames.
	ahead := func(frames int64) {
		s.cur = &output{sink: choiceSink{pos: alsapcm.Position{Submitted: 88200, Played: 88200 - frames}}, transport: o.transport, policyID: "n"}
	}
	ahead(88200)
	if st := s.next(); st.kind != stepWait || st.wait != p.done || !s.awaitingWrite {
		t.Fatalf("the producer's step above the sink's floor: %+v (the reporter wakes it: %v), want a wait for the write", st, s.awaitingWrite)
	}
	ahead(8820)
	if st := s.next(); st.kind != stepTransition || s.awaitingWrite {
		t.Fatalf("the producer's step at the sink's floor: %+v, want the transport's", st)
	}
	ahead(88200)
	b.pending.CompareAndSwap(p, nil)
	close(p.done)
	if st := s.next(); st.kind != stepWrite || st.o != o {
		t.Fatalf("the producer's step once the write settled: %+v, want the occurrence joining its sink", st)
	}
}

// TestAWriteThatPanicsLeavesNothingPending (binding note 3): a choice write
// that panics once its set is pending, which the API's recoverer answers,
// settles that set all the same and gives the writes' turn back, so no
// occurrence waits for a write that will not publish and the next write is
// made: before the state's write nothing is written, and after it the next
// refresh publishes what was. After its publish (B728 B9 round 18), the set
// is published and the next refresh finds it the same, and reconciles it,
// none of its reconciliations having returned (dsdJudged).
func TestAWriteThatPanicsLeavesNothingPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seams func(panicNow func()) *Seams
		// wrote is the mode the state holds after the panic, and lost a set
		// published whose reconciliation did not return.
		wrote string
		lost  bool
	}{
		{"before the state's write", func(panicNow func()) *Seams { return &Seams{ChoicePending: panicNow} }, dsdstate.ModeNative, false},
		{"after the state's write", func(panicNow func()) *Seams { return &Seams{ChoiceWritten: func(error) { panicNow() }} }, dsdstate.ModeConvert, false},
		{"after its publish", func(panicNow func()) *Seams { return &Seams{Published: panicNow} }, dsdstate.ModeConvert, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, store := choiceSessionBackend(t)
			key := b.dsdPub.Load().view.DAC.Key
			write := func(mode string, base uint64) ChoiceOutcome {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				return b.ChooseDSD(ctx, ChoiceRequest{DACKey: key, Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}}).Outcome
			}
			var seen *pendingChoice
			b.cfg.Seams = tc.seams(func() {
				seen = b.pending.Load()
				panic("the test's panic inside the write")
			})
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the write did not panic: the seam did not reach it")
					}
				}()
				write(dsdstate.ModeConvert, 1)
			}()
			b.cfg.Seams = nil
			if seen == nil {
				t.Fatal("no set was pending when the write panicked")
			}
			if b.pending.Load() != nil {
				t.Error("a set is still pending after the write panicked")
			}
			select {
			case <-seen.done:
			default:
				t.Error("what waits for the write that panicked was never woken")
			}
			own, err := dsdstate.KeyOf(choiceUSB(), "Test DAC")
			if err != nil {
				t.Fatal(err)
			}
			if mode, _, _ := store.State().ChoiceFor(own); mode != tc.wrote {
				t.Fatalf("the state holds %s after the panic, want %s", mode, tc.wrote)
			}
			judged := func() bool {
				b.dsdMu.Lock()
				defer b.dsdMu.Unlock()
				return b.dsdJudged == b.dsd.Load()
			}
			if got := judged(); got == tc.lost {
				t.Errorf("before the next refresh, the set published reconciled %v, want %v", got, !tc.lost)
			}
			b.RefreshDSD(true)
			if !judged() {
				t.Error("the refresh after the panic left the set published unreconciled")
			}
			pub := b.Policy().DSD.DAC.Choice
			if pub.Mode != tc.wrote {
				t.Errorf("the refresh after the panic publishes %s, want %s", pub.Mode, tc.wrote)
			}
			if got := write(dsdstate.ModeOff, pub.Revision); got != ChoiceWritten {
				t.Errorf("the write after the one that panicked: %s, want written, the turn given back", got)
			}
		})
	}
}

// panicReplans is replans whose hand-off panics once counted: a reconciliation
// that panics after it has let the session's lock go.
type panicReplans struct{ replans }

func (r *panicReplans) Replan(s engine.SessionID, o engine.OccurrenceID, why string) {
	r.replans.Replan(s, o, why)
	panic("the test's panic in a reconciliation's hand-off")
}

// TestALostReconciliationIsTriedOnceMore (B728 B9 rounds 18 and 19): a
// refresh whose reconciliation does not complete, here a hand-off that panics
// inside it once the session's lock is let go, leaves its set published and
// the occurrence it was judging unjudged, and panics into no caller; the next
// refresh, finding the set the same, reconciles it once more, and that
// retry's failure stops the session (backend_failed) and records the set
// judged, so a reconciliation that panics again is not run by every refresh
// after it.
func TestALostReconciliationIsTriedOnceMore(t *testing.T) {
	b, store := choiceSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	ev := &panicReplans{}
	s.ev = ev
	s.chain = []*occ{openedOcc(b, 2)}
	refresh := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		b.RefreshDSD(true)
		return false
	}
	if err := store.Update(func(st *dsdstate.State) error {
		st.DACs[0].Choice.Mode = dsdstate.ModeConvert
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got []bool
	for range 3 {
		got = append(got, refresh())
	}
	if want := []bool{false, false, false}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the refreshes panicked %v, want %v: a reconciliation's panic reaches no refresh's caller", got, want)
	}
	if n := len(ev.asked()); n != 2 {
		t.Errorf("the occurrence was handed off again %d times, want 2: the publish's reconciliation and the next refresh's", n)
	}
	if f, _ := faultOf(s); f == nil || f.Reason != ReasonFailed {
		t.Errorf("the session's fault once the retry failed: %+v, want backend_failed", f)
	}
}

// TestTheChoiceWritesLockOrderHolds (binding note 3): a choice write takes the
// writes' turn, then the refreshes' lock, then each session's; a refresh the
// refreshes' lock, then each session's; the state's store is taken alone, its
// writes with no lock of the backend's held, nor any taken inside them; under
// -race and together, writes, refreshes, evidence writes, reconciliations and
// reads all finish. A reader that takes the store's lock under a session's
// (B728 B9 round 2, point 7) stands in for any path that would, so a write
// that took a session's lock inside the state's write (what it would interrupt
// classified in its closure) deadlocks here.
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
	writers := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-writers:
				return
			default:
			}
			s.mu.Lock()
			_ = store.State()
			s.mu.Unlock()
		}
	}()
	go func() {
		wg.Wait()
		close(writers)
		readers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("writes, refreshes and evidence writes together did not finish: a lock order deadlocked")
	}
}
