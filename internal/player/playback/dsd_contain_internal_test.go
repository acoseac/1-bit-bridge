package playback

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

type failedHandoff struct {
	engine.Events
	calls atomic.Int32
}

func (r *failedHandoff) Replan(engine.SessionID, engine.OccurrenceID, string) {
	r.calls.Add(1)
	panic("hand-off")
}

type containSink struct {
	fixtureSink
	positions, drops, closes atomic.Int32
	panicPosition            bool
	stopped                  chan struct{}
}

func (s *containSink) Position() alsapcm.Position {
	s.positions.Add(1)
	if s.panicPosition {
		panic("position")
	}
	return s.fixtureSink.Position()
}
func (s *containSink) Drop() error {
	if s.drops.Add(1) == 1 {
		close(s.stopped)
	}
	return nil
}
func (s *containSink) Close() error { s.closes.Add(1); return s.Drop() }

func waitContained(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("containment did not stop the sink")
	}
}
func checkContained(t *testing.T, s *session) {
	t.Helper()
	if !s.mu.TryLock() {
		t.Fatal("containment left the session lock held")
	}
	defer s.mu.Unlock()
	if s.fault == nil || !s.nativeRevoked {
		t.Fatalf("first failure left session running: fault %+v, revoked %v", s.fault, s.nativeRevoked)
	}
}

// checkContainedWithin is checkContained beside a goroutine that may hold the
// session's lock a moment (a live monitor's next reading, or the containment's
// own Drop of a PCM output, whose deferred clean-up takes the lock to clear
// containDrop after the sink's Drop returned: stopDSD): there a single failed
// TryLock proves no leak, so the lock is taken within handMadeBound, and one
// still held then is the containment's (backlog B853, rounds 4 and 5).
func checkContainedWithin(t *testing.T, s *session) {
	t.Helper()
	if !lockWithin(&s.mu, handMadeBound) {
		t.Fatalf("containment left the session lock held %v", handMadeBound)
	}
	defer s.mu.Unlock()
	if s.fault == nil || !s.nativeRevoked {
		t.Fatalf("first failure left session running: fault %+v, revoked %v", s.fault, s.nativeRevoked)
	}
}

// TestTheContainmentCheckWaitsOutABriefHolderOfTheLock (backlog B853, round
// 5): a goroutine that holds the session's lock a moment is a legitimate
// holder, as the containment's own Drop is, whose deferred clean-up takes the
// lock once the sink's Drop returned (stopDSD): an immediate TryLock fails on
// it, the spurious failure checkContained could give beside that clean-up,
// and checkContainedWithin waits it out. The case's teardown lets the holder
// go and joins it, whatever an assertion did, before the session's end.
func TestTheContainmentCheckWaitsOutABriefHolderOfTheLock(t *testing.T) {
	b, _ := fixtureSessionBackend(t)
	s := b.session(1)
	t.Cleanup(func() { endSession(t, b, s) })
	s.mu.Lock()
	s.revokeDSDLocked(engine.Unplayable{Reason: ReasonFailed, Detail: "the test's containment"})
	s.fault = &engine.Fault{Reason: ReasonFailed, Detail: "the test's containment"}
	s.mu.Unlock()
	holding, letGo, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(letGo) }) }
	t.Cleanup(func() {
		release()
		select {
		case <-ended:
		case <-time.After(handMadeBound):
			t.Errorf("the holder did not let go of the session's lock within %v of its release", handMadeBound)
		}
	})
	go func() {
		defer close(ended)
		s.mu.Lock()
		close(holding)
		<-letGo
		s.mu.Unlock()
	}()
	select {
	case <-holding:
	case <-time.After(handMadeBound):
		t.Fatal("the holder did not take the session's lock")
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("an immediate TryLock took the lock the holder holds: the case needs it held")
	}
	// The holder lets go a moment after the bounded check has begun.
	letGoSoon := time.AfterFunc(10*time.Millisecond, release)
	defer letGoSoon.Stop()
	checkContainedWithin(t, s)
}

func TestAHandOffThatPanicsStopsItsSessionAndTheOthersAreJudged(t *testing.T) {
	var gone atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: &gone} })
	bad := &failedHandoff{}
	s := b.session(1)
	defer s.cancel()
	s.ev = bad
	s.chain = []*occ{openedOcc(b, 2)}
	others := make([]*session, 8)
	for i := range others {
		q := b.session(engine.SessionID(i + 2))
		defer q.cancel()
		q.ev = plainEvents{}
		o := openedOcc(b, engine.OccurrenceID(i+3))
		o.leading = true
		q.chain = []*occ{o}
		others[i] = q
	}
	judged := s.judged
	gone.Store(true)
	b.RefreshDSD(true)
	checkContained(t, s)
	waitContained(t, judged)
	for _, q := range others {
		if !q.nativeRevoked {
			t.Error("another session was not judged")
		}
	}
	b.RefreshDSD(true)
	if bad.calls.Load() != 1 {
		t.Fatalf("failed hand-off retried %d times", bad.calls.Load())
	}
	if b.dsdJudged != b.dsd.Load() {
		t.Fatal("failed session stranded the published verdict")
	}
}
func TestARevocationThatPanicsStopsTheSession(t *testing.T) {
	var gone atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: &gone} })
	var calls atomic.Int32
	b.cfg.Seams = &Seams{Revoking: func(engine.SessionID) { calls.Add(1); panic("revocation") }}
	s := b.session(1)
	defer s.cancel()
	s.ev = plainEvents{}
	o := openedOcc(b, 2)
	o.leading = true
	s.chain = []*occ{o}
	sink := &containSink{stopped: make(chan struct{})}
	s.outs = []*output{{sink: sink}}
	gone.Store(true)
	b.RefreshDSD(true)
	// The containment's Drop of the PCM output may still be in its deferred
	// clean-up, which holds the lock a moment: a bounded check, then both of
	// its counted goroutines (the worker and its call) joined, their real end
	// (round 5: an immediate TryLock could meet that clean-up).
	checkContainedWithin(t, s)
	waitContained(t, sink.stopped)
	joinBackend(t, b)
	b.RefreshDSD(true)
	if calls.Load() != 1 {
		t.Fatal("panicking revocation retried")
	}
}
func TestTheFirstUnreadPolicyStopsEveryDSDSession(t *testing.T) {
	var broken atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{broken: &broken} })
	ss := make([]*session, 3)
	for i := range ss {
		s := b.session(engine.SessionID(i + 1))
		defer s.cancel()
		s.chain = []*occ{openedOcc(b, engine.OccurrenceID(i+1))}
		ss[i] = s
	}
	broken.Store(true)
	if _, err := b.refreshDSD("test"); !errors.Is(err, errDSDUnread) {
		t.Fatalf("first failed read: %v", err)
	}
	for _, s := range ss {
		checkContained(t, s)
	}
	if !b.dsdRead.IsZero() {
		t.Fatal("failed policy remains fresh")
	}
}
func TestARetiringPositionsPanicStopsAndClosesWithoutAnotherRead(t *testing.T) {
	var gone atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: &gone} })
	s := b.session(1)
	defer s.cancel()
	s.ev = plainEvents{}
	s.releasing = true
	sink := &containSink{stopped: make(chan struct{}), panicPosition: true}
	out := &output{sink: sink}
	o := openedOcc(b, 2)
	o.out = out
	o.eof = true
	o.written = 64
	s.chain = []*occ{o}
	s.outs = []*output{out}
	gone.Store(true)
	b.RefreshDSD(true)
	// As the revocation's case: the Drop's clean-up may hold the lock a
	// moment, and its goroutines are joined (round 5).
	checkContainedWithin(t, s)
	waitContained(t, sink.stopped)
	joinBackend(t, b)
	s.closeAtTeardown(out)
	for range 2 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Error("cleanup retried the failed position callback")
				}
			}()
			_ = out.position()
		}()
	}
	if sink.positions.Load() != 1 || sink.closes.Load() != 1 {
		t.Fatalf("position called %d times, closes %d; want one each", sink.positions.Load(), sink.closes.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAnActivationWhoseFirstReadPanicsIsRefused(t *testing.T) {
	var broken atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{broken: &broken} })
	s := b.session(1)
	defer s.cancel()
	o := openedOcc(b, 2)
	s.chain = []*occ{o}
	broken.Store(true)
	if err := s.checkDSDActivation(o); !errors.Is(err, errDSDUnread) {
		t.Fatalf("activation answered %v; want unread policy", err)
	}
	checkContained(t, s)
}

func TestTheMonitorTakesItsNextTickAfterAPanic(t *testing.T) {
	var broken atomic.Bool
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) {
		p.Box = fixtureBox{broken: &broken}
		p.Tick = func() (<-chan time.Time, func()) { return ticks, func() { close(stopped) } }
	})
	s := b.session(1)
	defer s.cancel()
	s.chain = []*occ{openedOcc(b, 2)}
	s.startDSDMonitor()
	broken.Store(true)
	send := func() {
		t.Helper()
		select {
		case ticks <- time.Time{}:
		case <-time.After(5 * time.Second):
			t.Fatal("monitor no longer takes a tick")
		}
	}
	send()
	send() // the second receive proves the failed first reading completed
	// The monitor's second reading runs beside the check and takes the
	// session's lock a moment (round 4).
	checkContainedWithin(t, s)
	close(s.down)
	waitContained(t, stopped)
}

func TestAPublishPanicStopsAnUnjudgedSetWithoutRetry(t *testing.T) {
	var gone atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: &gone} })
	s := b.session(1)
	defer s.cancel()
	r := &replans{}
	s.ev = r
	s.chain = []*occ{openedOcc(b, 2)}
	judged := s.judged
	b.cfg.Seams = &Seams{Published: func() { panic("publish") }}
	gone.Store(true)
	if _, err := b.refreshDSD("publish"); !errors.Is(err, errDSDUnread) {
		t.Fatalf("published panic answered %v", err)
	}
	checkContained(t, s)
	waitContained(t, judged)
	b.cfg.Seams = nil
	b.RefreshDSD(true)
	if len(r.asked()) != 0 {
		t.Fatal("a stopped session's hand-off was retried")
	}
}
