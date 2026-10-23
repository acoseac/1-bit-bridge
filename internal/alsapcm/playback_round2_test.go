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
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestPlaybackOwnsASinkBeforeItsOpenedNote holds the writer in STATUS as
// the opened note panics. Release must wait for that writer and its node,
// for both Start's sink and the producer's next transport.
func TestPlaybackOwnsASinkBeforeItsOpenedNote(t *testing.T) {
	for _, successor := range []bool{false, true} {
		name := "first"
		target := 1
		if successor {
			name, target = "successor", 2
		}
		t.Run(name, func(t *testing.T) {
			var d *direct
			var sink *alsapcm.Sink
			var opens int
			var unpark func()
			note := make(chan struct{})
			pauseDone := make(chan error, 1)
			d = newDirectWith(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
					s, err := open(ctx, cfg)
					sink = s
					return s, err
				}
			}, func(cfg *playback.Config) {
				cfg.Notify = func(n playback.Note) {
					if n.Kind != playback.NoteOpened {
						return
					}
					opens++
					if opens != target {
						return
					}
					parked, release := d.k.ParkNextStatus()
					unpark = release
					go func() { pauseDone <- sink.Pause() }()
					receiveIn(t, parked, "the opened sink's writer to enter STATUS")
					close(note)
					panic(panicValue)
				}
			})
			d.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
			d.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
			err := d.start(1, "a")
			if successor {
				if err != nil {
					t.Fatal(err)
				}
				next := occurrence(102, 2, "b")
				if err := d.be.Prepare(bg, 1, 101, &next); err != nil {
					t.Fatal(err)
				}
				d.k.Free()
			} else if err == nil {
				t.Error("Start whose opened note panicked answered nil")
			}
			receiveIn(t, note, "the opened note to panic")
			t.Cleanup(func() { unpark(); _ = sink.Close() })
			// A bounded release gives the teardown time to demonstrate that it
			// owns the held writer. Acknowledging before the bound is the defect.
			ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
			err = d.be.Release(ctx, 1)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Release with the opened sink held in STATUS = %v; want unacknowledged", err)
			}
			if count(d.rec.events(), "released", 0) != 0 {
				t.Error("release acknowledged with the device still held")
			}
			unpark()
			receiveIn(t, pauseDone, "the held command to finish")
			ctx, cancel = context.WithTimeout(bg, alsapcm.Watchdog())
			defer cancel()
			if err := d.be.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(d.rec.events(), "released", 0); n != 1 {
				t.Errorf("release acknowledged %d times; want once after the writer ended", n)
			}
			if st := sink.Position().State; st != alsapcm.SinkStopped && st != alsapcm.SinkEnded {
				t.Errorf("sink state %v", st)
			}
			if v := d.k.View(); v.Open || v.Opens != v.Closes {
				t.Errorf("device still held: %+v", v)
			}
			if st := d.be.Stats(); st.Sessions != 0 {
				t.Errorf("session still owns device: %+v", st)
			}
			d.k.CheckClosedEach(t)
		})
	}
}

// TestPlaybackDeliversAFaultQueuedBehindAPanickingEvent parks the reporter
// before its first step, then makes the sink fault. The same step has an
// successor-stage and audible/position reports ahead of its fault; recovery
// must deliver it.
func TestPlaybackDeliversAFaultQueuedBehindAPanickingEvent(t *testing.T) {
	for _, kind := range []string{"prepared", "submitted", "audible", "position"} {
		t.Run(kind, func(t *testing.T) {
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			logs := &lines{}
			opened := make(chan *alsapcm.Sink, 1)
			r := newRig(t, withLog(logs), func(o *rigOptions) {
				o.onTick = func() { <-gate }
				o.openSink = func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
					return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
						s, err := open(ctx, cfg)
						opened <- s
						return s, err
					}
				}
			})
			t.Cleanup(release)
			r.rec.mu.Lock()
			r.rec.panicOn = func(e event) bool { return e.kind == kind }
			r.rec.mu.Unlock()
			r.files.add("a", wav(48000, 24, pattern(1, 9000, 24)))
			r.files.add("b", wav(48000, 24, pattern(2, 100000, 24)))
			r.insert("a", "b")
			r.k.XrunAt(30000)
			r.k.Free()
			played := make(chan error, 1)
			go func() { played <- r.eng.Play(bg) }()
			r.waitCall("Start to return", func(c call) bool { return c.kind == "start" })
			sink := receiveIn(t, opened, "the sink")
			waitOn(t, "the sink to detect the xrun", sink.Changed, func() bool { return sink.Position().State == alsapcm.SinkXrun })
			release()
			f, _ := r.faulted()
			if f.Reason != engine.ReasonXrun {
				t.Errorf("fault %+v; want original xrun", f)
			}
			receiveIn(t, played, "Play to finish")
			r.letGo()
			ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
			defer cancel()
			if err := r.be.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(r.rec.events(), "fault", 0); n != 1 {
				t.Errorf("fault attempted %d times, want once", n)
			}
			logs.panicLogged(t, backendPanicMessage, "the reporter")
		})
	}
}

// TestPlaybackContinuesTeardownAfterATimerStopPanic drives the timer seam
// while paused between transports. Each stored stop is called at most once,
// and its panic fails Pause or Resume without abandoning the release.
func TestPlaybackContinuesTeardownAfterATimerStopPanic(t *testing.T) {
	for _, action := range []string{"teardown", "pause", "resume", "resume-reopened"} {
		t.Run(action, func(t *testing.T) {
			logs := &lines{}
			var stops, arms atomic.Int32
			tm := &timers{}
			r := newRig(t, withLog(logs), func(o *rigOptions) {
				o.pauseRelease = time.Second
				o.afterFunc = func(d time.Duration, f func()) func() bool {
					arms.Add(1)
					tm.afterFunc(d, f)
					return func() bool { stops.Add(1); panic(panicValue) }
				}
				o.openSink = func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
					return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
						if stops.Load() != 0 {
							t.Error("opened a sink after the timer-stop panic")
						}
						return open(ctx, cfg)
					}
				}
			})
			r.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
			r.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
			r.insert("a", "b")
			r.play()
			r.k.Await(t, "a to drain for b", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
			if err := r.eng.Pause(bg); err != nil {
				t.Fatal(err)
			}
			start := r.waitCall("the session", func(c call) bool { return c.kind == "start" })
			if action == "resume-reopened" {
				r.k.Free()
				tm.fire(t)
				r.waitSnap("the device released between transports", func(s *engine.Snapshot) bool { return s.DeviceReleased() })
			}
			var answer error
			switch action {
			case "pause":
				answer = r.be.Pause(bg, start.s)
			case "resume", "resume-reopened":
				var how engine.ResumeResult
				how, answer = r.be.Resume(bg, start.s)
				if how != 0 {
					t.Errorf("Resume after a timer-stop panic answered %v; want no success", how)
				}
			}
			if action != "teardown" {
				var re *playback.ResumeError
				if answer == nil || errors.As(answer, &re) {
					t.Errorf("%s answered %v; want the backend fault", action, answer)
				}
				f, _ := r.faulted()
				if f.Reason != playback.ReasonFailed || answer != nil && answer.Error() != f.Detail {
					t.Errorf("%s answer %v, fault %+v; want the same backend failure", action, answer, f)
				}
			}
			if err := r.eng.Stop(bg); err != nil {
				t.Errorf("Stop: %v", err)
			}
			if _, unconfirmed := r.eng.Snapshot().ReleaseUnconfirmed(); unconfirmed {
				t.Error("release after timer stop panic was not acknowledged")
			}
			r.letGo()
			if st := r.be.Stats(); st.Sessions != 0 {
				t.Errorf("device ownership retained: %+v", st)
			}
			if n := arms.Load(); n != 1 {
				t.Errorf("release timer armed %d times; want once", n)
			}
			if n := stops.Load(); n != 1 {
				t.Errorf("stops called %d times; want once", n)
			}
			if v := r.k.View(); action == "resume-reopened" && v.GivenBytes != 1000*2*2 {
				t.Errorf("gave the DAC %d bytes; want only a's frames", v.GivenBytes)
			}
			logs.panicLogged(t, backendPanicMessage, "the pause release timer's stop")
		})
	}
}

// TestPlaybackResumeKeepsAPausedSuccessorSilentAfterATimerStopPanic holds
// the next transport's open until Pause arms the between-transports timer.
// Making that sink current stops the timer and records its panic. Resume
// must answer that fault and leave the sink paused, before any Release.
func TestPlaybackResumeKeepsAPausedSuccessorSilentAfterATimerStopPanic(t *testing.T) {
	for _, rollbackRefused := range []bool{false, true} {
		name := "paused"
		if rollbackRefused {
			name = "pause-refused"
		}
		t.Run(name, func(t *testing.T) {
			logs := &lines{}
			opened := make(chan *alsapcm.Sink, 1)
			gate, stopped := make(chan struct{}), make(chan struct{})
			var once sync.Once
			allowOpen := func() { once.Do(func() { close(gate) }) }
			d := newDirectWith(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
					s, err := open(ctx, cfg)
					if err == nil && cfg.Transport.RateHz == 48000 {
						opened <- s
						<-gate
					}
					return s, err
				}
			}, func(cfg *playback.Config) {
				cfg.Log = slog.New(logs)
				cfg.AfterFunc = func(time.Duration, func()) func() bool {
					return func() bool { close(stopped); panic(panicValue) }
				}
			})
			t.Cleanup(func() { d.release(1) })
			t.Cleanup(allowOpen)
			d.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
			d.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
			if err := d.start(1, "a"); err != nil {
				t.Fatal(err)
			}
			next := occurrence(102, 2, "b")
			if err := d.be.Prepare(bg, 1, 101, &next); err != nil {
				t.Fatal(err)
			}
			d.k.Free()
			sink := receiveIn(t, opened, "b's sink to open between transports")
			if err := d.be.Pause(bg, 1); err != nil {
				t.Fatal(err)
			}
			allowOpen()
			receiveIn(t, stopped, "the paused successor to become current and stop the timer")
			if rollbackRefused {
				// Close after the writer takes Resume but before its caller reads
				// the answer: Resume succeeds and the compensating Pause refuses.
				d.k.OnCommand(func(_ alsapcm.SinkConfig, op string) {
					if op == "resume" {
						_ = sink.Close()
					}
				})
			}
			before := d.k.View().GivenBytes
			how, err := d.be.Resume(bg, 1)
			var re *playback.ResumeError
			if how != 0 || err == nil || errors.As(err, &re) {
				t.Errorf("Resume = %v, %v; want the backend fault without success", how, err)
			}
			wantState := alsapcm.SinkPaused
			if rollbackRefused {
				wantState = alsapcm.SinkStopped
			}
			if st := sink.Position().State; st != wantState {
				t.Errorf("Resume that meets the timer-stop fault left the sink %v; want %v before release", st, wantState)
			}
			if got := d.k.View().GivenBytes; got != before {
				t.Errorf("gave the DAC %d bytes after the failed Resume; want %d", got, before)
			}
			waitOn(t, "the backend fault", d.rec.ch, func() bool { return count(d.rec.events(), "fault", 0) == 1 })
			for _, e := range d.rec.events() {
				if e.kind == "fault" && (e.fault.Reason != playback.ReasonFailed || err == nil || !strings.Contains(err.Error(), e.fault.Detail)) {
					t.Errorf("Resume answer %v, fault %+v; want the recorded backend failure", err, e.fault)
				}
			}
			if rollbackRefused && alsapcm.ReasonOf(err) != alsapcm.ReasonClosed {
				t.Errorf("Resume answer %v; want the refused Pause folded into the fault", err)
			}
			ctx, cancel := context.WithTimeout(bg, alsapcm.Watchdog())
			defer cancel()
			if err := d.be.Release(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := d.be.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			logs.panicLogged(t, backendPanicMessage, "the pause release timer's stop")
			d.k.CheckClosedEach(t)
		})
	}
}

// TestPlaybackResumePanicIsTheBackendsFault checks the backend's direct
// answer as well as the engine's fault: the dead writer cannot keep a
// resumable paused session or blame the device.
func TestPlaybackResumePanicIsTheBackendsFault(t *testing.T) {
	var admits atomic.Int32
	var answer error
	var r *rig
	logs := &lines{}
	r, _ = pausedRig(t, withLog(logs), func(o *rigOptions) {
		o.openSink = func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				admit := cfg.Admit
				cfg.Admit = func(n alsapcm.NodeInfo) error {
					if admits.Add(1) == 2 {
						panic(panicValue)
					}
					return admit(n)
				}
				return open(ctx, cfg)
			}
		}
	})
	r.releaseDevice()
	start := r.waitCall("the session", func(c call) bool { return c.kind == "start" })
	_, answer = r.be.Resume(bg, start.s)
	var re *playback.ResumeError
	if answer == nil || errors.As(answer, &re) || strings.Contains(answer.Error(), "device_error") {
		t.Errorf("resume answered %v; want backend failure, never ResumeError", answer)
	}
	f, _ := r.faulted()
	if f.Reason != playback.ReasonFailed {
		t.Errorf("fault %+v; want backend_failed", f)
	}
	r.letGo()
	logs.panicLogged(t, sinkPanicMessage, "the sink's writer")
}

// TestPlaybackReporterRecoverySendsNoFaultDuringRelease queues an xrun behind
// an audible callback, then begins release before that callback panics.
// Recovery must suppress the queued fault, without abandoning the release.
func TestPlaybackReporterRecoverySendsNoFaultDuringRelease(t *testing.T) {
	step, entered, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var stepOnce, once sync.Once
	allowStep := func() { stepOnce.Do(func() { close(step) }) }
	release := func() { once.Do(func() { close(gate) }) }
	opened := make(chan *alsapcm.Sink, 1)
	logs := &lines{}
	d := newDirectWith(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
		return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			s, err := open(ctx, cfg)
			opened <- s
			return s, err
		}
	}, func(cfg *playback.Config) {
		cfg.Log = slog.New(logs)
		cfg.Tick = func() (<-chan time.Time, func()) {
			<-step
			return nil, func() {}
		}
	})
	t.Cleanup(release)
	t.Cleanup(allowStep)
	d.rec.panicOn = func(e event) bool {
		if e.kind != "audible" {
			return false
		}
		close(entered)
		<-gate
		return true
	}
	d.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	d.k.XrunAt(30000)
	d.k.Free()
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	sink := receiveIn(t, opened, "the sink")
	waitOn(t, "the sink to detect the xrun", sink.Changed, func() bool { return sink.Position().State == alsapcm.SinkXrun })
	allowStep()
	receiveIn(t, entered, "the event ahead of the queued fault")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := d.be.Release(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("held reporter's release: %v", err)
	}
	release()
	ctx, cancel = context.WithTimeout(bg, alsapcm.Watchdog())
	defer cancel()
	if err := d.be.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(d.rec.events(), "fault", 0); n != 0 {
		t.Errorf("%d faults during release", n)
	}
	if n := count(d.rec.events(), "released", 0); n != 1 {
		t.Errorf("%d release acknowledgements, want one", n)
	}
	logs.panicLogged(t, backendPanicMessage, "the reporter")
	d.k.CheckClosedEach(t)
}
