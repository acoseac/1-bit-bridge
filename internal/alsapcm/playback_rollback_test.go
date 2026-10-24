package alsapcm_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestPlaybackResumeRollbackDropsFinalDrain holds Resume's successful reply
// until the fault is recorded and the sink is in its final hardware drain.
// No fake time is granted after that: rollback must stop the buffered sound.
func TestPlaybackResumeRollbackDropsFinalDrain(t *testing.T) {
	for _, heldDrop := range []bool{false, true} {
		name := "dropped"
		if heldDrop {
			name = "drop-deadline"
		}
		t.Run(name, func(t *testing.T) { testResumeRollback(t, true, heldDrop) })
	}
}

// TestPlaybackResumeRollbackKeepsOtherRefusals leaves an xrun's refused
// Pause joined to the fault, without sending Drop before session release.
func TestPlaybackResumeRollbackKeepsOtherRefusals(t *testing.T) {
	testResumeRollback(t, false, false)
}

// testResumeRollback uses the same reporter panic and command seams as
// B523. The direct backend keeps release from hiding the rollback result.
func testResumeRollback(t *testing.T, draining, heldDrop bool) {
	t.Helper()
	logs := &lines{}
	gate := make(chan struct{})
	var once sync.Once
	fault := func() { once.Do(func() { close(gate) }) }
	opened := make(chan *alsapcm.Sink, 1)
	d := newDirectWith(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
		return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			s, err := open(ctx, cfg)
			opened <- s
			return s, err
		}
	}, func(cfg *playback.Config) {
		cfg.Log = slog.New(logs)
		cfg.Tick = func() (<-chan time.Time, func()) {
			<-gate
			panic(panicValue)
		}
	})
	t.Cleanup(func() { d.release(1) })
	t.Cleanup(fault)
	d.files.add("a", wav(48000, 24, pattern(1, 9600, 24)))
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
	drainDone := make(chan error, 1)
	dropGate := make(chan struct{})
	var dropOnce sync.Once
	allowDrop := func() { dropOnce.Do(func() { close(dropGate) }) }
	t.Cleanup(allowDrop)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var drops atomic.Int32
	var before alsapcm.KernelView
	d.k.OnCommand(func(_ alsapcm.SinkConfig, op string) {
		switch op {
		case "resume":
			fault()
			waitOn(t, "the recorded fault", d.rec.ch, func() bool { return count(d.rec.events(), "fault", 0) == 1 })
			if draining {
				go func() { drainDone <- sink.Drain(bg) }()
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
		case "drop":
			drops.Add(1)
			// A Drop in progress must survive caller cancellation.
			cancel()
			if heldDrop {
				<-dropGate
			}
		}
	})
	how, err := d.be.Resume(ctx, 1)
	allowDrop()
	var re *playback.ResumeError
	if how != 0 || err == nil || errors.As(err, &re) {
		t.Errorf("Resume = %v, %v; want the backend fault", how, err)
	}
	for _, e := range d.rec.events() {
		if e.kind == "fault" && (e.fault.Reason != playback.ReasonFailed || err == nil || !strings.Contains(err.Error(), e.fault.Detail)) {
			t.Errorf("Resume answer %v lost the recorded fault %+v", err, e.fault)
		}
	}
	wantReason := alsapcm.ReasonXrun
	if draining {
		wantReason = alsapcm.ReasonBadState
	}
	var pauseErr *alsapcm.Error
	if !errors.As(err, &pauseErr) || pauseErr.Op != "pause" || pauseErr.Reason != wantReason {
		t.Errorf("Resume answer %v; want the refused Pause (%s) joined", err, wantReason)
	}
	if draining {
		if n := drops.Load(); n != 1 {
			t.Errorf("sent %d Drops after the draining Pause refusal; want one", n)
		}
		if heldDrop {
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Errorf("Resume answer %v; want the Drop's rollback deadline joined", err)
			}
		} else if st := sink.Position().State; st != alsapcm.SinkStopped {
			t.Errorf("rollback left sink %v; want stopped before release", st)
		}
		if after := d.k.View(); after.GivenBytes != before.GivenBytes || after.Consumed != before.Consumed {
			t.Errorf("rollback changed kernel bytes/played frames from %d/%d to %d/%d", before.GivenBytes, before.Consumed, after.GivenBytes, after.Consumed)
		}
	} else if n := drops.Load(); n != 0 || sink.Position().State != alsapcm.SinkXrun {
		t.Errorf("other refusal sent %d Drops, sink %v; want no Drop and xrun", n, sink.Position().State)
	}
	// On main, explicitly drop only after the assertions so cleanup cannot
	// hang waiting for a drain for which the test grants no more fake time.
	if draining {
		_ = sink.Drop()
		if drainErr := receiveIn(t, drainDone, "the drain to end on Drop"); alsapcm.ReasonOf(drainErr) != alsapcm.ReasonCancelled {
			t.Errorf("Drain = %v; want dropped", drainErr)
		}
	}
	d.release(1)
	join, stop := context.WithTimeout(bg, alsapcm.Watchdog())
	defer stop()
	if err := d.be.Wait(join); err != nil {
		t.Fatal(err)
	}
	logs.panicLogged(t, backendPanicMessage, "the reporter")
	d.files.allClosed(t)
	d.k.CheckClosedEach(t)
}
