package playback

import (
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// These assertions use the pre-existing public refresh and teardown paths,
// so the same tests can run against the lifecycle's baseline implementation.
func TestPolicyReadPanicIsContainedOnItsFirstCall(t *testing.T) {
	var broken atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{broken: &broken} })
	s := b.session(1)
	defer s.cancel()
	s.chain = []*occ{openedOcc(b, 2)}
	broken.Store(true)
	panicked := func() (did bool) {
		defer func() {
			if recover() != nil {
				did = true
			}
		}()
		b.RefreshDSD(true)
		return false
	}()
	if panicked {
		t.Error("the first policy-read panic escaped RefreshDSD")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault == nil || !s.nativeRevoked {
		t.Errorf("first failed reading left DSD running: fault %+v, revoked %v", s.fault, s.nativeRevoked)
	}
}

type cleanupPositionSink struct {
	fixtureSink
	reads, closes atomic.Int32
}

func (s *cleanupPositionSink) Position() alsapcm.Position { s.reads.Add(1); panic("retiring position") }
func (s *cleanupPositionSink) Close() error               { s.closes.Add(1); return nil }

func TestRetiringPositionFailureDoesNotStrandTeardown(t *testing.T) {
	b, _ := fixtureSessionBackend(t)
	s := b.session(1)
	defer s.cancel()
	s.releasing = true
	s.nativeRelease = &engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1}}
	sink := &cleanupPositionSink{}
	out := &output{sink: sink}
	o := openedOcc(b, 2)
	o.out = out
	o.written = 64
	s.chain = []*occ{o}
	s.outs = []*output{out}
	s.teardown()
	select {
	case <-s.down:
	default:
		t.Error("a failed final position callback stranded teardown after close")
	}
	if !s.mu.TryLock() {
		t.Error("final position callback left the session lock held")
		return
	}
	s.mu.Unlock()
	if sink.reads.Load() != 1 || sink.closes.Load() != 1 {
		t.Errorf("position calls %d, closes %d; want one each", sink.reads.Load(), sink.closes.Load())
	}
}

type firstHandoffPanic struct {
	engine.Events
	calls atomic.Int32
}

func (r *firstHandoffPanic) Replan(engine.SessionID, engine.OccurrenceID, string) {
	r.calls.Add(1)
	panic("handoff")
}
func TestFirstReconciliationPanicIsContainedAndStopsItsSession(t *testing.T) {
	var gone atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{gone: &gone} })
	s := b.session(1)
	defer s.cancel()
	r := &firstHandoffPanic{}
	s.ev = r
	s.chain = []*occ{openedOcc(b, 2)}
	gone.Store(true)
	panicked := func() (did bool) {
		defer func() {
			if recover() != nil {
				did = true
			}
		}()
		b.RefreshDSD(true)
		return false
	}()
	if panicked {
		t.Error("first reconciliation panic escaped the refresh")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault == nil || !s.nativeRevoked {
		t.Errorf("first reconciliation failure left DSD running: fault %+v, revoked %v", s.fault, s.nativeRevoked)
	}
	if r.calls.Load() != 1 {
		t.Errorf("hand-off calls %d; want one", r.calls.Load())
	}
}
