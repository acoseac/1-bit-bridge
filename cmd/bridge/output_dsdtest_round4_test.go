package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

type nativeObservedContext struct {
	context.Context
	checked chan struct{}
}

func (c nativeObservedContext) Err() error {
	e := c.Context.Err()
	select {
	case c.checked <- struct{}{}:
	default:
	}
	return e
}

func TestNativeFinalAttachmentFence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, gate := make(chan struct{}), make(chan struct{})
	checked := make(chan struct{}, 4)
	controls := make(chan struct{}, 1)
	env := outputDSDTestEnv{attachment: func(int) (qualification.Attachment, error) {
		close(entered)
		<-gate
		return qualification.Attachment{}, nil
	}, controls: func(int) (alsactl.Snapshot, error) { controls <- struct{}{}; return alsactl.Snapshot{}, nil }}
	result := make(chan error, 1)
	go func() {
		_, e := validateNativeFinal(nativeObservedContext{ctx, checked}, env, 0, qualification.Attachment{}, "")
		result <- e
	}()
	<-entered
	cancel()
	if e := <-result; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	for len(checked) > 0 {
		<-checked
	}
	close(gate)
	select {
	case <-controls:
		t.Fatal("controls read after cancelled attachment returned")
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("late attachment worker did not observe cancellation")
	}
}

type nativeBoundarySink struct {
	*nativeCompleteFake
	keys         chan byte
	keyOnRelease bool
	positionRead chan struct{}
	positionOnce sync.Once
	failAt       uint64
}

func (s *nativeBoundarySink) WriteDSD(ctx context.Context, id uint64, l, r []byte, off, n int) (int, error) {
	if id == s.failAt {
		return 0, io.ErrUnexpectedEOF
	}
	return s.nativeCompleteFake.WriteDSD(ctx, id, l, r, off, n)
}
func (s *nativeBoundarySink) Finish(context.Context) error {
	if !s.keyOnRelease {
		s.keys <- 'p'
		if s.positionRead != nil {
			<-s.positionRead
		}
	}
	return nil
}
func (s *nativeBoundarySink) Position() alsapcm.DSDPosition {
	if s.positionRead != nil {
		s.positionOnce.Do(func() { close(s.positionRead) })
	}
	return s.nativeCompleteFake.Position()
}
func (s *nativeBoundarySink) Release(ctx context.Context, m alsapcm.ReleaseMode) error {
	if s.keyOnRelease {
		s.keys <- 'p'
	}
	return s.nativeHarnessFake.Release(ctx, m)
}

func TestNativeFirstPreviousAtNaturalBoundary(t *testing.T) {
	for _, mode := range []string{"playback", "post_done"} {
		t.Run(mode, func(t *testing.T) {
			h := harnessRig(t)
			o, _ := parseNativeOptions(h.args, io.Discard)
			o.list = []string{"dsd", "dsd"}
			o.scenario = "matrix"
			fixtures := map[string]qualification.Fixture{}
			for _, f := range h.manifest.Fixtures {
				fixtures[f.ID] = f
			}
			keys := make(chan byte, 1)
			h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
				return &nativeBoundarySink{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: fixtures["dsd"].Frames}, keys: keys, keyOnRelease: mode == "post_done", positionRead: make(chan struct{})}, nil
			}
			var report nativeReport
			_ = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report)
			if report.Navigated || len(report.IgnoredKeys) != 1 || report.IgnoredKeys[0] != "p:first-fixture" {
				t.Fatalf("first fixture boundary p: navigated=%v ignored=%v", report.Navigated, report.IgnoredKeys)
			}
		})
	}
}

func TestNativeCoverageOnlyFedOccurrences(t *testing.T) {
	h := harnessRig(t)
	o, _ := parseNativeOptions(h.args, io.Discard)
	o.list = []string{"dsd", "dsd"}
	o.scenario = "matrix"
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		return &nativeBoundarySink{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: 3 * fixtures["dsd"].Frames}, failAt: 2}, nil
	}
	var report nativeReport
	_ = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, nil, &report)
	if !report.Completed[0] || report.Completed[1] {
		t.Fatalf("unfed duplicate completed: %v", report.Completed)
	}
}

func TestNativePromptExactBytes(t *testing.T) {
	for _, v := range []string{" PCM audible ", "PCM\taudible", "PCM é audible"} {
		t.Run(v, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			var echo bytes.Buffer
			c := newNativeConsole(ctx, bytes.NewBufferString(v+"\n"), cancel, true, &echo)
			got, e := c.confirm(ctx, io.Discard, "confirm")
			c.close()
			if bytes.ContainsAny([]byte(v), "\té") {
				if e == nil || !bytes.Contains(echo.Bytes(), []byte("printable ASCII")) {
					t.Fatalf("invalid input silently accepted: %q %v echo=%q", got, e, echo.String())
				}
			} else if got != v || e != nil {
				t.Fatalf("echoed bytes changed: %q %v", got, e)
			}
		})
	}
}

type nativeReleaseFenceSink struct {
	*nativeCompleteFake
	releasing, allow chan struct{}
	releaseErr       error
}

func (s *nativeReleaseFenceSink) Finish(context.Context) error {
	return errors.Join(alsapcm.ErrReleaseUnconfirmed, context.Canceled)
}
func (s *nativeReleaseFenceSink) Release(context.Context, alsapcm.ReleaseMode) error {
	close(s.releasing)
	<-s.allow
	if s.releaseErr == nil {
		close(s.done)
	}
	return s.releaseErr
}
func TestNativeCancelledFinishJoinsCleanup(t *testing.T) {
	for _, mode := range []string{"acknowledged", "expired"} {
		t.Run(mode, func(t *testing.T) {
			h := harnessRig(t)
			o, _ := parseNativeOptions(h.args, io.Discard)
			o.list = []string{"dsd"}
			fixtures := map[string]qualification.Fixture{}
			for _, f := range h.manifest.Fixtures {
				fixtures[f.ID] = f
			}
			s := &nativeReleaseFenceSink{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{events: h.events, now: h.env.now, done: make(chan struct{})}}, releasing: make(chan struct{}), allow: make(chan struct{})}
			if mode == "expired" {
				s.releaseErr = alsapcm.ErrReleaseUnconfirmed
			}
			h.env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) { return s, nil }
			result := make(chan error, 1)
			go func() {
				result <- runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, nil, &nativeReport{})
			}()
			select {
			case e := <-result:
				t.Fatalf("returned before cleanup acknowledgement or expiry: %v", e)
			case <-s.releasing:
			}
			select {
			case e := <-result:
				t.Fatalf("cleanup outstanding: %v", e)
			default:
			}
			close(s.allow)
			e := <-result
			if errors.Is(e, alsapcm.ErrReleaseUnconfirmed) != (mode == "expired") {
				t.Fatal("cleanup evidence", e)
			}
		})
	}
}

func TestNativeWorkerInjectedBudget(t *testing.T) {
	for _, kind := range []string{"pcm", "native"} {
		for _, mode := range []string{"cancel", "near_deadline", "expired"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				entered, gate := make(chan struct{}), make(chan struct{})
				defer close(gate)
				armed := make(chan time.Time, 1)
				ticks := make(chan time.Time)
				now := time.Unix(10, 0)
				deadline := now.Add(time.Second)
				if mode == "cancel" {
					deadline = now.Add(20 * time.Second)
				}
				env := outputDSDTestEnv{deadline: deadline, now: func() time.Time { return now }, at: func(at time.Time) (<-chan time.Time, func()) { armed <- at; return ticks, func() {} }}
				result := make(chan error, 1)
				go func() {
					if kind == "pcm" {
						result <- nativePCMCall(ctx, env, func(context.Context) error { close(entered); <-gate; return nil })
					} else {
						env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
							close(entered)
							<-gate
							return nil, context.Canceled
						}
						_, e := nativeOpenCall(ctx, env, alsapcm.DSDConfig{})
						result <- e
					}
				}()
				<-entered
				if mode == "expired" {
					cancel(context.DeadlineExceeded)
				} else {
					cancel(context.Canceled)
				}
				if mode != "expired" {
					select {
					case at := <-armed:
						want := now.Add(2 * time.Second)
						if deadline.Before(want) {
							want = deadline
						}
						if !at.Equal(want) {
							t.Errorf("cleanup deadline %v want %v", at, want)
						}
					case e := <-result:
						t.Fatalf("returned before injected expiry: %v", e)
					case <-time.After(time.Second):
						t.Fatal("worker ignored injected clock")
					}
					select {
					case e := <-result:
						t.Fatalf("returned early: %v", e)
					default:
					}
					close(ticks)
				}
				select {
				case e := <-result:
					if !errors.Is(e, alsapcm.ErrReleaseUnconfirmed) {
						t.Fatal(e)
					}
				case <-time.After(time.Second):
					t.Fatal("worker waited beyond injected budget")
				}
			})
		}
	}
}

func TestNativePromptTerminators(t *testing.T) {
	for _, term := range []string{"\r", "\n", "\r\n"} {
		t.Run([]string{"CR", "LF", "CRLF"}[map[string]int{"\r": 0, "\n": 1, "\r\n": 2}[term]], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			c := newNativeConsole(ctx, nativePipeInput{r}, cancel, true, nil)
			defer c.close()
			result := make(chan nativeLine, 1)
			go func() { v, e := c.confirm(ctx, io.Discard, "first"); result <- nativeLine{v, e} }()
			_, _ = io.WriteString(w, "PCM audibleX\b"+term[:1])
			if v := <-result; v.text != "PCM audible" || v.err != nil {
				t.Fatal(v)
			}
			// Re-arm the next prompt before delivering the LF of a split CRLF.
			c.prompt.Store(true)
			go func() { v, e := c.confirm(ctx, io.Discard, "second"); result <- nativeLine{v, e} }()
			if len(term) == 2 {
				_, _ = io.WriteString(w, term[1:])
			}
			go func() { _, _ = io.WriteString(w, "next\n") }()
			if v := <-result; v.text != "next" || v.err != nil {
				t.Fatalf("terminator submitted an extra line: %+v", v)
			}
		})
	}
}

type nativeOpenFenceSink struct {
	*nativeCompleteFake
	releaseOnce    sync.Once
	entered, allow chan struct{}
	fault          error
}

func (s *nativeOpenFenceSink) BeginRelease(alsapcm.ReleaseMode) {
	s.releaseOnce.Do(func() { close(s.entered); go func() { <-s.allow; close(s.done) }() })
}
func (s *nativeOpenFenceSink) Release(context.Context, alsapcm.ReleaseMode) error {
	s.BeginRelease(alsapcm.Immediate)
	<-s.done
	return s.fault
}
func TestNativeOpenCleanupFence(t *testing.T) {
	for _, mode := range []string{"acknowledged", "expired", "fault"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &nativeOpenFenceSink{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{done: make(chan struct{})}}, entered: make(chan struct{}), allow: make(chan struct{})}
			fault := errors.New("opened sink cleanup failed")
			if mode == "fault" {
				s.fault = errors.Join(alsapcm.ErrReleaseUnconfirmed, fault)
			}
			armed := make(chan struct{}, 1)
			ticks := make(chan time.Time)
			env := outputDSDTestEnv{now: func() time.Time { return time.Unix(0, 0) }, deadline: time.Unix(5, 0), at: func(time.Time) (<-chan time.Time, func()) { armed <- struct{}{}; return ticks, func() {} }}
			env.open = func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) { cancel(); return s, nil }
			result := make(chan error, 1)
			go func() {
				got, e := nativeOpenCall(ctx, env, alsapcm.DSDConfig{})
				if got != nil {
					e = errors.New("cancelled opener returned an owned sink")
				}
				result <- e
			}()
			select {
			case <-s.entered:
			case e := <-result:
				t.Fatalf("opened sink fence dropped: %v", e)
			case <-time.After(time.Second):
				t.Fatal("cleanup never started")
			}
			select {
			case e := <-result:
				t.Fatalf("opened sink's cleanup fence dropped: %v", e)
			default:
			}
			if mode == "expired" {
				select {
				case <-armed:
				case <-time.After(time.Second):
					close(s.allow)
					t.Fatal("cleanup did not use injected clock")
				}
				close(ticks)
			} else {
				close(s.allow)
			}
			select {
			case e := <-result:
				if mode == "expired" {
					close(s.allow)
					if !errors.Is(e, alsapcm.ErrReleaseUnconfirmed) {
						t.Fatal(e)
					}
				}
				if mode == "fault" && !errors.Is(e, fault) {
					t.Fatal("cleanup failure lost", e)
				}
				if mode == "acknowledged" && errors.Is(e, alsapcm.ErrReleaseUnconfirmed) {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("opener exceeded its completion fence/budget")
			}
			<-s.done
		})
	}
}

type nativeMultiSink struct {
	*nativeCompleteFake
	keys  chan byte
	calls int
	first bool
}

func (s *nativeMultiSink) WriteDSD(ctx context.Context, id uint64, l, r []byte, off, n int) (int, error) {
	s.calls++
	if s.calls == 1 {
		return s.nativeCompleteFake.WriteDSD(ctx, id, l, r, off, n)
	}
	if s.first {
		s.keys <- 'n'
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 0, io.ErrUnexpectedEOF
}
func TestNativeCoverageAcrossSinks(t *testing.T) {
	h := harnessRig(t)
	o, _ := parseNativeOptions(h.args, io.Discard)
	o.list = []string{"dsd", "dsd", "dsd"}
	fixtures := map[string]qualification.Fixture{}
	for _, f := range h.manifest.Fixtures {
		fixtures[f.ID] = f
	}
	keys := make(chan byte, 1)
	opens := 0
	h.env.open = func(_ context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
		opens++
		return &nativeMultiSink{nativeCompleteFake: &nativeCompleteFake{nativeHarnessFake: &nativeHarnessFake{cfg: c, events: h.events, now: h.env.now, done: make(chan struct{})}, played: 4 * fixtures["dsd"].Frames}, keys: keys, first: opens == 1}, nil
	}
	var report nativeReport
	_ = runNativeQueue(context.Background(), o, h.env, alsapcm.Endpoint{}, nil, fixtures, keys, &report)
	if opens != 2 {
		t.Fatalf("opens=%d", opens)
	}
	for i, complete := range report.Completed {
		if complete {
			t.Errorf("partial occurrence %d completed across sinks: %v", i, report.Completed)
		}
	}
}
