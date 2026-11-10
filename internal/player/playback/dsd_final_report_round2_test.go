package playback

import (
	"context"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The engine is real; only the already-joined workers and their closed output
// are staged. This exercises the engine's ordinary/token cutoff filtering.
type finalReportBackend struct {
	*Backend
	before  bool
	started chan *session
}

type finalReportEvents struct {
	engine.Events
	panicFault    bool
	panicPosition bool
	attempts      atomic.Int32
}

func (e *finalReportEvents) Fault(id engine.SessionID, f engine.Fault) {
	e.attempts.Add(1)
	if e.panicFault {
		panic("fault observer")
	}
	e.Events.Fault(id, f)
}
func (e *finalReportEvents) Retiring(token engine.ReleaseToken, n engine.NativeEvent) {
	if n.Kind == "position" && e.panicPosition {
		panic("final position event")
	}
	if n.Kind == "fault" {
		e.attempts.Add(1)
		if e.panicFault {
			panic("fault observer")
		}
	}
	if e.Events != nil {
		e.Events.(engine.NativeReleaseEvents).Retiring(token, n)
	}
}

func (b *finalReportBackend) Start(_ context.Context, r engine.StartRequest) error {
	s := b.session(r.Session)
	s.mu.Lock()
	s.started, s.ev, s.released = true, &finalReportEvents{Events: r.Events, panicPosition: true}, r.Released
	o := s.newOccLocked(r.First)
	o.leading = true
	o.written = 64
	o.plan = &engine.NativePlan{Tuple: engine.NativeTuple{Transport: "native", Format: "DSD_U32_BE", BitRate: 2822400}}
	o.nativeFormat.BitRateHz = 2822400
	var sink streamSink = fixtureSink{pos: alsapcm.Position{Written: 64, Played: 32}}
	if b.before {
		sink = &cleanupPositionSink{}
	}
	out := &output{sink: sink, transport: alsapcm.Transport{RateHz: 2822400}, s: s}
	o.out, s.cur, s.outs = out, out, []*output{out}
	close(o.done)
	s.mu.Unlock()
	close(s.startDone)
	r.Events.(engine.NativePlanEvents).NativePlan(r.Session, o.id, *o.plan)
	r.Events.Audible(r.Session, o.id)
	b.started <- s
	return nil
}
func (b *finalReportBackend) Prepare(context.Context, engine.SessionID, engine.OccurrenceID, *engine.Occurrence) error {
	return nil
}
func (b *finalReportBackend) ReleaseNative(ctx context.Context, r engine.ReleaseRequest) error {
	s := b.session(r.Token.Session)
	s.mu.Lock()
	s.fault = &engine.Fault{Reason: engine.ReasonXrun, Detail: "worker's recorded underrun"}
	s.mu.Unlock()
	return b.Backend.ReleaseNative(ctx, r)
}

func TestFinalReportRecoveryReachesTheEngineOnBothSidesOfCutoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("engine storage requires directory fsync; player runs on Linux")
	}
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	for _, before := range []bool{true, false} {
		name := "after cutoff"
		if before {
			name = "before cutoff"
		}
		t.Run(name, func(t *testing.T) {
			b, _ := fixtureSessionBackend(t)
			backend := &finalReportBackend{Backend: b, before: before, started: make(chan *session, 1)}
			dir := t.TempDir()
			e, err := engine.New(engine.Config{Backend: backend, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
			if err != nil {
				t.Fatal(err)
			}
			life, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- e.Run(life) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("engine did not join")
				}
				ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := b.Wait(ctx); err != nil {
					t.Error(err)
				}
			})
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ref, _ := engine.NewReference("staged native occurrence")
			if _, err := e.InsertAfter(ctx, 0, ref, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
				t.Fatal(err)
			}
			if err := e.Play(ctx); err != nil {
				t.Fatal(err)
			}
			var s *session
			select {
			case s = <-backend.started:
			case <-ctx.Done():
				t.Fatal("start did not return its session")
			}
			if err := e.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			waitContained(t, s.down)
			for {
				changed := e.Changed()
				faults := e.Snapshot().Faults()
				if len(faults) != 0 {
					if len(faults) != 1 || faults[0].Reason != engine.ReasonXrun || faults[0].Detail != "worker's recorded underrun" {
						t.Fatalf("engine recorded %+v; want the worker's original fault", faults)
					}
					break
				}
				select {
				case <-changed:
				case <-ctx.Done():
					t.Fatal("final-report recovery lost the worker's fault at the engine cutoff")
				}
			}
			s.mu.Lock()
			_, restored := s.ev.(*finalReportEvents)
			final := s.finalReport
			s.mu.Unlock()
			if !restored || final {
				t.Error("final report left temporary event routing installed")
			}
		})
	}
}

func TestFinalReportRecoveryContainsItsFaultCallback(t *testing.T) {
	for _, where := range []string{"before cutoff", "after cutoff", "fault callback itself", "new backend fault"} {
		t.Run(where, func(t *testing.T) {
			b, _ := fixtureSessionBackend(t)
			s := b.session(1)
			defer s.cancel()
			ev := &finalReportEvents{panicFault: true, panicPosition: where == "after cutoff"}
			s.ev, s.releasing = ev, true
			s.nativeRelease = &engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1}}
			budget := engine.NewOccurrenceBudget(1)
			reservation := budget.Reserve()
			reservation.ReleaseEngine()
			o := openedOcc(b, 2)
			o.reservation = reservation
			var sink streamSink = fixtureSink{pos: alsapcm.Position{Written: 64, Played: 32}}
			if where == "before cutoff" || where == "new backend fault" {
				sink = &cleanupPositionSink{}
			}
			out := &output{sink: sink}
			o.out, o.written = out, 64
			s.chain, s.outs = []*occ{o}, []*output{out}
			if where != "new backend fault" {
				s.fault = &engine.Fault{Reason: engine.ReasonXrun}
			}
			s.teardown()
			select {
			case <-s.down:
			default:
				t.Error("panicking fallback fault callback stranded teardown")
			}
			if budget.Used() != 0 {
				t.Error("panicking fallback retained the occurrence reservation")
			}
			if ev.attempts.Load() != 1 {
				t.Errorf("fault callback attempted %d times; want exactly once", ev.attempts.Load())
			}
			if s.ev != ev || s.finalReport {
				t.Error("panic left temporary reporter state installed")
			}
			if where != "new backend fault" && s.fault.Reason != engine.ReasonXrun {
				t.Error("report panic replaced the worker's fault")
			}
			if where == "new backend fault" && s.fault.Reason != ReasonFailed {
				t.Error("report panic did not record the backend fault")
			}
		})
	}
}
