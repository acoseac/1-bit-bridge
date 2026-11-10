package playback

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// A reconciliation or a reading of the effective DSD policy that panics is
// contained and bounded (B728 B9 round 19): no panic reaches a refresh's
// caller (a session's admission, its activation, its monitor, the
// supervisor's refresh, the silent probe's), a reconciliation that panics is
// tried once more by the next refresh and its second failure stops the
// sessions it failed in, and a reading that panics twice in a row stops every
// session that holds DSD.

// containBackend is choiceSessionBackend's backend, the set published, whose
// log is the recorder it returns and whose policy says what it does to the
// stopLog it returns, edit applied to the policy first.
func containBackend(t *testing.T, edit ...func(*DSDPolicy)) (*Backend, *choiceStore, *loggingtest.Recorder, *stopLog) {
	t.Helper()
	stops := &stopLog{}
	b, store := choiceSessionBackend(t, append([]func(*DSDPolicy){func(p *DSDPolicy) { p.Log = stops }}, edit...)...)
	rec := loggingtest.NewRecorder()
	b.log = rec.Logger()
	return b, store, rec, stops
}

// stopLog counts, by session, the policy's lines saying a session's DSD
// stops: a reconciliation's, before it calls Backend.Revoke.
type stopLog struct {
	mu sync.Mutex
	n  map[uint64]int
}

func (l *stopLog) Info(string, ...any) {}

func (l *stopLog) Warn(msg string, args ...any) {
	if !strings.Contains(msg, "its DSD stops") {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i+1 < len(args); i += 2 {
		if id, ok := args[i+1].(uint64); ok && args[i] == "session" {
			if l.n == nil {
				l.n = map[uint64]int{}
			}
			l.n[id]++
		}
	}
}

func (l *stopLog) count(id engine.SessionID) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n[uint64(id)]
}

// choose writes mode as the test DAC's choice into the state, as a write the
// next refresh publishes.
func choose(t *testing.T, store *choiceStore, mode string) {
	t.Helper()
	if err := store.Update(func(st *dsdstate.State) error {
		st.DACs[0].Choice.Mode = mode
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// refresh runs one forced refresh (RefreshDSD) on a goroutine of its own and
// says whether it panicked into its caller; one that does not return within
// 10 s fails the test, a lock a panic left held among the reasons.
func refresh(t *testing.T, b *Backend) (panicked bool) {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		p := true
		defer func() {
			_ = recover()
			done <- p
		}()
		b.RefreshDSD(true)
		p = false
	}()
	select {
	case p := <-done:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("a refresh did not return within 10 s: a lock a panic left held")
		return false
	}
}

// faultOf is s's fault, and whether its DSD was revoked.
func faultOf(s *session) (*engine.Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault == nil {
		return nil, s.nativeRevoked
	}
	f := *s.fault
	return &f, s.nativeRevoked
}

// judgedNow reports whether the set published is the one the last
// reconciliation that completed judged every session by.
func judgedNow(b *Backend) bool {
	b.dsdMu.Lock()
	defer b.dsdMu.Unlock()
	return b.dsdJudged == b.dsd.Load()
}

// panicLines are the backend's lines saying it recovered a panic, where's.
func panicLines(rec *loggingtest.Recorder, where string) []string {
	var out []string
	for _, l := range rec.Lines(panicMessage) {
		if strings.Contains(l, "where="+where) {
			out = append(out, l)
		}
	}
	return out
}

// lockFree reports whether s's lock can be taken now: a panic that left it
// held would fail every step of the session's after it.
func lockFree(s *session) bool {
	if !s.mu.TryLock() {
		return false
	}
	s.mu.Unlock()
	return true
}

// TestAHandOffThatPanicsStopsItsSessionAndTheOthersAreJudged (B728 B9 round
// 19, case a): session A's hand-off of an occurrence the set re-plans panics
// on every call, beside eight sessions each holding an occurrence heard that
// the set revokes. The refresh that publishes the set panics into no caller:
// every other session is judged and revoked whatever order the sessions are
// reconciled in (the backend's map), and A, whose reconciliation failed, is
// tried once more by the next refresh, which finds the set the same. That
// second failure is terminal: A's fault is recorded (backend_failed, the
// backend's panic) and its DSD revoked, its hand-off never asked again, and
// the retry judges no session whose reconciliation completed (each revoked
// session says its DSD stops once). A third refresh does nothing. Eight
// backends, so an order that reaches A before the others is all but certain
// in one of them.
func TestAHandOffThatPanicsStopsItsSessionAndTheOthersAreJudged(t *testing.T) {
	for range 8 {
		b, store, rec, stops := containBackend(t)
		a := b.session(1)
		t.Cleanup(a.cancel)
		ev := &panicReplans{}
		a.ev = ev
		a.chain = []*occ{openedOcc(b, 10)}
		var others []*session
		for i := range 8 {
			s := b.session(engine.SessionID(2 + i))
			t.Cleanup(s.cancel)
			s.ev = &replans{}
			o := openedOcc(b, engine.OccurrenceID(20+i))
			o.out, o.written, o.crossed = &output{sink: choiceSink{pos: alsapcm.Position{Written: 4096, Played: 2048}}}, 4096, true
			s.chain = []*occ{o}
			others = append(others, s)
		}
		choose(t, store, dsdstate.ModeConvert)
		if refresh(t, b) {
			t.Error("the refresh that published the set panicked into its caller")
		}
		for _, s := range others {
			if f, revoked := faultOf(s); !revoked {
				t.Fatalf("session %d, whose occurrence heard the set revokes, was not revoked by the publish's reconciliation (fault %+v): a panic in another session's stopped it", s.id, f)
			}
		}
		if f, _ := faultOf(a); f != nil {
			t.Errorf("A stopped at its reconciliation's first failure (%+v): it is tried once more first", f)
		}
		if n := len(ev.asked()); n != 1 {
			t.Errorf("A's hand-off was asked %d times by the publish's reconciliation, want 1", n)
		}
		if judgedNow(b) {
			t.Error("the set was recorded judged though A's reconciliation panicked")
		}
		if refresh(t, b) {
			t.Error("the refresh that tried A's reconciliation once more panicked into its caller")
		}
		f, revoked := faultOf(a)
		if f == nil || f.Reason != ReasonFailed || !revoked {
			t.Errorf("after its reconciliation failed twice, A's fault %+v and revoked %v; want backend_failed and its DSD revoked", f, revoked)
		}
		if n := len(ev.asked()); n != 2 {
			t.Errorf("A's hand-off was asked %d times, want 2: the publish's reconciliation and the one retry", n)
		}
		if !judgedNow(b) {
			t.Error("the set is not recorded judged once A's reconciliation failed for the second time")
		}
		if refresh(t, b) {
			t.Error("a third refresh panicked into its caller")
		}
		if n := len(ev.asked()); n != 2 {
			t.Errorf("A's hand-off was asked %d times once the set was judged, want 2: the terminal action never hands off", n)
		}
		for _, s := range others {
			if n := stops.count(s.id); n != 1 {
				t.Errorf("session %d's DSD was said to stop %d times, want once: the retry judged a session whose reconciliation completed", s.id, n)
			}
		}
		if n := stops.count(a.id); n != 0 {
			t.Errorf("A's DSD was said to stop %d times by a reconciliation, want none: its judgement was a hand-off", n)
		}
		if l := panicLines(rec, "the effective DSD set's reconciliation"); len(l) != 2 {
			t.Errorf("the reconciliation's panics were logged %d times, want 2 (each of A's two failures): %q", len(l), l)
		}
		if !lockFree(a) {
			t.Fatal("A's lock is held after its reconciliation panicked")
		}
	}
}

// TestARevocationThatPanicsStopsTheSession (B728 B9 round 19, case b):
// Backend.Revoke panics, here at its entry under the session's lock
// (Seams.Revoking), for a session whose occurrence heard the set revokes.
// The panic reaches no refresh's caller and leaves the session's lock free;
// the next refresh tries the reconciliation once more, whose Revoke panics
// again, and that second failure stops the session with its fault
// (backend_failed), the producer and the reporter woken, without calling
// Revoke a third time.
func TestARevocationThatPanicsStopsTheSession(t *testing.T) {
	b, store, rec, _ := containBackend(t)
	var entered atomic.Int32
	b.cfg.Seams = &Seams{Revoking: func(engine.SessionID) {
		entered.Add(1)
		panic("the test's panic in Backend.Revoke")
	}}
	s, _ := heardSession(t, b)
	choose(t, store, dsdstate.ModeConvert)
	if refresh(t, b) {
		t.Error("the refresh whose reconciliation's Revoke panicked panicked into its caller")
	}
	if !lockFree(s) {
		t.Fatal("the session's lock is held after Revoke panicked under it")
	}
	if n := entered.Load(); n != 1 {
		t.Errorf("Revoke was entered %d times by the publish's reconciliation, want 1", n)
	}
	if f, _ := faultOf(s); f != nil {
		t.Errorf("the session stopped at its reconciliation's first failure (%+v): it is tried once more first", f)
	}
	if refresh(t, b) {
		t.Error("the refresh that tried the reconciliation once more panicked into its caller")
	}
	if !lockFree(s) {
		t.Fatal("the session's lock is held after Revoke panicked under it again")
	}
	f, _ := faultOf(s)
	if f == nil || f.Reason != ReasonFailed {
		t.Errorf("after Revoke panicked twice the session's fault is %+v, want backend_failed: the session stops", f)
	}
	if len(s.wakeP) != 1 || len(s.wakeR) != 1 {
		t.Errorf("the producer (%d) and the reporter (%d) were not both woken to the fault", len(s.wakeP), len(s.wakeR))
	}
	if refresh(t, b) {
		t.Error("a third refresh panicked into its caller")
	}
	if n := entered.Load(); n != 2 {
		t.Errorf("Revoke was entered %d times, want 2: the terminal action never calls the callback that panicked", n)
	}
	if !judgedNow(b) {
		t.Error("the set is not recorded judged once the session was stopped")
	}
	if l := panicLines(rec, "the effective DSD set's reconciliation"); len(l) != 2 {
		t.Errorf("the reconciliation's panics were logged %d times, want 2: %q", len(l), l)
	}
}

// activate runs s's activation check of o on a goroutine of its own, bounded
// at 10 s, and says what it answered or that it panicked.
func activate(t *testing.T, s *session, o *occ) (err error, panicked bool) {
	t.Helper()
	type answer struct {
		err      error
		panicked bool
	}
	done := make(chan answer, 1)
	go func() {
		a := answer{panicked: true}
		defer func() {
			_ = recover()
			done <- a
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a.err = s.checkDSDActivation(ctx, o)
		a.panicked = false
	}()
	select {
	case a := <-done:
		return a.err, a.panicked
	case <-time.After(15 * time.Second):
		t.Fatal("the activation's check did not return within 15 s")
		return nil, false
	}
}

// TestAnActivationThatTakesTheRetryDoesNotPanic (B728 B9 round 19, case c):
// the publish's reconciliation of a successor's hand-off panics, and the
// successor's activation is the next refresh, which tries that reconciliation
// once more: its hand-off panics again, and the activation answers, refusing
// the occurrence being handed off again, with no panic. The retry's failure
// stops the session there (backend_failed), its hand-off asked twice.
func TestAnActivationThatTakesTheRetryDoesNotPanic(t *testing.T) {
	b, store, _, _ := containBackend(t)
	s := b.session(1)
	t.Cleanup(s.cancel)
	ev := &panicReplans{}
	s.ev = ev
	o := openedOcc(b, 2)
	s.chain = []*occ{o}
	choose(t, store, dsdstate.ModeConvert)
	if refresh(t, b) {
		t.Error("the refresh that published the set panicked into its caller")
	}
	err, panicked := activate(t, s, o)
	if panicked {
		t.Fatal("the activation whose reading of the policy tried the reconciliation once more panicked")
	}
	if err == nil {
		t.Error("the activation of an occurrence the set re-plans was let through")
	}
	if f, _ := faultOf(s); f == nil || f.Reason != ReasonFailed {
		t.Errorf("the session's fault once the activation's retry failed: %+v, want backend_failed", f)
	}
	if n := len(ev.asked()); n != 2 {
		t.Errorf("the hand-off was asked %d times, want 2: the publish's reconciliation and the activation's retry", n)
	}
}

// onceReplans is replans whose hand-off of first panics the first time it is
// asked: a callback that recovers. It counts each occurrence's asks and the
// asks that returned.
type onceReplans struct {
	engine.Events
	first engine.OccurrenceID
	mu    sync.Mutex
	asks  map[engine.OccurrenceID]int
	rets  map[engine.OccurrenceID]int
}

func newOnceReplans(first engine.OccurrenceID) *onceReplans {
	return &onceReplans{first: first, asks: map[engine.OccurrenceID]int{}, rets: map[engine.OccurrenceID]int{}}
}

func (r *onceReplans) Replan(_ engine.SessionID, o engine.OccurrenceID, _ string) {
	r.mu.Lock()
	r.asks[o]++
	n := r.asks[o]
	r.mu.Unlock()
	if o == r.first && n == 1 {
		panic("the test's panic in a hand-off, once")
	}
	r.mu.Lock()
	r.rets[o]++
	r.mu.Unlock()
}

func (r *onceReplans) counts(o engine.OccurrenceID) (asks, rets int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asks[o], r.rets[o]
}

// TestAHandOffThatRecoversIsAskedOnceAndJudgesTheSet (B728 B9 round 19, case
// d): two successors the set re-plans, the second's hand-off panicking the
// first time alone. The publish's reconciliation hands off the first and
// fails at the second; the next refresh tries it once more, which asks the
// second's hand-off again and not the first's, which returned: exactly one
// hand-off returns for each, nothing is revoked, and the set is recorded
// judged. One more refresh, the set unchanged, asks nothing.
func TestAHandOffThatRecoversIsAskedOnceAndJudgesTheSet(t *testing.T) {
	b, store, _, stops := containBackend(t)
	s := b.session(1)
	t.Cleanup(s.cancel)
	ev := newOnceReplans(3)
	s.ev = ev
	s.chain = []*occ{openedOcc(b, 2), openedOcc(b, 3)}
	choose(t, store, dsdstate.ModeConvert)
	if refresh(t, b) {
		t.Error("the refresh that published the set panicked into its caller")
	}
	if refresh(t, b) {
		t.Error("the refresh that tried the reconciliation once more panicked into its caller")
	}
	for _, o := range []engine.OccurrenceID{2, 3} {
		if _, rets := ev.counts(o); rets != 1 {
			t.Errorf("occurrence %d's hand-off returned %d times, want exactly once", o, rets)
		}
	}
	if f, revoked := faultOf(s); f != nil || revoked || stops.count(s.id) != 0 {
		t.Errorf("a reconciliation whose hand-off recovered revoked or stopped the session: fault %+v, revoked %v, said %d times", f, revoked, stops.count(s.id))
	}
	if !judgedNow(b) {
		t.Error("the set is not recorded judged once its reconciliation completed")
	}
	a2, _ := ev.counts(2)
	a3, _ := ev.counts(3)
	if refresh(t, b) {
		t.Error("a third refresh panicked into its caller")
	}
	if n2, _ := ev.counts(2); n2 != a2 {
		t.Errorf("one more refresh of the set unchanged asked occurrence 2's hand-off again (%d asks, then %d)", a2, n2)
	}
	if n3, _ := ev.counts(3); n3 != a3 {
		t.Errorf("one more refresh of the set unchanged asked occurrence 3's hand-off again (%d asks, then %d)", a3, n3)
	}
}

// panicOnceBox is the box whose reading of the unit's model panics once, the
// first time it is read while armed, which disarms it.
type panicOnceBox struct {
	choiceBox
	armed *atomic.Bool
}

func (p panicOnceBox) UnitModel(card int) (alsa.UnitModel, error) {
	if p.armed.CompareAndSwap(true, false) {
		panic("the test's panic in the box's reading")
	}
	return p.choiceBox.UnitModel(card)
}

// panicStore is the state whose reading panics while armed.
type panicStore struct {
	*choiceStore
	armed *atomic.Bool
}

func (p panicStore) State() dsdstate.State {
	if p.armed.Load() {
		panic("the test's panic in the state's reading")
	}
	return p.choiceStore.State()
}

// TestReadsThatPanicOverAnUnjudgedSetStopWhatHoldsDSD (B728 B9 round 19, case
// e): a set is published whose reconciliation panicked (a hand-off that
// panics once), and every reading of the policy panics from then on, in the
// binding (Bound) or in the state (Store.State), before any refresh comes to
// that set's retry. No refresh panics into its caller, and the first failure
// changes nothing; the second in a row stops the session that holds DSD, with
// its fault (backend_failed) and its DSD revoked, and from then on an
// admission refuses DSD and an activation refuses, each without a panic. The
// run of failures is logged once.
func TestReadsThatPanicOverAnUnjudgedSetStopWhatHoldsDSD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where func(armed *atomic.Bool) func(*DSDPolicy)
	}{
		{"Bound", func(armed *atomic.Bool) func(*DSDPolicy) {
			return func(p *DSDPolicy) {
				bound := p.Bound
				p.Bound = func() (BoundDAC, bool) {
					if armed.Load() {
						panic("the test's panic in the binding's reading")
					}
					return bound()
				}
			}
		}},
		{"Store.State", func(armed *atomic.Bool) func(*DSDPolicy) {
			return func(p *DSDPolicy) { p.Store = panicStore{choiceStore: p.Store.(*choiceStore), armed: armed} }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var armed atomic.Bool
			b, store, rec, _ := containBackend(t, tc.where(&armed))
			s := b.session(1)
			t.Cleanup(s.cancel)
			ev := newOnceReplans(2)
			s.ev = ev
			s.chain = []*occ{openedOcc(b, 2)}
			choose(t, store, dsdstate.ModeConvert)
			if refresh(t, b) {
				t.Error("the refresh that published the set panicked into its caller")
			}
			if judgedNow(b) {
				t.Fatal("the set was recorded judged though its reconciliation panicked")
			}
			armed.Store(true)
			if refresh(t, b) {
				t.Error("a refresh whose reading panicked panicked into its caller")
			}
			if f, _ := faultOf(s); f != nil {
				t.Errorf("the session stopped at the first reading that panicked (%+v): the bound is two in a row", f)
			}
			if refresh(t, b) {
				t.Error("the second refresh whose reading panicked panicked into its caller")
			}
			f, revoked := faultOf(s)
			if f == nil || f.Reason != ReasonFailed || !revoked {
				t.Errorf("after two readings in a row panicked, the session holding DSD has the fault %+v and revoked %v; want backend_failed and its DSD revoked", f, revoked)
			}
			if asks, _ := ev.counts(2); asks != 1 {
				t.Errorf("the hand-off was asked %d times, want 1: no reading came to the set's retry", asks)
			}
			if refresh(t, b) {
				t.Error("a third refresh whose reading panicked panicked into its caller")
			}
			if l := panicLines(rec, "the effective DSD policy's reading"); len(l) != 1 {
				t.Errorf("the run of readings that panicked was logged %d times, want once: %q", len(l), l)
			}
			// What asks for a reading from now on is refused, never panicked.
			admitted, aPanicked := func() (err error, panicked bool) {
				panicked = true
				defer func() { _ = recover() }()
				_, err = b.admitDSD(&occ{id: 9, leading: true}, format.NativeDSD{BitRateHz: dsdstate.DSD64, Channels: 2})
				panicked = false
				return err, panicked
			}()
			var u *engine.Unplayable
			if aPanicked || !errors.As(admitted, &u) || u.Reason != ReasonDACUnread {
				t.Errorf("an admission once the readings failed: %v (panicked %v), want refused %s", admitted, aPanicked, ReasonDACUnread)
			}
			s2 := b.session(2)
			t.Cleanup(s2.cancel)
			s2.ev = &replans{}
			o2 := openedOcc(b, 30)
			s2.chain = []*occ{o2}
			if err, panicked := activate(t, s2, o2); panicked || err == nil {
				t.Errorf("an activation once the readings failed: %v (panicked %v), want refused", err, panicked)
			}
		})
	}
}

// TestTheMonitorTakesItsNextTickAfterAPanic (B728 B9 round 19, case f): a
// session's monitor's reading panics in the box's reading of the unit's
// model (DSDBox.UnitModel), outside any reconciliation. The monitor goes on:
// it takes its next tick, whose reading completes, and the session, one
// failure short of the bound, plays on, the panic logged once.
func TestTheMonitorTakesItsNextTickAfterAPanic(t *testing.T) {
	ticks := make(chan time.Time)
	var armed atomic.Bool
	b, _, rec, _ := containBackend(t, func(p *DSDPolicy) {
		p.Tick = func() (<-chan time.Time, func()) { return ticks, func() {} }
		p.Box = panicOnceBox{armed: &armed}
	})
	s, _ := heardSession(t, b)
	armed.Store(true)
	s.startDSDMonitor()
	tick := func(which string) {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatalf("the monitor took no %s within 10 s", which)
		}
	}
	tick("first tick")
	// The monitor takes another tick only once it is back at its ticks: the
	// first tick's reading, and its panic, are over by then, and the third
	// tick's send waits for the second's reading the same way.
	tick("tick after its reading panicked")
	tick("tick after that")
	if armed.Load() {
		t.Fatal("the monitor's readings never read the unit's model")
	}
	if f, _ := faultOf(s); f != nil {
		t.Errorf("one reading that panicked stopped the session (%+v): the bound is two in a row", f)
	}
	if l := rec.Lines(panicMessage); len(l) != 1 {
		t.Errorf("the monitor's panic was logged %d times, want once: %q", len(l), l)
	}
	close(s.down)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Errorf("the monitor did not end with its session: %v", err)
	}
}
