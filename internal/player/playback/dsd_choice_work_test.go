package playback_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A choice's write beside B762's work off the supervisor's loop, made only
// while the player is stopped (B728 B9 PR 2): the configuration lease it
// holds keeps every Start from registering and every probe from taking the
// device slot, a probe that held the slot as it was taken waited for; what a
// written choice asks of the silent probe; a Start that gives its wait for the
// lease up; and the choice's write among the writes of the player's DSD state
// that the player's stop closes.

// signal is a channel a seam closes the first time it is reached.
type signal struct {
	once sync.Once
	ch   chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.ch) }) }

// heldStore is the player's DSD state in memory whose next Update once armed
// waits until gate closes, came closed as it comes; panics, when set, makes
// that Update panic once let go, as a store whose write panics.
type heldStore struct {
	*playbacktest.MemStore
	armed      atomic.Bool
	panics     atomic.Bool
	came, gate chan struct{}
	letOnce    sync.Once
}

func newHeldStore(st dsdstate.State) *heldStore {
	return &heldStore{MemStore: playbacktest.NewMemStore(st), came: make(chan struct{}), gate: make(chan struct{})}
}

func (h *heldStore) Update(fn func(*dsdstate.State) error) error {
	if h.armed.CompareAndSwap(true, false) {
		close(h.came)
		<-h.gate
	}
	if h.panics.CompareAndSwap(true, false) {
		panic("the test's store panics in its write")
	}
	return h.MemStore.Update(fn)
}

func (h *heldStore) let() { h.letOnce.Do(func() { close(h.gate) }) }

// heldRig is a probeRig whose state's store is a heldStore over its own, the
// seams given.
func heldRig(t *testing.T, probe *heldProbe, seams *playback.Seams) (*probeRig, *heldStore) {
	t.Helper()
	r := newProbeRig(t, probe, func(c *playback.Config) { c.Seams = seams })
	store := &heldStore{MemStore: r.store, came: make(chan struct{}), gate: make(chan struct{})}
	r.pol.Store = store
	r.b.RefreshDSD(true)
	t.Cleanup(store.let)
	return r, store
}

// TestAWriteWaitsForAProbeUnderWayAndOwesItsOwn (B728 B9 PR 2): a choice's
// write that finds the silent probe holding the device slot claims the
// configuration lease first, which keeps every Start from registering and
// every new probe out, and waits for the probe, within its request's time,
// before it reads the DAC: a Start asked meanwhile waits for the lease,
// holding nothing, and opens the DAC only once the write has published. The
// probe under way keeps what it asks, in the cache and as the state's
// driver_accepted, which a choice does not change; but it began before the
// write, so it never settles the probe the write owes (dsdProbe.asked), which
// the next ask runs. A run begun before the write that ends asking nothing
// leaves that probe owed too. (#1434 ran the probe beside the write.)
func TestAWriteWaitsForAProbeUnderWayAndOwesItsOwn(t *testing.T) {
	t.Run("a probe under way is waited for, the lease claimed", func(t *testing.T) {
		probe := newHeldProbe(dsd64Frames)
		let := probe.hold(0)
		defer let()
		claimed, waiting := newSignal(), newSignal()
		// The Start, once the lease lets it register, is held before it takes
		// the device slot until the probe has taken its answers: a Start that
		// took the slot first would void them (B762), an ordering this case is
		// not about.
		holding := newHeldOnce()
		defer holding.let()
		var beside atomic.Int32
		r := newProbeRig(t, probe, func(c *playback.Config) {
			c.Seams = &playback.Seams{ChoiceLeased: claimed.fire, Leased: func(engine.SessionID) { waiting.fire() },
				Holding: func(engine.SessionID) { holding.reach() }}
		})
		r.b.RefreshDSD(true)
		store := &besideStore{MemStore: r.store, probe: probe, beside: &beside}
		r.pol.Store = store
		owed := make(chan bool, 1)
		go func() { owed <- r.b.ProbeDSD(context.Background(), ready) }()
		within(t, probe.came, "the probe's call")
		written := make(chan playback.ChoiceResult, 1)
		go func() { written <- r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)) }()
		within(t, claimed.ch, "the write's lease, the probe holding the DAC")
		started := make(chan error, 1)
		go func() { started <- r.b.Start(context.Background(), startOf(t, 1)) }()
		select {
		case <-waiting.ch:
		case res := <-written:
			t.Fatalf("the write answered %s while the probe held the DAC", res.Outcome)
		case err := <-started:
			t.Fatalf("a Start asked while the write held the lease answered %v without waiting for it", err)
		}
		if st := r.b.Stats(); st.Sessions != 0 {
			t.Fatalf("a Start waiting for the lease registered %d sessions", st.Sessions)
		}
		let()
		if within(t, owed, "the probe's end") {
			t.Error("the probe under way answered owed: its answers were dropped")
		}
		wantChoice(t, within(t, written, "the write's answer"), playback.ChoiceWritten)
		within(t, holding.came, "the Start registered once the write let the lease go")
		holding.let()
		if err := within(t, started, "the Start"); err == nil {
			t.Fatal("the Start answered nil: the test's DAC refuses every open")
		}
		if n := beside.Load(); n != 0 {
			t.Errorf("the write's state was written %d times while the probe had the DAC's node open", n)
		}
		if opens, besideProbe := r.sinks.counts(); opens != 1 || besideProbe != 0 {
			t.Errorf("the DAC was opened %d times, %d of them beside the probe; want once, after it", opens, besideProbe)
		}
		if err := r.b.Release(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		r.idle(t)
		if rates, _ := accepted(r.pol, probedAt(2)); !slices.Equal(rates, []int{dsd64Frames}) {
			t.Errorf("the cache keeps DSD_U32_BE accepted at %v, want the probe under way's %d", rates, dsd64Frames)
		}
		if probeEvidence(r.store.State(), playbacktest.DSD64) == nil {
			t.Error("the probe under way wrote no driver_accepted")
		}
		if !r.b.ProbeDSDPending() {
			t.Fatal("the probe that began before the write settled the probe the write owes")
		}
		if r.b.ProbeDSD(context.Background(), ready) {
			t.Error("the probe owed answered owed again once it ran")
		}
		if calls, _ := probe.counts(); calls != 2 || r.b.ProbeDSDPending() {
			t.Errorf("the probe was called %d times and is still owed %v; want the one under way and the write's", calls, r.b.ProbeDSDPending())
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

// besideStore counts the writes made while the probe has the DAC's node open.
type besideStore struct {
	*playbacktest.MemStore
	probe  *heldProbe
	beside *atomic.Int32
}

func (s *besideStore) Update(fn func(*dsdstate.State) error) error {
	if _, open := s.probe.counts(); open > 0 {
		s.beside.Add(1)
	}
	return s.MemStore.Update(fn)
}

// TestAProbeAskedDuringAWriteIsRefusedAndStaysOwed (B728 B9 PR 2): while a
// choice's write holds the configuration lease, from inside its state's write
// to its publish, a probe asked is refused by the device slot
// (ProbeChoiceWrite, one of probeSlotSkips), asks the DAC nothing, and stays
// owed, and a Start asked waits for the lease, registering nothing; the next
// ask once the write has ended runs the probe.
func TestAProbeAskedDuringAWriteIsRefusedAndStaysOwed(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "inside its state's write"
		if after {
			name = "after its state's write, before its publish"
		}
		t.Run(name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames)
			waiting := newSignal()
			seams := &playback.Seams{Leased: func(engine.SessionID) { waiting.fire() }}
			r, store := heldRig(t, probe, seams)
			var asked func()
			started := make(chan error, 1)
			ask := func() {
				if !r.b.ProbeDSD(context.Background(), ready) {
					t.Error("a probe asked during the write answered nothing owed")
				}
				if calls, _ := probe.counts(); calls != 0 {
					t.Errorf("a probe asked during the write asked the DAC %d times", calls)
				}
				go func() { started <- r.b.Start(context.Background(), startOf(t, 1)) }()
				select {
				case <-waiting.ch:
				case err := <-started:
					t.Errorf("a Start asked during the write answered %v without waiting for its lease", err)
				}
			}
			if after {
				var once sync.Once
				seams.ChoiceWritten = func(error) { once.Do(ask) }
			} else {
				store.armed.Store(true)
				asked = ask
			}
			written := make(chan playback.ChoiceResult, 1)
			go func() { written <- r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)) }()
			if asked != nil {
				within(t, store.came, "the write's state's write")
				asked()
				store.let()
			}
			wantChoice(t, within(t, written, "the write's answer"), playback.ChoiceWritten)
			if err := within(t, started, "the Start, once the write let its lease go"); err == nil {
				t.Fatal("the Start answered nil: the test's DAC refuses every open")
			}
			if err := r.b.Release(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if !r.b.ProbeDSDPending() {
				t.Fatal("the probe refused during the write is not owed")
			}
			if r.b.ProbeDSD(context.Background(), ready) {
				t.Error("the probe owed answered owed again once it ran")
			}
			r.idle(t)
			if calls, _ := probe.counts(); calls != 1 || r.b.ProbeDSDPending() {
				t.Errorf("the probe was called %d times and is still owed %v; want run once, after the write", calls, r.b.ProbeDSDPending())
			}
		})
	}
}

// TestAProbeEndingAsAWriteTakesTheLeaseLeavesTheWritesProbeOwed (B728 B9 PR
// 2): the silent probe gives the device slot back (endProbe) before it takes
// its answers (publishProbe); a choice's write that takes the configuration
// lease in that window, the probe parked there, writes and owes a probe of its
// own, which the parked probe's answers, taken after the write, never settle
// (dsdProbe.asked, read as the run began): it stays owed, and the next ask
// runs it.
func TestAProbeEndingAsAWriteTakesTheLeaseLeavesTheWritesProbeOwed(t *testing.T) {
	probe := newHeldProbe(dsd64Frames)
	probed := newHeldOnce()
	defer probed.let()
	r := newProbeRig(t, probe, func(c *playback.Config) { c.Seams = &playback.Seams{Probed: probed.reach} })
	r.b.RefreshDSD(true)
	owed := make(chan bool, 1)
	go func() { owed <- r.b.ProbeDSD(context.Background(), ready) }()
	within(t, probed.came, "the probe's goroutine between its slot's end and its answers")
	wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)), playback.ChoiceWritten)
	probed.let()
	if within(t, owed, "the probe's end") {
		t.Error("the parked probe answered owed: its answers were dropped")
	}
	r.idle(t)
	if !r.b.ProbeDSDPending() {
		t.Fatal("the probe parked before its answers settled the probe the write owes")
	}
	if r.b.ProbeDSD(context.Background(), ready) {
		t.Error("the probe owed answered owed again once it ran")
	}
	if calls, _ := probe.counts(); calls != 2 || r.b.ProbeDSDPending() {
		t.Errorf("the probe was called %d times and is still owed %v; want the parked one and the write's", calls, r.b.ProbeDSDPending())
	}
}

// TestAStartGivingUpItsWaitForTheLeaseLeavesNoRecord (B728 B9 PR 2): a Start
// that comes while a choice's write holds the configuration lease waits for it
// before it registers; released (StartRequest.Released) or its context ended
// while it waits, it acquires nothing, lets its occurrence's reservation go,
// and meets its Release in either order (session.abandon), so no record is
// left: once the write ends, the next write lands.
func TestAStartGivingUpItsWaitForTheLeaseLeavesNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		// cancel says the Start's context ends rather than its release, and
		// releaseFirst that its Release comes before it gives its wait up.
		cancel, releaseFirst bool
		want                 error
	}{
		{"released, its Release before it gives up", false, true, nil},
		{"released, its Release after it gives up", false, false, nil},
		{"its context ended, its Release after it gives up", true, false, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames)
			waiting := newSignal()
			r, store := heldRig(t, probe, &playback.Seams{Leased: func(engine.SessionID) { waiting.fire() }})
			store.armed.Store(true)
			written := make(chan playback.ChoiceResult, 1)
			go func() { written <- r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)) }()
			within(t, store.came, "the write's state's write")
			budget := engine.NewOccurrenceBudget(4)
			req := startOf(t, 1)
			req.First.Reservation = budget.Reserve()
			released := make(chan struct{})
			req.Released = released
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan error, 1)
			go func() { started <- r.b.Start(ctx, req) }()
			within(t, waiting.ch, "the Start's wait for the lease")
			release := func() {
				t.Helper()
				if err := r.b.Release(context.Background(), 1); err != nil {
					t.Fatalf("the Release: %v", err)
				}
			}
			if tc.releaseFirst {
				release()
			}
			if tc.cancel {
				cancel()
			} else {
				close(released)
			}
			err := within(t, started, "the Start's answer")
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("the Start that gave its wait up answered %v; want %v", err, tc.want)
			}
			if !tc.releaseFirst {
				release()
			}
			req.First.Reservation.ReleaseEngine()
			if n := budget.Used(); n != 0 {
				t.Errorf("the abandoned Start's occurrence still holds %d of its budget: its reservation was not let go", n)
			}
			if st := r.b.Stats(); st.Sessions != 0 {
				t.Errorf("%d session records left once the abandoned Start and its Release met", st.Sessions)
			}
			if opens, _ := r.sinks.counts(); opens != 0 {
				t.Errorf("the abandoned Start opened the DAC %d times", opens)
			}
			store.let()
			wantChoice(t, within(t, written, "the write's answer"), playback.ChoiceWritten)
			wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeConvert, 2)), playback.ChoiceWritten)
			r.idle(t)
		})
	}
}

// TestAStartBetweenItsRegistrationAndItsHoldRefusesTheWrite (B728 B9 PR 2): a
// Start registered and parked before it takes the device slot (Seams.Holding),
// where nothing holds the device, refuses a choice's write as
// playback_not_stopped, the state's bytes and revision unchanged: the lease is
// decided by the sessions' records, never by the device's holder alone. Once
// that session is released the write lands.
func TestAStartBetweenItsRegistrationAndItsHoldRefusesTheWrite(t *testing.T) {
	probe := newHeldProbe(dsd64Frames)
	holding := newHeldOnce()
	defer holding.let()
	r := newProbeRig(t, probe, func(c *playback.Config) {
		c.Seams = &playback.Seams{Holding: func(engine.SessionID) { holding.reach() }}
	})
	r.b.RefreshDSD(true)
	started := make(chan error, 1)
	go func() { started <- r.b.Start(context.Background(), startOf(t, 1)) }()
	within(t, holding.came, "the Start between its registration and its hold")
	state, writes, published := r.store.State(), r.store.Writes(), r.b.Policy()
	wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeConvert, 1)), playback.ChoicePlaybackNotStopped)
	if r.store.Writes() != writes || !reflect.DeepEqual(r.store.State(), state) || !reflect.DeepEqual(r.b.Policy(), published) {
		t.Errorf("a refused write changed the state (%d writes, was %d) or the epoch", r.store.Writes(), writes)
	}
	holding.let()
	if err := within(t, started, "the Start"); err == nil {
		t.Fatal("the Start answered nil: the test's DAC refuses every open")
	}
	if err := r.b.Release(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeConvert, 1)), playback.ChoiceWritten)
	r.idle(t)
}

// TestAStartIsAdmittedAfterAWriteThatDidNotLand (B728 B9 PR 2): the
// configuration lease is let go on every way out of a write, so a Start that
// follows a refused write, a write whose state's write failed and a write
// whose store panicked registers at once, waiting for nothing, and the next
// write takes the writes' turn.
func TestAStartIsAdmittedAfterAWriteThatDidNotLand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, r *probeRig, store *heldStore)
	}{
		{"a refused write", func(t *testing.T, r *probeRig, _ *heldStore) {
			wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1, func(q *playback.ChoiceRequest) { q.DACKey = "d1-0000000000000000" })), playback.ChoiceDACChanged)
		}},
		{"a write that failed", func(t *testing.T, r *probeRig, store *heldStore) {
			store.Fail(errors.New("no space left on device"))
			wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1)), playback.ChoicePersistFailed)
			store.Fail(nil)
		}},
		{"a write whose store panicked", func(t *testing.T, r *probeRig, store *heldStore) {
			store.panics.Store(true)
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the write did not panic: the store's panic did not reach it")
					}
				}()
				r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeNative, 1))
			}()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := newHeldProbe(dsd64Frames)
			var waits atomic.Int32
			r, store := heldRig(t, probe, &playback.Seams{Leased: func(engine.SessionID) { waits.Add(1) }})
			tc.write(t, r, store)
			if _, writes := r.b.Running(); writes != 0 {
				t.Errorf("%d writes of the state counted once the write ended", writes)
			}
			if err := r.b.Start(context.Background(), startOf(t, 1)); err == nil {
				t.Fatal("the Start answered nil: the test's DAC refuses every open")
			}
			if n := waits.Load(); n != 0 {
				t.Errorf("the Start waited %d times for a lease the write that ended still held", n)
			}
			if opens, _ := r.sinks.counts(); opens != 1 {
				t.Errorf("the Start opened the DAC %d times; want once", opens)
			}
			if err := r.b.Release(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			wantChoice(t, r.b.ChooseDSD(bg, choiceOf(t, r.b, dsdstate.ModeConvert, 1)), playback.ChoiceWritten)
			r.idle(t)
		})
	}
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
		store := newHeldStore(playbacktest.ChoiceState(dsdstate.ModeNative))
		b := policyBackend(t, playbacktest.NewBox(5), store, func(p *playback.DSDPolicy) { p.Log = &choiceLog{} })
		b.RefreshDSD(true)
		req := choiceOf(t, b, dsdstate.ModeNative, 1)
		defer store.let()
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
		store.let()
		wantChoice(t, within(t, res, "the choice's answer"), playback.ChoiceWritten)
		within(t, closed, "the writes' close once the choice's write returned")
		if store.Writes() != 1 {
			t.Errorf("the state holds %d writes, want the choice's", store.Writes())
		}
	})
}
