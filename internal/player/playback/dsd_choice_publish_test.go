package playback_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A choice's write publishes what it read (B728 B9 PR 3): every outcome after
// the write read the DAC publishes that reading, from the state its decision
// read, and is answered with the pair it published; no refresh reads or
// publishes between the reading and the publication, nor between the
// publication and the answer; a state another writer wrote meanwhile is
// published after it; the cause is kept where it occurs; and every
// publication, a new set or its read side alone, moves the publication count.

// moveRig is a backend over a box holding the test DAC at cards 5 and 6,
// bound at the card card holds (0 for none), the unit unit holds, its state
// in memory, its seams the test's, its set published once.
type moveRig struct {
	b     *playback.Backend
	box   *playbacktest.Box
	store *playbacktest.MemStore
	card  atomic.Int32
	unit  atomic.Pointer[alsa.UnitModel]
}

func newMoveRig(t *testing.T, st dsdstate.State, seams *playback.Seams, edit ...func(*playback.DSDPolicy)) *moveRig {
	t.Helper()
	r := &moveRig{box: playbacktest.NewBox(5), store: playbacktest.NewMemStore(st)}
	r.box.SetUnit(6, playbacktest.DACUnit())
	r.box.SetAltsets(6, 0, playbacktest.AllAltsets()...)
	r.box.SetMixer(6, []byte("USB Mixer: usb_id=0x12345678, ctrlif=0, ctlerr=0\nCard: Test DAC\n"))
	r.box.SetControls(6)
	u := playbacktest.DACUnit()
	r.unit.Store(&u)
	r.card.Store(5)
	p := playbacktest.Policy(5, r.box, r.store)
	p.Log = &choiceLog{}
	p.Bound = func() (playback.BoundDAC, bool) {
		c := r.card.Load()
		if c == 0 {
			return playback.BoundDAC{}, false
		}
		return playback.BoundDAC{Card: int(c), Identity: r.unit.Load().USB, Enumeration: "test/2"}, true
	}
	for _, e := range edit {
		e(p)
	}
	b, err := playback.New(playback.Config{Resolver: noContent{}, Locator: cardFive{}, DSD: p, Seams: seams})
	if err != nil {
		t.Fatal(err)
	}
	r.b = b
	b.RefreshDSD(true)
	return r
}

// move binds m at card c, the unit the box holds there.
func (r *moveRig) move(c int, m alsa.UnitModel) {
	r.box.SetUnit(c, m)
	r.unit.Store(&m)
	r.card.Store(int32(c))
}

// optionIn is the read side's option for mode in p.
func optionIn(t *testing.T, p playback.PlayerPolicy, mode string) playback.DSDOption {
	t.Helper()
	for _, o := range dacOf(t, p).Options {
		if o.Mode == mode {
			return o
		}
	}
	t.Fatalf("the options %+v hold no %s", dacOf(t, p).Options, mode)
	return playback.DSDOption{}
}

// TestEveryOutcomeAfterTheReadingPublishesThatReading (B728 B9 PR 3): each
// outcome a write decides once it has read the DAC publishes that reading, from
// the state its decision read, and is answered with the pair it published,
// which is the pair published then: the DAC is moved to another card before the
// write reads it, where no refresh runs, and the answer and the epoch published
// are both the reading's, at the next generation. A DAC gone publishes no DAC,
// another model the other model's key, another maker's unit its own token, and
// a landed write the set held to what both states play alike. A change of the
// box after the reading, where the write reaches the state's write, is not
// published by it: its publication is the reading it decided on.
func TestEveryOutcomeAfterTheReadingPublishesThatReading(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	otherModel := playbacktest.DACUnit()
	otherModel.USB.ID.Product, otherModel.Product = 0x5679, "Other DAC"
	otherMaker := playbacktest.DACUnit()
	otherMaker.Manufacturer = "Other Labs"
	landed := fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)
	uncertain := func(t *testing.T, p playback.PlayerPolicy) {
		t.Helper()
		if p.DSD == nil || !containsRefusal(p.DSD.Refused, playbacktest.DSD256, string(policy.ReasonChoiceUncertain)) {
			t.Errorf("the published refusals %+v, want DSD256 refused as dsd_choice_uncertain", p.DSD)
		}
	}
	for _, tc := range []struct {
		name string
		st   dsdstate.State
		// edit configures the policy; prepare the store before the write.
		edit    func(*playback.DSDPolicy)
		prepare func(t *testing.T, r *moveRig)
		// to is where the DAC is before the write reads it: card 6 with the
		// test DAC unless it says another unit, or none.
		to   func(r *moveRig)
		mode string
		base uint64
		ask  func(*playback.ChoiceRequest)
		want playback.ChoiceOutcome
		// after says the box changes after the reading, before the
		// publication: the write reaches the state's write.
		after bool
		check func(t *testing.T, r *moveRig, req playback.ChoiceRequest, p playback.PlayerPolicy)
	}{
		{name: "dac_unavailable", st: native, to: func(r *moveRig) { r.card.Store(0) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoiceDACUnavailable,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) {
				if p.DSD == nil || p.DSD.DAC != nil {
					t.Errorf("the epoch publishes %+v, want no DAC", p.DSD)
				}
			}},
		{name: "dac_changed by the key", st: native, to: func(r *moveRig) { r.move(6, otherModel) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoiceDACChanged,
			check: func(t *testing.T, _ *moveRig, req playback.ChoiceRequest, p playback.PlayerPolicy) {
				if d := dacOf(t, p); d.Key == req.DACKey {
					t.Errorf("the epoch publishes the key %s, the request's: want the other model's", d.Key)
				}
			}},
		{name: "dac_changed by the token", st: native, to: func(r *moveRig) { r.move(6, otherMaker) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoiceDACChanged,
			check: func(t *testing.T, _ *moveRig, req playback.ChoiceRequest, p playback.PlayerPolicy) {
				if d := dacOf(t, p); d.Key != req.DACKey || d.Token == req.DACToken {
					t.Errorf("the epoch publishes the key %s and the token %s, want the request's key and another unit's token", d.Key, d.Token)
				}
			}},
		{name: "state_unreadable, no state", st: native, edit: func(p *playback.DSDPolicy) { p.Store = nil }, mode: dsdstate.ModeOff, base: 0, want: playback.ChoiceStateUnreadable},
		{name: "persist_uncertain, the fence", st: native, edit: func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) },
			prepare: func(t *testing.T, r *moveRig) {
				r.store.Land(landed)
				wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1, capped)), playback.ChoicePersistUncertain)
			}, mode: dsdstate.ModeOff, base: 1, want: playback.ChoicePersistUncertain,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) { uncertain(t, p) }},
		{name: "stale", st: native, mode: dsdstate.ModeOff, base: 0, want: playback.ChoiceStale, after: true,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) {
				if got := revisionOf(t, p); got != 1 {
					t.Errorf("the stale answer publishes revision %d, want 1", got)
				}
			}},
		{name: "mode_not_offered", st: dsdstate.Defaults(), to: func(r *moveRig) {
			r.box.SetAltsets(6, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
			r.move(6, playbacktest.DACUnit())
		}, mode: dsdstate.ModeNative, base: 0, ask: acknowledged, want: playback.ChoiceModeNotOffered,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) {
				if optionIn(t, p, dsdstate.ModeNative).Offered {
					t.Error("the refusal publishes native offered: it was decided on a reading that offers none")
				}
			}},
		{name: "dop_confirmation_required", st: dsdstate.Defaults(), mode: dsdstate.ModeDoP, base: 0, ask: acknowledged, want: playback.ChoiceConfirmationRequired, after: true,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) {
				if o := optionIn(t, p, dsdstate.ModeDoP); !o.Offered || !o.NeedsConfirmation {
					t.Errorf("DoP's option %+v, want offered and needing the confirmation", o)
				}
			}},
		{name: "first_use_unacknowledged", st: dsdstate.Defaults(), mode: dsdstate.ModeDoP, base: 0, ask: confirmed, want: playback.ChoiceFirstUseUnacknowledged, after: true},
		{name: "state_unreadable, the file refused in memory", st: native,
			prepare: func(_ *testing.T, r *moveRig) {
				r.store.Fail(fmt.Errorf("%w: a named pipe is there", dsdstate.ErrNotWritable))
			}, mode: dsdstate.ModeOff, base: 1, want: playback.ChoiceStateUnreadable, after: true},
		{name: "persist_uncertain, the store's fence", st: native,
			prepare: func(_ *testing.T, r *moveRig) { r.store.Fail(dsdstate.ErrLandedNotHeld) },
			mode:    dsdstate.ModeOff, base: 1, want: playback.ChoicePersistUncertain, after: true},
		{name: "persist_uncertain, a write that landed", st: native, edit: func(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) },
			prepare: func(_ *testing.T, r *moveRig) { r.store.Land(landed) },
			mode:    dsdstate.ModeNative, base: 1, ask: capped, want: playback.ChoicePersistUncertain, after: true,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) { uncertain(t, p) }},
		{name: "persist_failed", st: native,
			prepare: func(_ *testing.T, r *moveRig) { r.store.Fail(errors.New("no space left on device")) },
			mode:    dsdstate.ModeOff, base: 1, want: playback.ChoicePersistFailed, after: true},
		{name: "written", st: native, mode: dsdstate.ModeConvert, base: 1, want: playback.ChoiceWritten, after: true,
			check: func(t *testing.T, _ *moveRig, _ playback.ChoiceRequest, p playback.PlayerPolicy) {
				if c := dacOf(t, p).Choice; c.Mode != dsdstate.ModeConvert || c.Revision != 2 {
					t.Errorf("the answer publishes the choice %+v, want convert at revision 2", c)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r *moveRig
			// armed is the case's own write: a write the case's setup makes
			// (the fence's landed write) moves nothing.
			var armed atomic.Bool
			seams := &playback.Seams{
				ChoiceLeased: func() {
					if !armed.Load() {
						return
					}
					if tc.to != nil {
						tc.to(r)
						return
					}
					r.move(6, playbacktest.DACUnit())
				},
				ChoiceWritten: func(error) {
					if armed.Load() && tc.after {
						r.box.SetAltsets(6, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
					}
				},
			}
			var edit []func(*playback.DSDPolicy)
			if tc.edit != nil {
				edit = append(edit, tc.edit)
			}
			r = newMoveRig(t, tc.st, seams, edit...)
			if tc.prepare != nil {
				tc.prepare(t, r)
			}
			var asks []func(*playback.ChoiceRequest)
			if tc.ask != nil {
				asks = append(asks, tc.ask)
			}
			req := choiceOf(t, r.b, tc.mode, tc.base, asks...)
			before := r.b.Policy()
			armed.Store(true)
			res := r.b.ChooseDSD(bg, req)
			wantChoice(t, res, tc.want)
			published := r.b.Policy()
			if !res.Epoch || !reflect.DeepEqual(res.Policy, published) {
				t.Fatalf("the answer's epoch (epoch %v) %+v is not the one published %+v", res.Epoch, res.Policy, published)
			}
			if published.Generation != before.Generation+1 || published.Publication <= before.Publication {
				t.Errorf("the epoch after the write is generation %d, publication %d; before it %d and %d: want the write's reading published, at the next generation",
					published.Generation, published.Publication, before.Generation, before.Publication)
			}
			if tc.after && !optionIn(t, published, dsdstate.ModeNative).Offered {
				t.Error("the write published native not offered: the box's change after its reading, which it never read")
			}
			if tc.check != nil {
				tc.check(t, r, req, published)
			}
		})
	}
}

// knownDAC gives the policy the profile database that knows the test DAC,
// under which auto plays native where the kernel offers it, and DoP where it
// does not.
func knownDAC(p *playback.DSDPolicy) { p.Profiles = playbacktest.KnownProfiles(nil) }

// capped asks native capped at DSD128.
func capped(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = playbacktest.DSD128 }

// containsRefusal reports whether rs refuses bitRate with reason.
func containsRefusal(rs []playback.DSDRefusal, bitRate int, reason string) bool {
	for _, r := range rs {
		if r.BitRate == bitRate && r.Reason == reason {
			return true
		}
	}
	return false
}

// TestARefusalCarriesTheOptionsItWasDecidedOn (B728 B9 PR 3): a refusal is
// answered with the read side of the reading it was decided on. The box stops
// offering native DSD, the DAC's key, token and the choice's revision as they
// were and no refresh run, and a write of native is mode_not_offered, its
// answer carrying native's option not offered, which the epoch published then
// says too; the box offers native again once the write has decided, before it
// publishes, so a publication made from a reading of its own would carry
// native offered.
func TestARefusalCarriesTheOptionsItWasDecidedOn(t *testing.T) {
	box, store := playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeConvert))
	b := policyBackend(t, box, store, func(p *playback.DSDPolicy) { p.Log = &choiceLog{} })
	b.RefreshDSD(true)
	if !optionIn(t, b.Policy(), dsdstate.ModeNative).Offered {
		t.Fatal("native is not offered before the change: the case needs it")
	}
	req := choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged)
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
	playback.SetSeamsForTest(b, &playback.Seams{ChoiceWritten: func(error) { box.SetAltsets(5, 0, playbacktest.AllAltsets()...) }})
	res := b.ChooseDSD(bg, req)
	wantChoice(t, res, playback.ChoiceModeNotOffered)
	if !res.Epoch || optionIn(t, res.Policy, dsdstate.ModeNative).Offered {
		t.Errorf("mode_not_offered answered with native's option %+v: the answer carries a reading the refusal was not decided on", optionIn(t, res.Policy, dsdstate.ModeNative))
	}
	if !reflect.DeepEqual(res.Policy, b.Policy()) {
		t.Errorf("the answer's epoch %+v is not the one published %+v", res.Policy.DSD, b.Policy().DSD)
	}
	if res.Policy.DSD.DAC.Key != req.DACKey || res.Policy.DSD.DAC.Token != req.DACToken || revisionOf(t, res.Policy) != 1 {
		t.Errorf("the answer's DAC %+v: the case needs the key, the token and the revision unchanged", res.Policy.DSD.DAC)
	}
}

// publishedOnce is a channel the Published seam signals on, never blocking:
// a refresh's publication of a new set.
type publishedOnce struct{ ch chan struct{} }

func newPublishedOnce() *publishedOnce { return &publishedOnce{ch: make(chan struct{}, 16)} }

func (p *publishedOnce) seam() {
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

// TestNoRefreshPublishesBetweenAWritesReadingAndItsPublication (B728 B9 PR 3):
// from a write's reading until it has published it, a refresh reads nothing
// and publishes nothing: the write parked in the state's write, the box
// changed, the supervisor's refresh asked by hand and on its goroutine, the
// box is not read and the epoch is as it was. Let go, the write publishes its
// own reading (native, as it read it), and the refresh it then owes publishes
// the box as it is now (DoP in native's place) at a larger publication.
func TestNoRefreshPublishesBetweenAWritesReadingAndItsPublication(t *testing.T) {
	store := newHeldStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	box := playbacktest.NewBox(5)
	published := newPublishedOnce()
	b := keyedBackend(t, box, store, nil, knownDAC)
	playback.SetSeamsForTest(b, &playback.Seams{Published: published.seam})
	t.Cleanup(store.let)
	req := choiceOf(t, b, dsdstate.ModeAuto, 1, acknowledged)
	store.armed.Store(true)
	answered := make(chan playback.ChoiceResult, 1)
	go func() { answered <- b.ChooseDSD(bg, req) }()
	within(t, store.came, "the write inside the state's write, its reading held")
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
	reads, before := box.Reads(), b.Policy()
	b.RefreshDSD(true)
	b.RequestRefreshDSD(bg, true)
	if n := box.Reads(); n != reads {
		t.Errorf("a refresh read the box %d times while the write held its reading", n-reads)
	}
	if !reflect.DeepEqual(b.Policy(), before) {
		t.Errorf("a refresh published while the write held its reading: %+v, was %+v", b.Policy().DSD, before.DSD)
	}
	store.let()
	res := within(t, answered, "the write's answer")
	wantChoice(t, res, playback.ChoiceWritten)
	if rates, transports := ratesOf(res.Policy); len(rates) != 3 || transports[0] != "" {
		t.Errorf("the write published %v (%q): want native at every rate, as it read the box", rates, transports)
	}
	within(t, published.ch, "the refresh the write owed, publishing the box as it is now")
	now := b.Policy()
	if rates, transports := ratesOf(now); len(rates) == 0 || transports[0] != playback.TransportDoP {
		t.Errorf("the owed refresh published %v (%q): want DoP in native's place", rates, transports)
	}
	if now.Publication <= res.Policy.Publication || now.Generation != res.Policy.Generation+1 {
		t.Errorf("the owed refresh published publication %d, generation %d; the write's %d, %d: want it after the write's",
			now.Publication, now.Generation, res.Policy.Publication, res.Policy.Generation)
	}
}

// TestTheAnswerIsThePairTheWritePublished (B728 B9 PR 3): a write's answer is
// the pair its publication stored, captured under the same hold: the box
// changed and a refresh asked as the write publishes (ChoicePublished), and
// the write held, its lock let go, until that refresh has published
// (ChoiceAnswered), its answer is still its own pair, native as it read it,
// and the refresh's publication, DoP, follows it.
func TestTheAnswerIsThePairTheWritePublished(t *testing.T) {
	box := playbacktest.NewBox(5)
	published := newPublishedOnce()
	b := keyedBackend(t, box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), nil, knownDAC)
	var asked sync.Once
	playback.SetSeamsForTest(b, &playback.Seams{
		Published: published.seam,
		ChoicePublished: func() {
			asked.Do(func() {
				box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
				b.RequestRefreshDSD(bg, true)
			})
		},
		ChoiceAnswered: func() {
			select {
			case <-published.ch:
			case <-time.After(watchdog):
				t.Error("the refresh asked as the write published did not publish while the write was held")
			}
		},
	})
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeAuto, 1, acknowledged))
	wantChoice(t, res, playback.ChoiceWritten)
	if rates, transports := ratesOf(res.Policy); len(rates) != 3 || transports[0] != "" {
		t.Errorf("the answer carries %v (%q): want the write's own pair, native at every rate", rates, transports)
	}
	now := b.Policy()
	if rates, transports := ratesOf(now); len(rates) == 0 || transports[0] != playback.TransportDoP {
		t.Fatalf("the refresh published %v (%q): the case needs DoP published after the write", rates, transports)
	}
	if now.Publication <= res.Policy.Publication {
		t.Errorf("the refresh's publication %d does not follow the write's %d", now.Publication, res.Policy.Publication)
	}
}

// parkedBox is the test DAC's box whose next reading of the unit's model,
// once armed, waits until it is let go, came closed as it comes.
type parkedBox struct {
	*playbacktest.Box
	armed      atomic.Bool
	came, gate chan struct{}
	letOnce    sync.Once
}

func newParkedBox() *parkedBox {
	return &parkedBox{Box: playbacktest.NewBox(5), came: make(chan struct{}), gate: make(chan struct{})}
}

func (p *parkedBox) UnitModel(card int) (alsa.UnitModel, error) {
	if p.armed.CompareAndSwap(true, false) {
		close(p.came)
		<-p.gate
	}
	return p.Box.UnitModel(card)
}

func (p *parkedBox) let() { p.letOnce.Do(func() { close(p.gate) }) }

// TestAWritesReadingWaitsForARefreshUnderWay (B728 B9 PR 3): a refresh parked
// in its reading of the box holds a write's reading back, the write past its
// lease: the box changed while the refresh is parked (native no longer
// offered) and the refresh let go, the write reads the box after the refresh
// has published, so it decides on the box as it is then (mode_not_offered for
// native), and its answer is that publication, no earlier one.
func TestAWritesReadingWaitsForARefreshUnderWay(t *testing.T) {
	box := newParkedBox()
	t.Cleanup(box.let)
	leased := make(chan struct{})
	var once sync.Once
	b := policyBackend(t, box.Box, playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), func(p *playback.DSDPolicy) {
		p.Box, p.Log = box, &choiceLog{}
	})
	b.RefreshDSD(true)
	playback.SetSeamsForTest(b, &playback.Seams{ChoiceLeased: func() { once.Do(func() { close(leased) }) }})
	req := choiceOf(t, b, dsdstate.ModeNative, 1, acknowledged)
	box.armed.Store(true)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		b.RefreshDSD(true)
	}()
	within(t, box.came, "the refresh parked in its reading of the box")
	answered := make(chan playback.ChoiceResult, 1)
	go func() { answered <- b.ChooseDSD(bg, req) }()
	within(t, leased, "the write past its lease")
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
	box.let()
	within(t, refreshed, "the refresh once let go")
	res := within(t, answered, "the write's answer")
	wantChoice(t, res, playback.ChoiceModeNotOffered)
	if !reflect.DeepEqual(res.Policy, b.Policy()) || optionIn(t, res.Policy, dsdstate.ModeNative).Offered {
		t.Errorf("the answer %+v, the epoch published %+v: want the publication of the box as the refresh left it", res.Policy.DSD, b.Policy().DSD)
	}
}

// TestAWriteWhoseTimeRunsOutAtItsReadingWritesNothing (B728 B9 PR 3): a write
// waits for the refreshes' lock at most until its context ends, asks it once
// it holds the lock and again once it has read the box, before the state's
// write, and answers persist_failed with the context's error, nothing written.
// Its request ended while a refresh parked in its reading held the lock: it
// answers at once, the refresh still parked, with the epoch standing and
// nothing read, its lease let go, so a PCM Start opens the DAC meanwhile; let
// go, the refresh publishes the box as it is then, and the write reads, writes
// and publishes nothing after it. Its own reading parked: the reading is not
// interrupted, and is published, as every outcome after it is.
func TestAWriteWhoseTimeRunsOutAtItsReadingWritesNothing(t *testing.T) {
	t.Run("a refresh holding the lock", func(t *testing.T) {
		box := newParkedBox()
		leased, waited := newSignal(), newSignal()
		r := newProbeRig(t, newHeldProbe(dsd64Frames), func(c *playback.Config) {
			c.Seams = &playback.Seams{ChoiceLeased: leased.fire, Leased: func(engine.SessionID) { waited.fire() }}
		})
		r.pol.Box = box
		r.b.RefreshDSD(true)
		t.Cleanup(box.let)
		req := choiceOf(t, r.b, dsdstate.ModeConvert, 1)
		// What the parked refresh reads once let go: native no longer offered.
		box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
		box.armed.Store(true)
		refreshed := make(chan struct{})
		go func() {
			defer close(refreshed)
			r.b.RefreshDSD(true)
		}()
		within(t, box.came, "the refresh parked in its reading of the box")
		standing, reads := r.b.Policy(), box.Reads()
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		answered := make(chan playback.ChoiceResult, 1)
		go func() { answered <- r.b.ChooseDSD(ctx, req) }()
		within(t, leased.ch, "the write past its lease")
		cancel()
		res := within(t, answered, "the write's answer, the refresh still parked")
		wantChoice(t, res, playback.ChoicePersistFailed)
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("the cause %v, want the request's end", res.Err)
		}
		if !res.Epoch || !reflect.DeepEqual(res.Policy, standing) {
			t.Errorf("the answer's epoch %+v, want the one standing %+v", res.Policy.DSD, standing.DSD)
		}
		started := make(chan error, 1)
		go func() { started <- r.b.Start(context.Background(), startOf(t, 1)) }()
		if err := within(t, started, "a PCM Start, the refresh still parked"); err == nil {
			t.Error("the Start answered nil: the test's DAC refuses every open")
		}
		if opens, _ := r.sinks.counts(); opens != 1 {
			t.Errorf("the DAC was opened %d times, want the Start's one", opens)
		}
		select {
		case <-waited.ch:
			t.Error("the Start waited for the write's lease")
		default:
		}
		if err := r.b.Release(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		box.let()
		within(t, refreshed, "the refresh once let go")
		r.idle(t)
		// The refresh's one reading of the unit (Box.Reads counts the unit's)
		// and its one publication: the write's wait, given up, did nothing.
		if n := box.Reads() - reads; n != 1 {
			t.Errorf("the unit was read %d times once the refresh was let go, want its one", n)
		}
		if r.store.Writes() != 0 {
			t.Errorf("a write past its time wrote the state %d times", r.store.Writes())
		}
		if now := r.b.Policy(); now.Publication != standing.Publication+1 || optionIn(t, now, dsdstate.ModeNative).Offered {
			t.Errorf("after the refresh: publication %d (%d standing), native offered %v; want the refresh's one publication of the box as it is",
				now.Publication, standing.Publication, optionIn(t, now, dsdstate.ModeNative).Offered)
		}
	})
	t.Run("its own reading", func(t *testing.T) {
		box := newParkedBox()
		t.Cleanup(box.let)
		store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		b := policyBackend(t, box.Box, store, func(p *playback.DSDPolicy) { p.Box, p.Log = box, &choiceLog{} })
		b.RefreshDSD(true)
		req := choiceOf(t, b, dsdstate.ModeConvert, 1)
		box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
		box.armed.Store(true)
		ctx, cancel := context.WithCancel(bg)
		answered := make(chan playback.ChoiceResult, 1)
		go func() { answered <- b.ChooseDSD(ctx, req) }()
		within(t, box.came, "the write parked in its reading of the box")
		cancel()
		box.let()
		res := within(t, answered, "the write's answer")
		wantChoice(t, res, playback.ChoicePersistFailed)
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("the cause %v, want the cancellation", res.Err)
		}
		if store.Writes() != 0 {
			t.Errorf("a write past its time wrote the state %d times", store.Writes())
		}
		if !res.Epoch || !reflect.DeepEqual(res.Policy, b.Policy()) || optionIn(t, res.Policy, dsdstate.ModeNative).Offered {
			t.Errorf("the answer %+v, want the publication of the write's reading, native no longer offered", res.Policy.DSD)
		}
	})
}

// TestAStateWrittenBetweenTheDecisionAndThePublicationIsPublishedAfterIt
// (B728 B9 PR 3): a write publishes the state its decision read, never one
// another writer left after it, and a state written in between is published
// after the write's publication. A write of DoP refused for its confirmation,
// and another writer (the migration's part for the DAC) recording the
// confirmation once the write's decision is made and before it publishes: the
// answer says DoP needs its confirmation, as decided, and the refresh the
// write then owes publishes the confirmation at a larger publication.
func TestAStateWrittenBetweenTheDecisionAndThePublicationIsPublishedAfterIt(t *testing.T) {
	store := playbacktest.NewMemStore(dsdstate.Defaults())
	b := keyedBackend(t, playbacktest.NewBox(5), store, nil)
	var migrated sync.Once
	playback.SetSeamsForTest(b, &playback.Seams{ChoiceWritten: func(error) {
		migrated.Do(func() {
			if err := store.Update(func(st *dsdstate.State) error {
				st.Entry(playbacktest.DACKey(), playbacktest.DACStamp()).DoPConfirmed = &dsdstate.DoPConfirmation{BCDDevice: playbacktest.DACBCD, ConfirmedAt: playbacktest.T0, SetBy: dsdstate.ByMigration}
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}})
	res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeDoP, 0, acknowledged))
	wantChoice(t, res, playback.ChoiceConfirmationRequired)
	if o := optionIn(t, res.Policy, dsdstate.ModeDoP); !o.NeedsConfirmation || dacOf(t, res.Policy).Choice.DoPConfirmed {
		t.Errorf("the answer's DoP option %+v, confirmed %v: want the state the refusal was decided on, unconfirmed", o, dacOf(t, res.Policy).Choice.DoPConfirmed)
	}
	// The refresh the write owed runs on the refreshes' goroutine, the one
	// goroutine of the backend's left once the write has returned: joined, it
	// has published the confirmation, a change of the read side alone.
	ctx, cancel := context.WithTimeout(bg, watchdog)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	now := b.Policy()
	if !dacOf(t, now).Choice.DoPConfirmed {
		t.Fatalf("the confirmation written after the write's decision is not published: %+v", dacOf(t, now).Choice)
	}
	if now.Publication <= res.Policy.Publication {
		t.Errorf("the confirmation is published at %d, the write at %d: want it after the write", now.Publication, res.Policy.Publication)
	}
}

// TestTheCauseIsKeptWhereItOccurs (B728 B9 PR 3): a write answers its
// outcome's cause where one occurred: the context's end at the writes' turn
// and at a probe holding the device slot, cancelled or past its deadline;
// ErrDSDWritesClosed once the player's stop closed the writes; the store's
// error for a write that failed, one that landed, a file refused in memory and
// the store's fence; a cause for no state and for the backend's own fence;
// and none for a write written or refused for a word.
func TestTheCauseIsKeptWhereItOccurs(t *testing.T) {
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	expired, cancelExpired := context.WithDeadline(bg, time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, ctx := range []struct {
		name string
		ctx  context.Context
		want error
	}{{"cancelled", cancelled, context.Canceled}, {"past its deadline", expired, context.DeadlineExceeded}} {
		t.Run("the turn's wait, "+ctx.name, func(t *testing.T) {
			store := newHeldStore(playbacktest.ChoiceState(dsdstate.ModeNative))
			b := policyBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Log = &choiceLog{} })
			b.RefreshDSD(true)
			t.Cleanup(store.let)
			store.armed.Store(true)
			held := make(chan playback.ChoiceResult, 1)
			go func() { held <- b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1)) }()
			within(t, store.came, "the write holding the turn")
			res := b.ChooseDSD(ctx.ctx, choiceOf(t, b, dsdstate.ModeOff, 1))
			wantChoice(t, res, playback.ChoicePersistFailed)
			if !errors.Is(res.Err, ctx.want) || !res.Epoch {
				t.Errorf("the cause %v (epoch %v), want %v and the epoch standing", res.Err, res.Epoch, ctx.want)
			}
			store.let()
			wantChoice(t, within(t, held, "the held write's answer"), playback.ChoiceWritten)
		})
		t.Run("the probe's wait, "+ctx.name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames)
			let := probe.hold(0)
			defer let()
			r := newProbeRig(t, probe)
			r.b.RefreshDSD(true)
			go r.b.ProbeDSD(bg, ready)
			within(t, probe.came, "the probe holding the device slot")
			res := r.b.ChooseDSD(ctx.ctx, choiceOf(t, r.b, dsdstate.ModeConvert, 1))
			wantChoice(t, res, playback.ChoicePersistFailed)
			if !errors.Is(res.Err, ctx.want) || !res.Epoch {
				t.Errorf("the cause %v (epoch %v), want %v and the epoch standing", res.Err, res.Epoch, ctx.want)
			}
			let()
			r.idle(t)
		})
	}
	t.Run("the writes closed", func(t *testing.T) {
		b, _, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		within(t, b.CloseDSDWrites(), "the writes' close")
		res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1))
		wantChoice(t, res, playback.ChoicePersistFailed)
		if !errors.Is(res.Err, playback.ErrDSDWritesClosed) || !res.Epoch {
			t.Errorf("the cause %v (epoch %v), want ErrDSDWritesClosed and the epoch standing", res.Err, res.Epoch)
		}
	})
	noSpace := errors.New("no space left on device")
	landed := fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)
	notWritable := fmt.Errorf("%w: a named pipe is there", dsdstate.ErrNotWritable)
	for _, tc := range []struct {
		name  string
		edit  func(*playback.DSDPolicy)
		store func(*playbacktest.MemStore)
		mode  string
		base  uint64
		ask   func(*playback.ChoiceRequest)
		want  playback.ChoiceOutcome
		cause error
		// any says a cause of the backend's own, unexported.
		any bool
	}{
		{name: "a write that failed", store: func(m *playbacktest.MemStore) { m.Fail(noSpace) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoicePersistFailed, cause: noSpace},
		{name: "a write that landed", store: func(m *playbacktest.MemStore) { m.Land(landed) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoicePersistUncertain, cause: atomicwrite.ErrLanded},
		{name: "a file refused in memory", store: func(m *playbacktest.MemStore) { m.Fail(notWritable) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoiceStateUnreadable, cause: dsdstate.ErrNotWritable},
		{name: "the store's fence", store: func(m *playbacktest.MemStore) { m.Fail(dsdstate.ErrLandedNotHeld) }, mode: dsdstate.ModeOff, base: 1, want: playback.ChoicePersistUncertain, cause: dsdstate.ErrLandedNotHeld},
		{name: "no state", edit: func(p *playback.DSDPolicy) { p.Store = nil }, mode: dsdstate.ModeOff, base: 0, want: playback.ChoiceStateUnreadable, any: true},
		{name: "written", mode: dsdstate.ModeConvert, base: 1, want: playback.ChoiceWritten},
		{name: "stale", mode: dsdstate.ModeOff, base: 0, want: playback.ChoiceStale},
		{name: "dac_changed", mode: dsdstate.ModeOff, base: 1, ask: func(r *playback.ChoiceRequest) { r.DACKey = "d1-0000000000000000" }, want: playback.ChoiceDACChanged},
		{name: "first_use_unacknowledged", mode: dsdstate.ModeDoP, base: 0, ask: confirmed, want: playback.ChoiceFirstUseUnacknowledged, edit: freshState},
		{name: "dop_confirmation_required", mode: dsdstate.ModeDoP, base: 0, ask: acknowledged, want: playback.ChoiceConfirmationRequired, edit: freshState},
		{name: "mode_not_offered", mode: dsdstate.ModeNative, base: 1, ask: acknowledged, want: playback.ChoiceModeNotOffered, edit: func(p *playback.DSDPolicy) {
			p.Box.(*playbacktest.Box).SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var edit []func(*playback.DSDPolicy)
			if tc.edit != nil {
				edit = append(edit, tc.edit)
			}
			b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative), edit...)
			if tc.store != nil {
				tc.store(store)
			}
			var asks []func(*playback.ChoiceRequest)
			if tc.ask != nil {
				asks = append(asks, tc.ask)
			}
			res := b.ChooseDSD(bg, choiceOf(t, b, tc.mode, tc.base, asks...))
			wantChoice(t, res, tc.want)
			switch {
			case tc.any:
				if res.Err == nil {
					t.Error("the answer carries no cause")
				}
			case tc.cause == nil:
				if res.Err != nil {
					t.Errorf("the answer carries the cause %v, want none", res.Err)
				}
			case !errors.Is(res.Err, tc.cause):
				t.Errorf("the cause %v, want %v", res.Err, tc.cause)
			}
		})
	}
	t.Run("the backend's own fence", func(t *testing.T) {
		b, store, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		store.Land(landed)
		wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1)), playback.ChoicePersistUncertain)
		store.Fail(nil)
		res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1))
		wantChoice(t, res, playback.ChoicePersistUncertain)
		if res.Err == nil {
			t.Error("the fence's answer carries no cause")
		}
	})
	t.Run("the player not stopped", func(t *testing.T) {
		b, _, _ := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		if err := b.Release(bg, 7); err != nil {
			t.Fatal(err)
		}
		res := b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeOff, 1))
		wantChoice(t, res, playback.ChoicePlaybackNotStopped)
		if res.Err != nil || !res.Epoch {
			t.Errorf("the cause %v (epoch %v), want none and the epoch standing", res.Err, res.Epoch)
		}
	})
}

// freshState gives the policy a state with no choice, no confirmation and no
// first use: what DoP's checks refuse.
func freshState(p *playback.DSDPolicy) {
	p.Store.(*playbacktest.MemStore).Set(dsdstate.Defaults())
}

// TestEveryPublicationMovesThePublicationCount (B728 B9 PR 3): every
// publication of the player's DSD moves its count, which no other thing does:
// New's is 1; the first reading publishes a set, 2, at the next generation; a
// reading that changes nothing publishes nothing; a change of the read side
// alone (a first use observed) moves the count and not the generation; a change
// of the set moves both; and a write's own publication moves the count, after
// which a reading of the box and state it wrote changes nothing.
func TestEveryPublicationMovesThePublicationCount(t *testing.T) {
	box, store := playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	b := policyBackend(t, box, store, func(p *playback.DSDPolicy) { p.Log = &choiceLog{} })
	step := func(what string, wantPub, wantGen uint64) playback.PlayerPolicy {
		t.Helper()
		p := b.Policy()
		if p.Publication != wantPub || p.Generation != wantGen {
			t.Errorf("%s: publication %d, generation %d; want %d and %d", what, p.Publication, p.Generation, wantPub, wantGen)
		}
		return p
	}
	step("New", 1, 1)
	b.RefreshDSD(true)
	step("the first reading", 2, 2)
	b.RefreshDSD(true)
	step("a reading that changes nothing", 2, 2)
	st := store.State()
	st.DACs[0].FirstUse[0].ObservedAt = playbacktest.T0
	store.Set(st)
	b.RefreshDSD(true)
	step("a first use observed", 3, 2)
	box.SetAltsets(5, 0, playbacktest.S32Altset(), playbacktest.S24Altset())
	b.RefreshDSD(true)
	step("native no longer offered", 4, 3)
	wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1)), playback.ChoiceWritten)
	written := b.Policy()
	if written.Publication != 5 {
		t.Errorf("a written choice publishes %d, want 5", written.Publication)
	}
	b.RefreshDSD(true)
	step("a reading of what the write published", 5, written.Generation)
}

// TestNoSessionMeetsAChoicesReading (B728 B9 PR 3): while a write holds its
// reading, no session is admitted or acquires the device, so no refresh owed
// meanwhile is one a session's admission, activation or monitor needs: a
// Start waits for the write's lease, its admission and the device's slot not
// reached and the box not read for it; a Release of a session that never
// started registers its cleanup record and holds nothing, which the write's
// lease, already taken, does not ask. Let go, the write is written and the
// Start goes on.
func TestNoSessionMeetsAChoicesReading(t *testing.T) {
	store := newHeldStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	box := playbacktest.NewBox(5)
	var admitted, holding atomic.Int32
	waiting := newSignal()
	b := keyedBackend(t, box, store, nil)
	playback.SetSeamsForTest(b, &playback.Seams{
		Admitting: func(engine.OccurrenceID) { admitted.Add(1) },
		Holding:   func(engine.SessionID) { holding.Add(1) },
		Leased:    func(engine.SessionID) { waiting.fire() },
	})
	t.Cleanup(store.let)
	store.armed.Store(true)
	written := make(chan playback.ChoiceResult, 1)
	go func() { written <- b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeConvert, 1)) }()
	within(t, store.came, "the write inside the state's write, its reading held")
	reads := box.Reads()
	started := make(chan error, 1)
	go func() { started <- b.Start(context.Background(), startOf(t, 1)) }()
	within(t, waiting.ch, "the Start waiting for the write's lease")
	if err := b.Release(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if n, h, r := admitted.Load(), holding.Load(), box.Reads()-reads; n != 0 || h != 0 || r != 0 {
		t.Errorf("while the write held its reading: %d admissions, %d holds of the device slot, %d reads of the box", n, h, r)
	}
	store.let()
	wantChoice(t, within(t, written, "the write's answer"), playback.ChoiceWritten)
	if err := within(t, started, "the Start once the lease is let go"); err == nil {
		t.Error("the Start answered nil: the test's resolver opens nothing")
	}
	if err := b.Release(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}
