package playback_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A choice's write beside B762's work off the supervisor's loop (B728 B9,
// merged with B762): what a written choice asks of the silent probe the
// backend's goroutines run, and the choice's write among the writes of the
// player's DSD state that the player's stop closes.

// TestAChoiceWrittenBesideAProbeDropsNothingAndStaysAsked: a choice written
// for the DAC makes a probe due and owed, drops none of the probe's answers
// and moves no generation (oweProbe), so a probe under way as it is written
// keeps what it asks, in the cache and as the state's driver_accepted, and
// owes nothing more: what it asked is the probe the choice owed, since a
// choice changes nothing the kernel or the descriptors answer. A run begun
// before the write that ends asking nothing leaves that probe owed, since it
// began before the ask (dsdProbe.asked), and the next ask runs it.
func TestAChoiceWrittenBesideAProbeDropsNothingAndStaysAsked(t *testing.T) {
	t.Run("a probe under way keeps what it asks, which is the probe owed", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames)
		let := probe.hold(0)
		defer let()
		r := newProbeRig(t, probe)
		r.b.RefreshDSD(true)
		owed := make(chan bool, 1)
		go func() { owed <- r.b.ProbeDSD(context.Background(), ready) }()
		within(t, probe.came, "the probe's call")
		wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)), playback.ChoiceWritten)
		if !r.b.ProbeDSDPending() {
			t.Fatal("a written choice owes no probe")
		}
		let()
		if within(t, owed, "the probe's end") {
			t.Error("the probe under way as the choice was written answered owed: its answers were dropped")
		}
		r.idle(t)
		if rates, _ := accepted(r.pol, probedAt(2)); !slices.Equal(rates, []int{dsd64Frames}) {
			t.Errorf("the cache keeps DSD_U32_BE accepted at %v, want the probe under way's %d", rates, dsd64Frames)
		}
		if probeEvidence(r.store.State(), playbacktest.DSD64) == nil {
			t.Error("the probe under way wrote no driver_accepted")
		}
		if r.b.ProbeDSDPending() {
			t.Error("a probe is still owed once the one under way, the probe the choice owed, kept its answers")
		}
		if calls, _ := probe.counts(); calls != 1 {
			t.Errorf("the probe was called %d times, want the one under way alone", calls)
		}
	})
	t.Run("a run begun before the write that asks nothing leaves the probe owed", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames)
		r := newProbeRig(t, probe)
		r.b.RefreshDSD(true)
		came, let := r.bound.holdNext()
		defer let()
		// A run asked on a snapshot that saw no DAC: it reads what its end
		// checks against, then the binding, which the test holds, and then
		// asks nothing.
		owed := make(chan bool, 1)
		go func() { owed <- r.b.ProbeDSD(context.Background(), playback.ProbeConditions{Fresh: true}) }()
		within(t, came, "the run's read of the binding")
		wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)), playback.ChoiceWritten)
		let()
		within(t, owed, "the run's end")
		if !r.b.ProbeDSDPending() {
			t.Fatal("a run begun before the choice's write cleared the probe the write owes")
		}
		if calls, _ := probe.counts(); calls != 0 {
			t.Fatalf("the run asked the DAC %d times, want none", calls)
		}
		if r.b.ProbeDSD(context.Background(), ready) {
			t.Error("the probe owed answered owed again once it ran")
		}
		r.idle(t)
		if calls, _ := probe.counts(); calls != 1 || r.b.ProbeDSDPending() {
			t.Errorf("the probe owed was called %d times and is still owed %v, want run once", calls, r.b.ProbeDSDPending())
		}
	})
}

// heldStore is the player's DSD state in memory whose next Update once armed
// waits until gate closes, came closed as it comes.
type heldStore struct {
	*playbacktest.MemStore
	armed      atomic.Bool
	came, gate chan struct{}
}

func (h *heldStore) Update(fn func(*dsdstate.State) error) error {
	if h.armed.CompareAndSwap(true, false) {
		close(h.came)
		<-h.gate
	}
	return h.MemStore.Update(fn)
}

// TestAChoiceWriteIsAWriteOfTheDSDState: a choice's write, made on its
// caller's goroutine, is one of the writes of the player's DSD state that the
// player's stop closes (CloseDSDWrites, B762 round 2). Once they are closed it
// is not made: persist_failed, the state as it was, no probe owed, its line
// saying why and the gate's own line said once. One under way as they close
// keeps them open, and with them serve's player's share of the data
// directory's lock, until it returns.
func TestAChoiceWriteIsAWriteOfTheDSDState(t *testing.T) {
	t.Run("refused once the writes are closed, nothing written", func(t *testing.T) {
		b, store, log := choiceBackend(t, playbacktest.ChoiceState(dsdstate.ModeNative))
		within(t, b.CloseDSDWrites(), "the writes' close with none under way")
		state, writes := store.State(), store.Writes()
		for range 2 {
			wantChoice(t, b.ChooseDSD(bg, choiceOf(t, b, dsdstate.ModeNative, 1)), playback.ChoicePersistFailed)
		}
		if store.Writes() != writes || !reflect.DeepEqual(store.State(), state) {
			t.Errorf("a write after the close changed the state: %+v, was %+v", store.State(), state)
		}
		if b.ProbeDSDPending() {
			t.Error("a write not made owes a probe")
		}
		lines := log.writes()
		if len(lines) != 2 || lines[0].attrs["why"] != "the player's stop closed the writes of its DSD state" || lines[1].attrs["why"] != lines[0].attrs["why"] {
			t.Errorf("the writes' lines %+v, want two saying the player's stop closed the writes", lines)
		}
		if n := strings.Count(log.everything(), "came after the player's stop closed them"); n != 1 {
			t.Errorf("the gate's line was said %d times, want once", n)
		}
	})
	t.Run("under way as the writes close, it keeps them open until it returns", func(t *testing.T) {
		store := &heldStore{MemStore: playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)), came: make(chan struct{}), gate: make(chan struct{})}
		b := policyBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Log = &choiceLog{} })
		b.RefreshDSD(true)
		req := choiceOf(t, b, dsdstate.ModeNative, 1)
		var once sync.Once
		let := func() { once.Do(func() { close(store.gate) }) }
		defer let()
		store.armed.Store(true)
		res := make(chan playback.ChoiceResult, 1)
		go func() { res <- b.ChooseDSD(bg, req) }()
		within(t, store.came, "the choice's write inside the state's Update")
		closed := b.CloseDSDWrites()
		select {
		case <-closed:
			t.Fatal("the writes closed while a choice's write was under way")
		default:
		}
		if _, writes := b.Running(); writes != 1 {
			t.Errorf("%d writes counted with the choice's under way, want 1", writes)
		}
		let()
		wantChoice(t, within(t, res, "the choice's answer"), playback.ChoiceWritten)
		within(t, closed, "the writes' close once the choice's write returned")
		if store.Writes() != 1 {
			t.Errorf("the state holds %d writes, want the choice's", store.Writes())
		}
	})
}
