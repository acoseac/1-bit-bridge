package playback

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// hookedLog is a logger whose records a recorder keeps, which runs hook on
// the backend's panic line, on the goroutine that logs it.
type hookedLog struct {
	slog.Handler
	hook func()
}

func (h hookedLog) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == panicMessage && h.hook != nil {
		h.hook()
	}
	return h.Handler.Handle(ctx, r)
}

// statePanics is a state's store whose State, once armed, panics once.
type statePanics struct {
	fixtureStore
	armed atomic.Bool
}

func (s *statePanics) State() dsdstate.State {
	if s.armed.CompareAndSwap(true, false) {
		panic("the test's state panics at the publication's read")
	}
	return s.fixtureStore.State()
}

// TestAPublicationThatPanicsAnswersWithNoEpoch (B728 B9 PR 3): a write's
// publication that panics is contained as a refresh's reading is: logged
// without its value (unless the runtime's own error), the next reading made
// fresh (dsdRead zeroed), and a refresh asked; the write's outcome stands, its
// state written and durable, and its answer carries no epoch unless its pair
// was stored. The writes' turn, the configuration lease and the write's hold
// of its reading are let go. The refresh the containment asked runs, its end
// heard (Seams.ChoiceRefreshed) before anything else refreshes: before the
// pair is stored it publishes the state written, at the next publication;
// after it, it finds that pair standing and publishes nothing. Before the
// pair is stored, the panic stands for any (a published set gone); after it,
// the store's state panics at the publication's read.
func TestAPublicationThatPanicsAnswersWithNoEpoch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored bool
	}{
		{"before its pair is stored", false},
		{"after its pair is stored, at its read of the state", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.NewRecorder()
			store := &statePanics{fixtureStore: fixtureStore{st: nativeFixture(t)}}
			var b *Backend
			var saved *dsdSet
			var readZero atomic.Bool
			log := slog.New(hookedLog{Handler: rec.Logger().Handler(), hook: func() {
				// Under the refreshes' lock, as the containment logs: the
				// reading made fresh already, and the set put back for the
				// refresh the containment asks.
				readZero.Store(b.dsdRead.IsZero())
				if saved != nil {
					b.dsd.Store(saved)
				}
			}})
			pol := &DSDPolicy{Store: store, Kernel: fixtureKernel, Box: fixtureBox{}, Probes: alsapcm.NewProbeCache(),
				Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
				Now:   func() time.Time { return fixtureT0 }}
			var err error
			b, err = New(Config{Resolver: nativeFilesForRefusal{}, Locator: refuseLocator{}, DSD: pol, Log: log})
			if err != nil {
				t.Fatal(err)
			}
			b.RefreshDSD(true)
			key := boundKey(t, b)
			standing := b.dsdPub.Load().seq
			refreshed := make(chan struct{})
			var once sync.Once
			b.cfg.Seams = &Seams{ChoiceWritten: func(error) {
				if tc.stored {
					store.armed.Store(true)
					return
				}
				saved = b.dsd.Load()
				b.dsd.Store(nil)
			}, ChoiceRefreshed: func() { once.Do(func() { close(refreshed) }) }}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res := b.ChooseDSD(ctx, ChoiceRequest{DACKey: key, DACToken: boundToken(t, b), Choice: policy.Choice{Mode: dsdstate.ModeConvert, By: dsdstate.ByApp}})
			// The refresh the containment asks runs on the refreshes'
			// goroutine: what it published is read once it has returned, and
			// before anything else refreshes.
			select {
			case <-refreshed:
			case <-time.After(10 * time.Second):
				t.Fatal("the refresh the containment asked never ran")
			}
			pub := b.dsdPub.Load()
			if got := pub.view.DAC.Choice.Mode; got != dsdstate.ModeConvert {
				t.Errorf("the containment's refresh left %s published; want the state written, convert", got)
			}
			// The one publication after the one standing: the containment's
			// refresh's before the pair was stored, the write's own after it,
			// which that refresh found standing.
			if pub.seq != standing+1 {
				t.Errorf("publication %d once the containment's refresh returned, %d standing before the write; want one publication, %d", pub.seq, standing, standing+1)
			}
			if tc.stored && pub.seq != res.Policy.Publication {
				t.Errorf("publication %d once the containment's refresh returned; want the write's own, %d, standing", pub.seq, res.Policy.Publication)
			}
			// That refresh ran on a goroutine of the backend's, counted from
			// the ask on: its seam reached is not its goroutine ended, so it is
			// joined before the seams go.
			if err := b.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			b.cfg.Seams = nil
			if res.Outcome != ChoiceWritten {
				t.Fatalf("the write answered %s; want written, its state durable", res.Outcome)
			}
			if mode, _, _ := store.State().ChoiceFor(b.dsd.Load().set.Key, b.dsd.Load().set.Stamp); mode != dsdstate.ModeConvert {
				t.Errorf("the state holds %s; want the write's convert", mode)
			}
			if res.Epoch != tc.stored {
				t.Errorf("the answer carries an epoch: %v; want %v", res.Epoch, tc.stored)
			}
			if tc.stored && (res.Policy.DSD == nil || res.Policy.DSD.DAC == nil || res.Policy.DSD.DAC.Choice.Mode != dsdstate.ModeConvert) {
				t.Errorf("the answer's epoch %+v, want the pair stored, convert chosen", res.Policy.DSD)
			}
			if !tc.stored && res.Policy.DSD != nil {
				t.Errorf("the answer carries %+v with no epoch", res.Policy.DSD)
			}
			lines := rec.Lines(panicMessage)
			if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "the publication of a choice's write") {
				t.Fatalf("the panic was not logged as the publication's: %q", rec.All())
			}
			if tc.stored && len(rec.Carrying("the test's state panics at the publication's read")) != 0 {
				t.Errorf("the panic's value was logged: %q", lines)
			}
			if !readZero.Load() {
				t.Error("the containment did not make the next reading fresh")
			}
			lease, turn, reading, locked := ChoiceHeldForTest(b)
			if !locked || lease || turn || reading {
				t.Fatalf("after the publication's panic: locked %v, lease %v, turn %v, reading %v; want every hold let go", locked, lease, turn, reading)
			}
		})
	}
}

// heldControlsBox is the fixture's box whose next read of the card's
// controls, once armed, waits until the test lets it go: a refresh held in
// its reading of the box, the state read before it (readDSD).
type heldControlsBox struct {
	fixtureBox
	mu         sync.Mutex
	next, came chan struct{}
}

func (h *heldControlsBox) ReadControls(card int) (alsactl.Snapshot, error) {
	h.mu.Lock()
	next, came := h.next, h.came
	h.next, h.came = nil, nil
	h.mu.Unlock()
	if next != nil {
		close(came)
		<-next
	}
	return h.fixtureBox.ReadControls(card)
}

// holdNext holds the next read until let, and says when it came.
func (h *heldControlsBox) holdNext() (came <-chan struct{}, let func()) {
	next, c := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.next, h.came = next, c
	h.mu.Unlock()
	var once sync.Once
	return c, func() { once.Do(func() { close(next) }) }
}

// TestTheRefreshAWriteAsksIsMadeForcedAndHeardEnding (B728 B9 PR 3b's
// follow-up, its round 2): the refresh a choice's write asks once its hold of
// its reading has ended (askChoiceRefresh) is made, forced, on the refreshes'
// goroutine, and its end is said (Seams.ChoiceRefreshed), whatever happens to
// the run that takes it: one a later ask on a context that has ended replaced
// while a refresh was held, which the run's own context would skip; one that
// begins between the ask's two marks, its force not set yet, which the idle
// rule would skip; and one that panics past its own containment, which the
// goroutine's recover ends. Where it reads, it publishes the state set after
// the held reading or before the run.
func TestTheRefreshAWriteAsksIsMadeForcedAndHeardEnding(t *testing.T) {
	converted := func(t *testing.T) dsdstate.State {
		st := nativeFixture(t)
		st.DACs[0].Choice = &dsdstate.Choice{Mode: dsdstate.ModeConvert, Revision: 1, SetAt: fixtureT0, SetBy: dsdstate.ByApp}
		return st
	}
	// rig is a backend over box and store whose set is published once, its
	// ChoiceRefreshed seam closing the channel it answers, and log its logger
	// (nil for the package's).
	rig := func(t *testing.T, box DSDBox, store DSDStore, log *slog.Logger) (*Backend, <-chan struct{}) {
		t.Helper()
		pol := &DSDPolicy{Store: store, Kernel: fixtureKernel, Box: box, Probes: alsapcm.NewProbeCache(),
			Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
			Now:   func() time.Time { return fixtureT0 }}
		b, err := New(Config{Resolver: nativeFilesForRefusal{}, Locator: refuseLocator{}, DSD: pol, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		b.RefreshDSD(true)
		refreshed := make(chan struct{})
		var once sync.Once
		b.cfg.Seams = &Seams{ChoiceRefreshed: func() { once.Do(func() { close(refreshed) }) }}
		return b, refreshed
	}
	// heard waits for the seam, bounded, then joins the refresh's goroutine
	// before the seams go: the seam reached is not its goroutine ended.
	heard := func(t *testing.T, b *Backend, refreshed <-chan struct{}) {
		t.Helper()
		select {
		case <-refreshed:
		case <-time.After(10 * time.Second):
			t.Fatal("the end of the refresh the write asked was never said")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := b.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		b.cfg.Seams = nil
	}
	publishesConvert := func(t *testing.T, b *Backend) {
		t.Helper()
		if got := b.dsdPub.Load().view.DAC.Choice.Mode; got != dsdstate.ModeConvert {
			t.Errorf("the refresh the write asked left %s published; want the state set before it ran, convert", got)
		}
	}

	t.Run("replaced by a later ask whose context has ended", func(t *testing.T) {
		box, store := &heldControlsBox{}, &fixtureStore{st: nativeFixture(t)}
		b, refreshed := rig(t, box, store, nil)
		came, let := box.holdNext()
		t.Cleanup(let)
		b.RequestRefreshDSD(context.Background(), true)
		select {
		case <-came:
		case <-time.After(10 * time.Second):
			t.Fatal("the refresh never reached its read of the card's controls")
		}
		if err := store.Update(func(s *dsdstate.State) error { *s = converted(t); return nil }); err != nil {
			t.Fatal(err)
		}
		b.askChoiceRefresh(context.Background())
		ended, cancel := context.WithCancel(context.Background())
		cancel()
		b.RequestRefreshDSD(ended, false)
		let()
		heard(t, b, refreshed)
		publishesConvert(t, b)
	})

	t.Run("begun between the ask's two marks", func(t *testing.T) {
		store := &fixtureStore{st: nativeFixture(t)}
		b, refreshed := rig(t, fixtureBox{}, store, nil)
		if err := store.Update(func(s *dsdstate.State) error { *s = converted(t); return nil }); err != nil {
			t.Fatal(err)
		}
		// What askChoiceRefresh's first mark leaves before its ask sets the
		// force: a run that begins there takes the write's refresh unforced,
		// the last reading as recent as the fixture's clock makes it.
		b.choiceRefresh.Store(true)
		b.RequestRefreshDSD(context.Background(), false)
		heard(t, b, refreshed)
		publishesConvert(t, b)
	})

	t.Run("a refresh that panics past its containment", func(t *testing.T) {
		rec := loggingtest.NewRecorder()
		var once sync.Once
		log := slog.New(hookedLog{Handler: rec.Logger().Handler(), hook: func() {
			// The containment's line panics once, so the refresh panics out of
			// RefreshDSD; the goroutine's own recover says it.
			once.Do(func() { panic("the test's containment panics") })
		}})
		var broken atomic.Bool
		b, refreshed := rig(t, fixtureBox{broken: &broken}, &fixtureStore{st: nativeFixture(t)}, log)
		broken.Store(true)
		b.askChoiceRefresh(context.Background())
		heard(t, b, refreshed)
		if lines := rec.Lines(panicMessage); len(lines) != 1 || !strings.Contains(lines[0], "the effective DSD policy's refresh") {
			t.Errorf("the panic was said %q, want once, as the refresh's", lines)
		}
	})
}
