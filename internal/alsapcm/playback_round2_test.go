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
// and a panic in it cannot interrupt Pause, Resume or the release.
func TestPlaybackContinuesTeardownAfterATimerStopPanic(t *testing.T) {
	for _, action := range []string{"teardown", "pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			logs := &lines{}
			var stops atomic.Int32
			r := newRig(t, withLog(logs), func(o *rigOptions) {
				o.pauseRelease = time.Second
				o.afterFunc = func(time.Duration, func()) func() bool {
					return func() bool { stops.Add(1); panic(panicValue) }
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
			switch action {
			case "pause":
				if err := r.be.Pause(bg, start.s); err != nil {
					t.Errorf("second Pause: %v", err)
				}
			case "resume":
				if _, err := r.be.Resume(bg, start.s); err != nil {
					t.Errorf("Resume: %v", err)
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
			want := int32(1)
			if action == "pause" {
				want = 2
			} // second Pause arms a new timer
			if n := stops.Load(); n != want {
				t.Errorf("stops called %d times; want %d", n, want)
			}
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

// TestPlaybackReporterRecoverySendsNoFaultDuringRelease holds the reporter
// before a panic until Release has ended the session's life. Its recovery
// must leave that release alone, without sending a late fault.
func TestPlaybackReporterRecoverySendsNoFaultDuringRelease(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	d := newDirectWith(t, nil, func(cfg *playback.Config) {
		cfg.Log = slog.New(&lines{})
		cfg.Tick = func() (<-chan time.Time, func()) {
			close(entered)
			<-gate
			panic(panicValue)
		}
	})
	t.Cleanup(release)
	d.files.add("a", wav(48000, 24, pattern(1, 100000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	receiveIn(t, entered, "the reporter to start")
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
	d.k.CheckClosedEach(t)
}
