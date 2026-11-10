package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// pauseMarks is what a Pause registers on a sink: its pause, its epoch (which
// fences a Resume or a paused activation begun before it) and the moment it
// paused (from which the release after thirty seconds is counted).
type pauseMarks struct {
	paused bool
	epoch  uint64
	at     time.Time
}

func marksOf(s *DSDSink) pauseMarks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pauseMarks{paused: s.paused, epoch: s.pauseEpoch, at: s.pauseAt}
}

func (m pauseMarks) same(o pauseMarks) bool {
	return m.paused == o.paused && m.epoch == o.epoch && m.at.Equal(o.at)
}

// wantOwnCancellation fails unless e is a command's own context's
// cancellation: neither a cleanup failure nor the sink's fence.
func wantOwnCancellation(t *testing.T, e error, what string) {
	t.Helper()
	if !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, ErrActivationFenced) {
		t.Errorf("%s answered %v; want its own context's cancellation", what, e)
	}
}

// TestNativeAPauseOnItsOwnEndedContextChangesNothing: a Pause whose context
// ended before the call answers that cancellation and changes nothing, on an
// inert sink, on one already paused, and beside a Resume under way, its STATUS
// parked: the pause, its epoch and its moment stay as they were, so the inert
// sink then activates and the Resume is not fenced (B728 B9 PR 1 round 5: on
// an inert or a paused sink the Pause registered a pause, skipped its wait and
// answered success; beside the Resume it fenced it).
func TestNativeAPauseOnItsOwnEndedContextChangesNothing(t *testing.T) {
	for _, sink := range []string{"inert", "already paused", "resuming"} {
		t.Run(sink, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Prefill = time.Millisecond
			parked, release := make(chan struct{}), make(chan struct{})
			if sink == "resuming" {
				var checks atomic.Int32
				c.Check = func(NodeInfo) error {
					// The open's check, the activation's, then the Resume's,
					// whose STATUS is parked.
					if checks.Add(1) == 3 {
						k.inspect(func(k *fakeKernel) { k.statusParked = parked; k.statusGate = release })
					}
					return nil
				}
			}
			s := nativeSink(t, k, c)
			resumed := make(chan error, 1)
			switch sink {
			case "already paused":
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
			case "resuming":
				data := bytes.Repeat([]byte{0x81}, s.params.BufferFrames*4)
				if _, e := s.WriteDSD(context.Background(), 1, data, data, 0, len(data)*8); e != nil {
					t.Fatal(e)
				}
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "the stream running")
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				go func() { resumed <- s.Resume(context.Background()) }()
				await(t, parked, "the Resume's STATUS, parked")
			}
			before := marksOf(s)
			ended, stop := context.WithCancel(context.Background())
			stop()
			wantOwnCancellation(t, s.Pause(ended), "a Pause on its own context ended")
			if after := marksOf(s); !after.same(before) {
				t.Errorf("the Pause changed the sink's pause from %+v to %+v", before, after)
			}
			switch sink {
			case "inert":
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Errorf("the inert sink's activation answered %v; want it activated, nothing paused", e)
				}
			case "resuming":
				close(release)
				if e := receive(t, resumed, "the Resume's answer"); e != nil {
					t.Errorf("the Resume answered %v; want it resumed, nothing fencing it", e)
				}
				unpaused := false
				k.inspect(func(k *fakeKernel) {
					for _, p := range k.pauses {
						unpaused = unpaused || p == 0
					}
				})
				if !unpaused {
					t.Error("the Resume never unpaused the kernel")
				}
			}
		})
	}
}

// TestNativeAPauseWhoseContextEndsBesideItsChangeAnswersTheCancellation: a
// Pause waits for the writer to apply its pause, the sink's life running, and
// its context ends beside the change that applies it, both ready in its wait.
// The seam has the wait take the change (the context's Done is never ready
// there, its end real all the same), and the Pause, its pause applied, answers
// its context's end, never the success it would have been on a context
// running (B728 B9 PR 1 round 5: whichever arm the select took decided, and
// the change answered nil). Since round 6 the read of the context as Pause
// takes its lock again answers it, and the read after settle stands behind
// that one. A staged sink, as in
// TestNativeWaitingCommandsAnswerTheEndedLifesFailure: no goroutine owns it.
func TestNativeAPauseWhoseContextEndsBesideItsChangeAnswersTheCancellation(t *testing.T) {
	life, end := context.WithCancel(context.Background())
	defer end()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &DSDSink{life: life, cfg: DSDConfig{LedgerLimit: 1}, active: true,
		changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}),
		releaseExpired: make(chan struct{}), seams: nativeSeams{now: time.Now}}
	calls := 0
	wait := verdictWaitContext{Context: ctx} // done nil: the wait takes the change
	wait.atWait = func() {
		calls++
		if calls != 1 {
			t.Fatal("the Pause revisited its wait")
		}
		s.mu.Lock()
		s.pauseApplied = true
		s.notify()
		s.mu.Unlock()
		cancel()
	}
	e := s.Pause(wait)
	if calls != 1 {
		t.Fatal("the Pause never entered its wait")
	}
	wantOwnCancellation(t, e, "a Pause whose context ended beside its change")
	if !marksOf(s).paused {
		t.Error("the premise is a pause registered and applied")
	}
	if life.Err() != nil {
		t.Fatal("the premise is a life still running")
	}
}
