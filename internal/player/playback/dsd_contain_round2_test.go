package playback

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

type heldContainSink struct {
	fixtureSink
	drops    atomic.Int32
	in, gate chan struct{}
}

func (s *heldContainSink) Drop() error {
	if s.drops.Add(1) == 1 {
		close(s.in)
	}
	<-s.gate
	return nil
}

func TestRepeatedContainmentKeepsOneDropPastCallerCancellation(t *testing.T) {
	var broken atomic.Bool
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{broken: &broken} })
	s := b.session(1)
	defer s.cancel()
	returned := make(chan struct{}, 64)
	b.cfg.Seams = &Seams{SinkCall: func(context.Context) func(error) {
		return func(error) { returned <- struct{}{} }
	}}
	sink := &heldContainSink{in: make(chan struct{}), gate: make(chan struct{})}
	var once sync.Once
	unhold := func() { once.Do(func() { close(sink.gate) }) }
	t.Cleanup(func() {
		unhold()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.Wait(ctx); err != nil {
			t.Error(err)
		}
	})
	out := &output{sink: sink}
	o := openedOcc(b, 2)
	o.out = out
	s.chain, s.outs = []*occ{o}, []*output{out}
	broken.Store(true)
	b.RefreshDSD(true)
	waitContained(t, sink.in)
	s.cancel()
	waitContained(t, returned) // call returned, but its Drop is still held
	for range 32 {
		b.RefreshDSD(true)
	}
	if n := b.Stats().Goroutines; n > 2 {
		t.Errorf("repeated containment accumulated %d goroutines around one held Drop", n)
	}
	unhold()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n := sink.drops.Load(); n != 1 {
		t.Errorf("repeated containment called Drop %d times; want one in-flight operation", n)
	}
}

func TestContainmentFencesAnAdmissionWithoutWaitingForIt(t *testing.T) {
	for _, inside := range []bool{false, true} {
		name := "while admission is pending"
		if inside {
			name = "from inside admission"
		}
		t.Run(name, func(t *testing.T) {
			var broken atomic.Bool
			b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = fixtureBox{broken: &broken} })
			s := b.session(1)
			defer s.cancel()
			o := s.newOccLocked(engine.Occurrence{ID: 2})
			o.leading = inside
			other := b.session(3)
			defer other.cancel()
			other.chain = []*occ{openedOcc(b, 4)}
			in, gate := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unhold := func() { once.Do(func() { close(gate) }) }
			defer unhold()
			b.cfg.Seams = &Seams{Admitting: func(engine.OccurrenceID) { close(in); <-gate }}
			answer := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				unhold()
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("admission did not join")
				}
			})
			go func() {
				defer close(finished)
				_, err := b.admitDSD(o, format.NativeDSD{BitRateHz: 2822400, Channels: 2})
				answer <- err
			}()
			waitContained(t, in)
			broken.Store(true)
			if !inside {
				refreshed := make(chan struct{})
				go func() { b.RefreshDSD(true); close(refreshed) }()
				waitContained(t, refreshed) // may not wait on the parked admission
				checkContained(t, s)
				// A subsequent successful read must not erase containment.
				broken.Store(false)
			}
			unhold()
			select {
			case err := <-answer:
				var u *engine.Unplayable
				if !errors.As(err, &u) || u.Reason != ReasonDACUnread {
					t.Errorf("contained pending admission answered %v; want unread-policy refusal", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("containment waited for its own admission")
			}
			checkContained(t, s)
			checkContained(t, other)
			s.mu.Lock()
			admitted := o.admitted
			s.mu.Unlock()
			if admitted != nil {
				t.Error("contained admission published an accepted set")
			}
		})
	}
}
