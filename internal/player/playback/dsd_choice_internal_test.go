package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A choice's write inside the backend (B728 B9 PR 2): the order its checks
// are answered in, the lock order it keeps beside the backend's other work,
// what a write that panics leaves, and the backend's own fence after a write
// that landed. Ported from #1434's (0df60ade) where its stop-only design kept
// them.

// choiceWrite is a write of mode over base for b's bound DAC, as GET publishes
// its key and its token, within 30 s; its outcome.
func choiceWrite(t *testing.T, b *Backend, key, mode string, base uint64) ChoiceOutcome {
	t.Helper()
	return choiceWriteOf(b, key, boundToken(t, b), mode, base)
}

// choiceWriteOf is a write of mode over base made from a read side that
// carried key and token.
func choiceWriteOf(b *Backend, key string, token DACToken, mode string, base uint64) ChoiceOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return b.ChooseDSD(ctx, ChoiceRequest{DACKey: key, DACToken: token, Choice: policy.Choice{BaseRevision: base, Mode: mode, By: dsdstate.ByApp}}).Outcome
}

// boundKey is the bound DAC's key as b publishes it.
func boundKey(t *testing.T, b *Backend) string {
	t.Helper()
	v := b.dsdPub.Load().view
	if v.DAC == nil {
		t.Fatal("the backend publishes no DAC")
	}
	return v.DAC.Key
}

// boundToken is the bound DAC's token as b publishes it.
func boundToken(t *testing.T, b *Backend) DACToken {
	t.Helper()
	v := b.dsdPub.Load().view
	if v.DAC == nil {
		t.Fatal("the backend publishes no DAC")
	}
	return v.DAC.Token
}

// fixtureToken is the token b's key mints for the fixture's unit of stamp's
// maker, as a reading of that unit would (dacToken).
func fixtureToken(t *testing.T, b *Backend, stamp dsdstate.Stamp) DACToken {
	t.Helper()
	key, err := dsdstate.KeyOf(fixtureUSB(), "Test DAC")
	if err != nil {
		t.Fatal(err)
	}
	return b.dacToken(policy.Identity{Key: key, Stamp: stamp, BCDDevice: fixtureBCD})
}

// refusingContent refuses every reference before anything is opened: a
// Start registers, takes the device slot and is refused, opening no device.
type refusingContent struct{}

func (refusingContent) Open(context.Context, engine.Reference) (Content, error) {
	return Content{}, &engine.Unplayable{Reason: engine.ReasonMissing, Detail: "the test opens nothing"}
}

// TestTheWritesChecksAreAnsweredInTheirOrder (B728 B9 PR 2; the design's
// section 7.2): each check answers before the ones after it, nothing written:
// the request's words (bad_request) first; the writes of the state closed by
// the player's stop (persist_failed) before the player not stopped; the
// player not stopped (playback_not_stopped, the configuration lease) before a
// backend with no DSD policy (state_unreadable), which comes before the DAC
// is read, the reading being that policy's; a DAC not available before
// another DAC's key, and that before a state that did not open; and the
// fence of an earlier write that landed (persist_uncertain) before the
// state's own checks, a stale base among them. A token not of a token's form
// is the request's words' bad_request, and the token of a unit of another
// maker on the key is another DAC's dac_changed, before a state that did not
// open and before the fence (B728 B9 PR 3).
func TestTheWritesChecksAreAnsweredInTheirOrder(t *testing.T) {
	busy := func(t *testing.T, b *Backend) {
		t.Helper()
		// A session's record with nothing held: a Release that came before its
		// Start.
		if err := b.Release(context.Background(), 9); err != nil {
			t.Fatal(err)
		}
	}
	closed := func(t *testing.T, b *Backend) {
		t.Helper()
		select {
		case <-b.CloseDSDWrites():
		case <-time.After(10 * time.Second):
			t.Fatal("the writes' close with none under way did not end")
		}
	}
	noDSD := func(t *testing.T) *Backend {
		t.Helper()
		b, err := New(Config{Resolver: refusingContent{}, Locator: refuseLocator{}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	other := dsdstate.StampOf("Other Labs")
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*Backend, string)
		mode  string
		base  uint64
		want  ChoiceOutcome
		// token is the token the write carries: "" is the fixture's unit's,
		// as a reading of it mints it, other the token of the fixture's model
		// made by another maker.
		token DACToken
	}{
		{"a word the write does not know, every other check failing too", func(t *testing.T) (*Backend, string) {
			b := noDSD(t)
			busy(t, b)
			closed(t, b)
			return b, "d1-0000000000000000"
		}, "loud", 0, ChoiceBadRequest, ""},
		{"a token that is no token, every other check failing too", func(t *testing.T) (*Backend, string) {
			b := noDSD(t)
			busy(t, b)
			closed(t, b)
			return b, "d1-0000000000000000"
		}, dsdstate.ModeOff, 0, ChoiceBadRequest, "t1-0"},
		{"the writes closed, the player not stopped and no DSD policy", func(t *testing.T) (*Backend, string) {
			b := noDSD(t)
			busy(t, b)
			closed(t, b)
			return b, "d1-0000000000000000"
		}, dsdstate.ModeOff, 0, ChoicePersistFailed, ""},
		{"the player not stopped, with no DSD policy", func(t *testing.T) (*Backend, string) {
			b := noDSD(t)
			busy(t, b)
			return b, "d1-0000000000000000"
		}, dsdstate.ModeOff, 0, ChoicePlaybackNotStopped, ""},
		{"no DSD policy, answered before the DAC is read", func(t *testing.T) (*Backend, string) {
			return noDSD(t), "d1-0000000000000000"
		}, dsdstate.ModeOff, 0, ChoiceStateUnreadable, ""},
		{"no DAC available, another DAC's key and no state", func(t *testing.T) (*Backend, string) {
			b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) {
				p.Bound = func() (BoundDAC, bool) { return BoundDAC{}, false }
				p.Store = nil
			})
			return b, "d1-0000000000000000"
		}, dsdstate.ModeOff, 1, ChoiceDACUnavailable, ""},
		{"another DAC's key and no state", func(t *testing.T) (*Backend, string) {
			b, _ := fixtureSessionBackend(t)
			b.cfg.DSD.Store = nil
			return b, "d1-0000000000000000"
		}, dsdstate.ModeOff, 1, ChoiceDACChanged, ""},
		{"another maker's unit's token and no state", func(t *testing.T) (*Backend, string) {
			b, _ := fixtureSessionBackend(t)
			key := boundKey(t, b)
			b.cfg.DSD.Store = nil
			return b, key
		}, dsdstate.ModeOff, 1, ChoiceDACChanged, "other"},
		{"another maker's unit's token, an earlier write landed", func(t *testing.T) (*Backend, string) {
			b, store := fixtureSessionBackend(t)
			st := store.State()
			b.landedChoice.Store(&st)
			return b, boundKey(t, b)
		}, dsdstate.ModeOff, 7, ChoiceDACChanged, "other"},
		{"no state", func(t *testing.T) (*Backend, string) {
			b, _ := fixtureSessionBackend(t)
			key := boundKey(t, b)
			b.cfg.DSD.Store = nil
			return b, key
		}, dsdstate.ModeOff, 1, ChoiceStateUnreadable, ""},
		{"an earlier write landed, the base stale", func(t *testing.T) (*Backend, string) {
			b, store := fixtureSessionBackend(t)
			st := store.State()
			b.landedChoice.Store(&st)
			return b, boundKey(t, b)
		}, dsdstate.ModeOff, 7, ChoicePersistUncertain, ""},
		{"the base stale", func(t *testing.T) (*Backend, string) {
			b, _ := fixtureSessionBackend(t)
			return b, boundKey(t, b)
		}, dsdstate.ModeOff, 7, ChoiceStale, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, key := tc.setup(t)
			token := tc.token
			switch token {
			case "":
				token = fixtureToken(t, b, fixtureStamp)
			case "other":
				token = fixtureToken(t, b, other)
			}
			if got := choiceWriteOf(b, key, token, tc.mode, tc.base); got != tc.want {
				t.Errorf("the write answered %s; want %s", got, tc.want)
			}
			// The first acquisition after the write let go of its lease
			// (letLease), which nothing else can hold here (the backend runs
			// nothing): asserted free at once, so a lock the write kept fails
			// the case, where a Lock waited to the package's timeout (backlog
			// B853, round 4).
			if !b.mu.TryLock() {
				t.Fatal("the backend's lock is held once the write returned")
			}
			lease := b.lease
			b.mu.Unlock()
			if lease != nil || len(b.choiceTurn) != 0 {
				t.Errorf("the write left its lease (%v) or its turn (%d) held", lease != nil, len(b.choiceTurn))
			}
		})
	}
}

// forgetfulStore is a state's store whose first write lands and is not known
// to be durable (atomicwrite.ErrLanded), keeping nothing of it, and which
// fences nothing after: a store that does not hold its own fence.
type forgetfulStore struct {
	fixtureStore
	landed, writes atomic.Int32
}

func (f *forgetfulStore) Update(fn func(*dsdstate.State) error) error {
	if f.landed.CompareAndSwap(0, 1) {
		next := f.State()
		if err := fn(&next); err != nil {
			return err
		}
		return fmt.Errorf("the directory's barrier failed: %w", atomicwrite.ErrLanded)
	}
	f.writes.Add(1)
	return f.fixtureStore.Update(fn)
}

// TestTheBackendsOwnFenceRefusesEveryLaterWrite (B728 B9 PR 2): a write that
// landed and is not known to be durable fences every later write until the
// next start by the backend's own record of it (landedChoice), whatever the
// store says: a later write is persist_uncertain and reaches no store, so it
// cannot be made over the state the file holds from the one the store does.
func TestTheBackendsOwnFenceRefusesEveryLaterWrite(t *testing.T) {
	store := &forgetfulStore{fixtureStore: fixtureStore{st: nativeFixture(t)}}
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Store = store })
	key := boundKey(t, b)
	if got := choiceWrite(t, b, key, dsdstate.ModeConvert, 0); got != ChoicePersistUncertain {
		t.Fatalf("the write that landed answered %s; want persist_uncertain", got)
	}
	if got := choiceWrite(t, b, key, dsdstate.ModeOff, 0); got != ChoicePersistUncertain {
		t.Errorf("a write after the one that landed answered %s; want persist_uncertain", got)
	}
	if n := store.writes.Load(); n != 0 {
		t.Errorf("the store took %d writes after the one that landed", n)
	}
}

// TestAWriteThatPanicsLetsGoOfItsLeaseAndItsTurn (B728 B9 PR 2): a write that
// panics, before the DAC is read, in its store's write, or once the state is
// written and before it is published (serve's adapter recovers it), lets go of
// the configuration lease, the writes' turn and its hold of its reading, and
// counts no write of the state on its way out, so the next Start registers, a
// probe may take the slot, a refresh reads and the next write is made; what a
// panic after the state's write left in the state the next refresh publishes.
func TestAWriteThatPanicsLetsGoOfItsLeaseAndItsTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seams func(panicNow func()) *Seams
		store bool
		wrote string
	}{
		{"before the DAC is read", func(panicNow func()) *Seams { return &Seams{ChoiceLeased: panicNow} }, false, dsdstate.ModeNative},
		{"in its store's write", func(func()) *Seams { return nil }, true, dsdstate.ModeNative},
		{"after the state's write", func(panicNow func()) *Seams { return &Seams{ChoiceWritten: func(error) { panicNow() }} }, false, dsdstate.ModeConvert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &panickyStore{fixtureStore: fixtureStore{st: nativeFixture(t)}}
			b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Store = store })
			b.cfg.Resolver = refusingContent{}
			key := boundKey(t, b)
			store.panics.Store(tc.store)
			b.cfg.Seams = tc.seams(func() { panic("the test's panic inside the write") })
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the write did not panic: the panic did not reach it")
					}
				}()
				choiceWrite(t, b, key, dsdstate.ModeConvert, 0)
			}()
			b.cfg.Seams = nil
			// As the refused writes' case: asserted free at once (round 4).
			if !b.mu.TryLock() {
				t.Fatal("the backend's lock is held once the write that panicked returned")
			}
			lease := b.lease
			b.mu.Unlock()
			if lease != nil || len(b.choiceTurn) != 0 {
				t.Fatalf("the write that panicked left its lease (%v) or its turn (%d) held", lease != nil, len(b.choiceTurn))
			}
			// Its hold of its reading too (B728 B9 PR 3): a refresh after it
			// reads and publishes.
			if !b.dsdMu.TryLock() {
				t.Fatal("the refreshes' lock is held once the write that panicked returned")
			}
			reading := b.choiceReading
			b.dsdMu.Unlock()
			if reading {
				t.Fatal("the write that panicked left its hold of its reading")
			}
			if _, writes := b.Running(); writes != 0 {
				t.Errorf("%d writes of the state counted once the write panicked", writes)
			}
			if p, why := b.reserveProbe(); p == nil {
				t.Errorf("a probe after the write that panicked was refused: %s", why)
			} else {
				b.endProbe(p)
			}
			if mode, _, _ := store.State().ChoiceFor(b.dsd.Load().set.Key, b.dsd.Load().set.Stamp); mode != tc.wrote {
				t.Errorf("the state holds %s after the panic; want %s", mode, tc.wrote)
			}
			b.RefreshDSD(true)
			pub := b.dsdPub.Load().view.DAC.Choice
			if pub.Mode != tc.wrote {
				t.Errorf("the refresh after the panic publishes %s; want %s", pub.Mode, tc.wrote)
			}
			if got := choiceWrite(t, b, key, dsdstate.ModeOff, pub.Revision); got != ChoiceWritten {
				t.Errorf("the write after the one that panicked answered %s; want written", got)
			}
			if err := b.Start(context.Background(), engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1}}); !errors.As(err, new(*engine.Unplayable)) {
				t.Errorf("the Start after the write that panicked answered %v; want its content's refusal, having registered", err)
			}
			if err := b.Release(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// panickyStore is a state's store whose write panics once armed.
type panickyStore struct {
	fixtureStore
	panics atomic.Bool
}

func (p *panickyStore) Update(fn func(*dsdstate.State) error) error {
	if p.panics.CompareAndSwap(true, false) {
		panic("the test's store panics in its write")
	}
	return p.fixtureStore.Update(fn)
}

// TestTheChoiceWritesLockOrderHolds (B728 B9 PR 2): a choice's write takes the
// writes' turn, then the backend's lock for its lease, then the refreshes'
// lock and each session's under it for its publish; the state's store is taken
// alone, its writes with no lock of the backend's held, nor any taken inside
// them. Under -race and together, writes, Starts and Releases (sessions the
// lease refuses a write beside), refreshes, evidence writes and reads finish.
// A reader takes the store's lock under the backend's and under a session's,
// standing in for any path that would, so a write that took either inside the
// state's write deadlocks here.
func TestTheChoiceWritesLockOrderHolds(t *testing.T) {
	b, store := fixtureSessionBackend(t)
	b.cfg.Resolver = refusingContent{}
	key, token := boundKey(t, b), boundToken(t, b)
	var wg sync.WaitGroup
	done := make(chan struct{})
	stop := make(chan struct{})
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			modes := []string{dsdstate.ModeNative, dsdstate.ModeConvert, dsdstate.ModeAuto}
			for j := range 25 {
				rev := b.dsdPub.Load().view.DAC.Choice.Revision
				b.ChooseDSD(context.Background(), ChoiceRequest{DACKey: key, DACToken: token,
					Choice: policy.Choice{BaseRevision: rev, Mode: modes[(i+j)%3], FirstUseAcknowledged: true, By: dsdstate.ByApp}})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := range 40 {
			id := engine.SessionID(100 + j)
			_ = b.Start(context.Background(), engine.StartRequest{Session: id, First: engine.Occurrence{ID: engine.OccurrenceID(id)}})
			_ = b.Release(context.Background(), id)
		}
	}()
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
						BCDDevice: fixtureBCD, StreamingObservedAt: fixtureT0}}
				}
				return nil
			})
		}
	}()
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b.mu.Lock()
			var live *session
			for _, s := range b.sessions {
				live = s
				break
			}
			_ = store.State()
			b.mu.Unlock()
			if live != nil {
				live.mu.Lock()
				_ = store.State()
				live.mu.Unlock()
			}
		}
	}()
	go func() {
		wg.Wait()
		close(stop)
		readers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("writes, Starts, Releases, refreshes and evidence writes together did not finish: a lock order deadlocked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
