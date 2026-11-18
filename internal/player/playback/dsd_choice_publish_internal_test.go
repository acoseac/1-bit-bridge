package playback

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
// of its reading are let go, and the next refresh publishes the state written.
// Before the pair is stored, the panic stands for any (a published set gone);
// after it, the store's state panics at the publication's read.
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
			b.cfg.Seams = &Seams{ChoiceWritten: func(error) {
				if tc.stored {
					store.armed.Store(true)
					return
				}
				saved = b.dsd.Load()
				b.dsd.Store(nil)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res := b.ChooseDSD(ctx, ChoiceRequest{DACKey: key, DACToken: boundToken(t, b), Choice: policy.Choice{Mode: dsdstate.ModeConvert, By: dsdstate.ByApp}})
			// The refresh the containment asks runs on a goroutine of the
			// backend's, counted from the ask on, and reads the seams: it is
			// joined, whatever it publishes, before they go.
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
			b.RefreshDSD(true)
			if got := b.dsdPub.Load().view.DAC.Choice.Mode; got != dsdstate.ModeConvert {
				t.Errorf("the next refresh publishes %s; want the state written, convert", got)
			}
			wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer wcancel()
			if err := b.Wait(wctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
