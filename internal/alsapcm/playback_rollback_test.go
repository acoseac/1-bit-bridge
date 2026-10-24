package alsapcm_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestPlaybackResumeRollbackDropsFinalDrain holds Resume's successful reply
// until the producer's own Drain reaches the final hardware drain and a
// fault is recorded. Rollback must stop sound even when more time is granted.
func TestPlaybackResumeRollbackDropsFinalDrain(t *testing.T) {
	for _, heldDrop := range []bool{false, true} {
		name := "dropped"
		if heldDrop {
			name = "drop-deadline"
		}
		t.Run(name, func(t *testing.T) { testResumeRollback(t, rollbackFinalDrain, heldDrop) })
	}
}

// TestPlaybackResumeRollbackKeepsOtherRefusals leaves an xrun's refused
// Pause joined to the fault, without sending Drop before session release.
func TestPlaybackResumeRollbackKeepsOtherRefusals(t *testing.T) {
	testResumeRollback(t, rollbackXrun, false)
}

// TestPlaybackResumeRollbackKeepsACompletedDrain lets the final drain finish
// while Resume's reply is held, with the sink still owned by the producer.
// Completing before the fault gives a closed refusal; completing after
// Pause refuses but before rollback checks the state keeps bad_state.
// Neither refusal may cause a Drop of the producer's stopped sink.
func TestPlaybackResumeRollbackKeepsACompletedDrain(t *testing.T) {
	t.Run("before-fault", func(t *testing.T) { testResumeRollback(t, rollbackCompletedBeforeFault, false) })
	t.Run("after-pause-refusal", func(t *testing.T) { testResumeRollback(t, rollbackCompletedAfterRefusal, false) })
}

type rollbackWindow uint8

const (
	rollbackXrun rollbackWindow = iota
	rollbackFinalDrain
	rollbackCompletedBeforeFault
	rollbackCompletedAfterRefusal
)

// testResumeRollback uses the same reporter panic and command seams as
// B523. The direct backend keeps release from hiding the rollback result.
func testResumeRollback(t *testing.T, window rollbackWindow, heldDrop bool) {
	t.Helper()
	draining := window != rollbackXrun
	playedOut := window == rollbackCompletedBeforeFault || window == rollbackCompletedAfterRefusal
	logs := &lines{}
	gate := make(chan struct{})
	var once sync.Once
	fault := func() { once.Do(func() { close(gate) }) }
	opened := make(chan *alsapcm.Sink, 1)
	var callMu sync.Mutex
	var rollbackCalls []context.Context
	var rollbackErrors []error
	// Hold the completed Drain's answer before the producer can mark its
	// output drained or close it. Resume still sees the producer's sink.
	var holdNextDrain atomic.Bool
	var drainContext context.Context
	drainCompleted := make(chan error, 1)
	drainReply := make(chan struct{})
	var drainOnce sync.Once
	allowDrain := func() { drainOnce.Do(func() { close(drainReply) }) }
	pauseRefused := make(chan error, 1)
	pauseReply := make(chan struct{})
	var pauseOnce sync.Once
	allowPause := func() { pauseOnce.Do(func() { close(pauseReply) }) }
	drainResult := make(chan error, 1)
	d := newDirectWith(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
		return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			s, err := open(ctx, cfg)
			opened <- s
			return s, err
		}
	}, func(cfg *playback.Config) {
		cfg.Log = slog.New(logs)
		cfg.Seams = &playback.Seams{SinkCall: func(ctx context.Context) func(error) {
			callMu.Lock()
			if holdNextDrain.Load() && drainContext == nil {
				drainContext = ctx
			}
			heldDrain := ctx == drainContext
			callMu.Unlock()
			_, bounded := ctx.Deadline()
			if bounded {
				callMu.Lock()
				rollbackCalls = append(rollbackCalls, ctx)
				callMu.Unlock()
			}
			return func(err error) {
				if bounded {
					callMu.Lock()
					rollbackErrors = append(rollbackErrors, err)
					callMu.Unlock()
				}
				if bounded && window == rollbackCompletedAfterRefusal && alsapcm.ReasonOf(err) == alsapcm.ReasonBadState {
					pauseRefused <- err
					<-pauseReply
				}
				if heldDrain && err == nil && holdNextDrain.CompareAndSwap(true, false) {
					drainCompleted <- err
					<-drainReply
				}
				if alsapcm.ReasonOf(err) == alsapcm.ReasonCancelled {
					drainResult <- err
				}
			}
		}}
		cfg.Tick = func() (<-chan time.Time, func()) {
			<-gate
			panic(panicValue)
		}
	})
	t.Cleanup(func() { d.release(1) })
	t.Cleanup(fault)
	t.Cleanup(allowDrain)
	t.Cleanup(allowPause)
	// Half a second fits in the lookahead plus hardware buffer, so the
	// producer can reach EOF without any fake time (and without an xrun).
	d.files.add("a", wav(48000, 24, pattern(1, 24000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	sink := receiveIn(t, opened, "the sink")
	d.k.Await(t, "the hardware buffer to fill", runningAndWaiting)
	if err := d.be.Pause(bg, 1); err != nil {
		t.Fatal(err)
	}
	if v := d.k.View(); v.State != "PAUSED" || v.Appl <= v.Consumed {
		t.Fatalf("need paused hardware with unplayed frames: %+v", v)
	}
	drainStarted := make(chan struct{}, 1)
	dropGate := make(chan struct{})
	var dropOnce sync.Once
	allowDrop := func() { dropOnce.Do(func() { close(dropGate) }) }
	t.Cleanup(allowDrop)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var drops atomic.Int32
	var before alsapcm.KernelView
	var recorded event
	resumeEntered := make(chan struct{})
	resumeReply := make(chan struct{})
	var resumeOnce sync.Once
	allowResume := func() { resumeOnce.Do(func() { close(resumeReply) }) }
	t.Cleanup(allowResume)
	d.k.OnCommand(func(_ alsapcm.SinkConfig, op string) {
		switch op {
		case "drain":
			drainStarted <- struct{}{}
		case "resume":
			close(resumeEntered)
			<-resumeReply
		case "drop":
			drops.Add(1)
			// A Drop in progress must survive caller cancellation.
			cancel()
			if heldDrop {
				<-dropGate
			}
		}
	})
	if draining {
		// Name the end as the engine does, and wait for the producer to send
		// Drain. Starting a test-owned Drain would miss its exit path.
		holdNextDrain.Store(playedOut)
		if err := d.be.Prepare(bg, 1, 101, nil); err != nil {
			t.Fatal(err)
		}
		receiveIn(t, drainStarted, "the producer's Drain")
	}
	type resumeAnswer struct {
		how engine.ResumeResult
		err error
	}
	answer := make(chan resumeAnswer, 1)
	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		how, err := d.be.Resume(ctx, 1)
		answer <- resumeAnswer{how, err}
	}()
	t.Cleanup(func() {
		allowResume()
		allowPause()
		allowDrain()
		allowDrop()
		cancel()
		receiveIn(t, resumeDone, "Resume's goroutine to exit")
	})
	// The command hook only signals and holds the reply. Every watchdog wait and
	// assertion runs here, so a watchdog failure cannot Goexit the command
	// goroutine and strand Resume. Cleanup always frees both held replies.
	receiveIn(t, resumeEntered, "Resume to reach its command hook")
	if window == rollbackCompletedBeforeFault {
		d.k.Grant(time.Second)
		if err := receiveIn(t, drainCompleted, "the producer's completed Drain"); err != nil {
			t.Fatal(err)
		}
		if st := sink.Position().State; st != alsapcm.SinkStopped {
			t.Fatalf("completed Drain left sink %v; want stopped", st)
		}
	} else if draining {
		// The producer's Drain is already writing out its lookahead.
		// Leave 150 ms unplayed in the final hardware buffer.
		d.k.Grant(350 * time.Millisecond)
		d.k.Await(t, "the final hardware drain", func(v alsapcm.KernelView) bool { return v.State == "DRAINING" && v.Starved })
	} else {
		// With no Drain, running out of buffered frames is an xrun.
		d.k.Grant(time.Second)
		for {
			changed := sink.Changed()
			if sink.Position().State == alsapcm.SinkXrun {
				break
			}
			receiveIn(t, changed, "the sink to report its xrun")
		}
	}
	before = d.k.View()
	fault()
	waitOn(t, "the recorded fault", d.rec.ch, func() bool { return count(d.rec.events(), "fault", 0) >= 1 })
	evs := d.rec.events()
	if len(evs) != 1 || evs[0].kind != "fault" || evs[0].fault.Reason != playback.ReasonFailed {
		t.Fatalf("events at rollback = %+v; want exactly one backend fault", evs)
	}
	recorded = evs[0]
	allowResume()
	if window == rollbackCompletedAfterRefusal {
		// Pause observed finalDrain and refused with bad_state. Before its
		// caller checks Position, the final buffer finishes. The producer
		// is held at SinkCall, so its stopped sink is still current.
		receiveIn(t, pauseRefused, "the rollback Pause refusal")
		d.k.Grant(time.Second)
		if err := receiveIn(t, drainCompleted, "the producer's completed Drain"); err != nil {
			t.Fatal(err)
		}
		if st := sink.Position().State; st != alsapcm.SinkStopped {
			t.Fatalf("completed Drain left sink %v; want stopped", st)
		}
		allowPause()
	}
	got := receiveIn(t, answer, "Resume's answer")
	how, err := got.how, got.err
	allowDrop()
	var re *playback.ResumeError
	if how != 0 || err == nil || errors.As(err, &re) {
		t.Errorf("Resume = %v, %v; want the backend fault", how, err)
	}
	if err == nil || !strings.Contains(err.Error(), recorded.fault.Detail) {
		t.Errorf("Resume answer %v lost the recorded fault %+v", err, recorded.fault)
	}
	wantReason := alsapcm.ReasonXrun
	if draining {
		callMu.Lock()
		calls := append([]context.Context(nil), rollbackCalls...)
		pauseErrors := append([]error(nil), rollbackErrors...)
		callMu.Unlock()
		if playedOut {
			if len(calls) != 1 || len(pauseErrors) != 1 {
				t.Errorf("completed-drain rollback made %d bounded sink calls (%d answers); want only Pause", len(calls), len(pauseErrors))
			} else if want := errors.Join(errors.New(recorded.fault.Detail), pauseErrors[0]); err == nil || err.Error() != want.Error() || !errors.Is(err, pauseErrors[0]) {
				t.Errorf("Resume answer %v; want exactly the fault joined with Pause: %v", err, want)
			}
		} else if len(calls) != 2 {
			t.Errorf("rollback made %d bounded sink calls; want Pause and Drop", len(calls))
		} else {
			pauseDeadline, _ := calls[0].Deadline()
			dropDeadline, _ := calls[1].Deadline()
			if calls[0] != calls[1] || !pauseDeadline.Equal(dropDeadline) {
				t.Errorf("Pause and Drop used different rollback contexts/deadlines: %v / %v", pauseDeadline, dropDeadline)
			}
		}
		wantReason = alsapcm.ReasonBadState
		if window == rollbackCompletedBeforeFault {
			wantReason = alsapcm.ReasonClosed
		}
	}
	var pauseErr *alsapcm.Error
	if !errors.As(err, &pauseErr) || pauseErr.Op != "pause" || pauseErr.Reason != wantReason {
		t.Errorf("Resume answer %v; want the refused Pause (%s) joined", err, wantReason)
	}
	if playedOut {
		if n := drops.Load(); n != 0 || sink.Position().State != alsapcm.SinkStopped {
			t.Errorf("completed-drain refusal sent %d Drops, sink %v; want no Drop and stopped", n, sink.Position().State)
		}
	} else if draining {
		if n := drops.Load(); n != 1 {
			t.Errorf("sent %d Drops after the draining Pause refusal; want one", n)
		}
		if heldDrop {
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Errorf("Resume answer %v; want the Drop's rollback deadline joined", err)
			}
		} else {
			d.k.Grant(50 * time.Millisecond)
			// On the unfixed path, wait until the grant actually plays,
			// so the frames assertion cannot pass just by scheduling luck.
			d.k.Await(t, "the post-rollback grant to play or the kernel to stop", func(v alsapcm.KernelView) bool {
				return v.State != "DRAINING" || v.Consumed > before.Consumed
			})
			if st := sink.Position().State; st != alsapcm.SinkStopped {
				t.Errorf("rollback left sink %v; want stopped before release", st)
			}
			if v := d.k.View(); v.State == "DRAINING" {
				t.Errorf("rollback left kernel DRAINING after a further fake-time grant: %+v", v)
			}
		}
		if after := d.k.View(); after.GivenBytes != before.GivenBytes || after.Consumed != before.Consumed {
			t.Errorf("rollback changed kernel bytes/played frames from %d/%d to %d/%d", before.GivenBytes, before.Consumed, after.GivenBytes, after.Consumed)
		}
	} else if n := drops.Load(); n != 0 || sink.Position().State != alsapcm.SinkXrun {
		t.Errorf("other refusal sent %d Drops, sink %v; want no Drop and xrun", n, sink.Position().State)
	}
	// End an unfixed drain only after the assertions; release must then join
	// the producer that received the cancelled Drain, as it does in production.
	allowDrain()
	if draining && !playedOut {
		_ = sink.Drop()
		receiveIn(t, drainResult, "the producer's cancelled Drain result")
	}
	d.release(1)
	join, stop := context.WithTimeout(bg, alsapcm.Watchdog())
	defer stop()
	if err := d.be.Wait(join); err != nil {
		t.Fatal(err)
	}
	if evs := d.rec.events(); !reflect.DeepEqual(evs, []event{recorded}) {
		t.Errorf("events after release = %+v; want exactly the original fault and nothing after it: %+v", evs, recorded)
	}
	logs.panicLogged(t, backendPanicMessage, "the reporter")
	d.files.allClosed(t)
	d.k.CheckClosedEach(t)
}
