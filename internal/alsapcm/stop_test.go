package alsapcm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDrainWaitsForTheKernelToFinish pins a drain on the non-blocking node:
// DRAIN returns EAGAIN at once, and the drain waits, re-asking the state at
// least once a period, until the stream is set up again. The wait asks for
// no event: the kernel raises POLLERR when the drain ends.
func TestDrainWaitsForTheKernelToFinish(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.states = []int32{stateDraining, stateDraining, stateSetup}
	k.polls = []fakePoll{{timeout: true}, {revents: pollErr}}
	if err := d.Drain(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if k.callsOf(ioctlDrain) != 1 || k.callsOf(ioctlDrop) != 0 {
		t.Errorf("issued %#x: want DRAIN once and no DROP", k.calls)
	}
	for i, ev := range k.pollEvents {
		if ev != 0 {
			t.Errorf("poll %d asked for %#x, want no event", i, ev)
		}
		if w := k.pollWaits[i]; w > d.Params().PeriodTime() || w <= 0 {
			t.Errorf("poll %d waits %v, want at most a period (%v)", i, w, d.Params().PeriodTime())
		}
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestDrainDropsAtItsDeadline pins that a drain never waits without a
// bound: a stream still draining when the deadline passes on the clock is
// dropped, the drain reports it, and the waits before it are bounded.
func TestDrainDropsAtItsDeadline(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.states = make([]int32, 1000)
	for i := range k.states {
		k.states[i] = stateDraining
	}
	k.polls = make([]fakePoll, 1000)
	for i := range k.polls {
		k.polls[i] = fakePoll{timeout: true}
	}
	start := k.now()
	err := d.Drain(context.Background(), time.Second)
	wantReason(t, err, ReasonDrainTimeout)
	if k.callsOf(ioctlDrop) != 1 {
		t.Errorf("DROP issued %d times, want once at the deadline", k.callsOf(ioctlDrop))
	}
	if waited := k.now().Sub(start); waited < time.Second || waited > time.Second+d.Params().PeriodTime() {
		t.Errorf("waited %v on the clock, want the 1s deadline", waited)
	}
	if limit := int(time.Second/d.Params().PeriodTime()) + 1; len(k.pollWaits) > limit {
		t.Errorf("polled %d times, more than %d", len(k.pollWaits), limit)
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestDrainCancelledDrops pins that a drain whose context ends while it
// waits stops waiting at once and drops the stream.
func TestDrainCancelledDrops(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.states = []int32{stateDraining}
	k.polls = []fakePoll{{block: true}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Drain(ctx, time.Minute) }()
	<-k.entered
	cancel()
	err := <-done
	wantReason(t, err, ReasonCancelled)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%v does not wrap context.Canceled", err)
	}
	if k.callsOf(ioctlDrop) != 1 {
		t.Errorf("DROP issued %d times, want once", k.callsOf(ioctlDrop))
	}
	if len(k.pollWaits) != 1 {
		t.Errorf("waited %d times, want the one wait the cancel ended", len(k.pollWaits))
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestDrainTypesWhatEndsIt pins that a drain the stream ends some other way
// is typed: an xrun, a suspend, a device gone (the state, ENODEV from
// STATUS, POLLHUP from the wait).
func TestDrainTypesWhatEndsIt(t *testing.T) {
	for name, c := range map[string]struct {
		set  func(*fakeKernel)
		want Reason
	}{
		"xrun":        {func(k *fakeKernel) { k.states = []int32{stateXrun} }, ReasonXrun},
		"suspended":   {func(k *fakeKernel) { k.states = []int32{stateSuspended} }, ReasonSuspended},
		"gone":        {func(k *fakeKernel) { k.states = []int32{stateDisconnected} }, ReasonDisconnected},
		"ENODEV":      {func(k *fakeKernel) { k.fail = map[uint32]error{ioctlStatus: errnoENODEV} }, ReasonDisconnected},
		"POLLHUP":     {func(k *fakeKernel) { k.states, k.polls = []int32{stateDraining}, []fakePoll{{revents: pollHup}} }, ReasonDisconnected},
		"DRAIN fails": {func(k *fakeKernel) { k.fail = map[uint32]error{ioctlDrain: errnoENODEV} }, ReasonDisconnected},
		// Linux answers every PCM ioctl, STATUS included, with EBADFD once
		// the runtime is DISCONNECTED (one PCM removed, not the whole card).
		"EBADFD": {func(k *fakeKernel) {
			k.fail = map[uint32]error{ioctlDrain: errnoEBADFD, ioctlStatus: errnoEBADFD}
		}, ReasonDisconnected},
	} {
		k := newFake(t)
		d := mustOpen(t, k, cfg48k)
		c.set(k)
		if err := d.Drain(context.Background(), time.Second); ReasonOf(err) != c.want {
			t.Errorf("%s: got %v, want %s", name, err, c.want)
		}
		_ = d.Close()
		k.checkClosedOnce(t)
	}
}

// TestDrainAfterAnUnderrunIsAnXrun pins that a stream which underran after
// its last write reports the xrun: DRAIN moves an XRUN stream to SETUP and
// answers EAGAIN, so a drain that asked the state only after it read that
// SETUP as a clean end, and a gap in the sound passed as a clean run (the
// Claude API review and Codex on #1301). A stopped stream is not drained.
func TestDrainAfterAnUnderrunIsAnXrun(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.state = stateXrun
	if err := d.Drain(context.Background(), time.Second); ReasonOf(err) != ReasonXrun {
		t.Errorf("Drain after an underrun: %v, want %s", err, ReasonXrun)
	}
	if n := k.callsOf(ioctlDrain); n != 0 {
		t.Errorf("DRAIN issued %d times on a stream that had stopped", n)
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestDrainSeesAnUnderrunBetweenItsCheckAndDRAIN pins the window the state
// check before DRAIN leaves: the check reads RUNNING, the stream underruns
// before DRAIN, and DRAIN moves the XRUN stream to SETUP. A running stream
// with frames left is seen DRAINING first when its drain is clean, so SETUP
// straight after DRAIN, from a stream that was RUNNING, is the underrun
// (Codex and the Claude API review of the #1301 fixes).
func TestDrainSeesAnUnderrunBetweenItsCheckAndDRAIN(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.states = []int32{stateRunning, stateSetup}
	if err := d.Drain(context.Background(), time.Second); ReasonOf(err) != ReasonXrun {
		t.Errorf("Drain: %v, want %s", err, ReasonXrun)
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestCloseClosesOnEveryPath pins that the node is closed exactly once
// whichever way its use ends: each refusal after the open, and each way a
// stream ends before Close. The hardware parameters are freed before the
// close whenever HW_PARAMS had set them, and never otherwise.
func TestCloseClosesOnEveryPath(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    func(*fakeKernel)
		hwFree int
	}{
		{"PVERSION fails", func(k *fakeKernel) { k.fail[ioctlPVersion] = errnoENOTTY }, 0},
		{"old protocol", func(k *fakeKernel) { k.version = protocolVersion(2, 0, 9) }, 0},
		{"INFO fails", func(k *fakeKernel) { k.fail[ioctlInfo] = errnoENODEV }, 0},
		{"another endpoint", func(k *fakeKernel) { k.info.card = 9 }, 0},
		{"refine refuses", func(k *fakeKernel) { k.hw.rates = []uint32{44100} }, 0},
		{"refine fails", func(k *fakeKernel) { k.fail[ioctlHWRefine] = errnoENODEV }, 0},
		{"HW_PARAMS fails", func(k *fakeKernel) { k.fail[ioctlHWParams] = errno(5) }, 0},
		{"read-back mismatch", func(k *fakeKernel) { k.commit = func(p *hwParams) { p.setInteger(hwParamRate, 44100) } }, 1},
		{"SW_PARAMS fails", func(k *fakeKernel) { k.fail[ioctlSWParams] = errnoEINVAL }, 1},
		{"PREPARE fails", func(k *fakeKernel) { k.fail[ioctlPrepare] = errno(5) }, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newFake(t)
			c.set(k)
			if d, err := openFake(k, cfg48k); err == nil {
				_ = d.Close()
				t.Fatalf("opened")
			}
			if got := k.callsOf(ioctlHWFree); got != c.hwFree {
				t.Errorf("HW_FREE issued %d times, want %d", got, c.hwFree)
			}
			k.checkClosedOnce(t)
		})
	}

	for _, c := range []struct {
		name string
		end  func(context.Context, *fakeKernel, *Device)
	}{
		{"after an xrun", func(ctx context.Context, k *fakeKernel, d *Device) {
			k.writes = []fakeWrite{{err: errnoEPIPE}}
			_, _ = d.Write(ctx, frames48k(8))
		}},
		{"after a disconnect", func(ctx context.Context, k *fakeKernel, d *Device) {
			k.writes = []fakeWrite{{err: errnoENODEV}}
			k.fail[ioctlDrop], k.fail[ioctlHWFree] = errnoENODEV, errnoENODEV
			_, _ = d.Write(ctx, frames48k(8))
		}},
		{"after a cancel", func(ctx context.Context, k *fakeKernel, d *Device) {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			_, _ = d.Write(ctx, frames48k(8))
		}},
		{"after a drain timeout", func(ctx context.Context, k *fakeKernel, d *Device) {
			k.states = []int32{stateDraining, stateDraining}
			k.polls = []fakePoll{{timeout: true}, {timeout: true}}
			_ = d.Drain(ctx, d.Params().PeriodTime())
		}},
		{"after a drop", func(ctx context.Context, k *fakeKernel, d *Device) { _ = d.Drop() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newFake(t)
			d := mustOpen(t, k, cfg48k)
			c.end(context.Background(), k, d)
			if err := d.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if k.callsOf(ioctlHWFree) != 1 {
				t.Errorf("HW_FREE issued %d times, want once", k.callsOf(ioctlHWFree))
			}
			k.checkClosedOnce(t)
		})
	}
}

// TestCloseIsOnceAndThenNothingReachesTheKernel pins that a second Close
// does nothing, and that every operation on a closed Device is refused
// without a call to the kernel.
func TestCloseIsOnceAndThenNothingReachesTheKernel(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	calls := len(k.calls)
	if err := d.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	ctx := context.Background()
	for name, op := range map[string]func() error{
		"Write":  func() error { _, err := d.Write(ctx, frames48k(1)); return err },
		"Start":  d.Start,
		"Status": func() error { _, err := d.Status(); return err },
		"Delay":  func() error { _, err := d.Delay(); return err },
		"Drop":   d.Drop,
		"Drain":  func() error { return d.Drain(ctx, time.Second) },
	} {
		if err := op(); ReasonOf(err) != ReasonClosed {
			t.Errorf("%s after Close: %v, want %s", name, err, ReasonClosed)
		}
	}
	if len(k.calls) != calls || len(k.pollWaits) != 0 {
		t.Errorf("calls after Close: %#x", k.calls[calls:])
	}
	k.checkClosedOnce(t)
}

// TestStatusAndDelayReadTheKernel pins Status and Delay: the state and the
// counters as STATUS gives them, and DELAY's frames.
func TestStatusAndDelayReadTheKernel(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.delay = 4321
	k.states = []int32{stateRunning}
	st, err := d.Status()
	if err != nil || st.State != State(stateRunning) || st.Delay != 4321 {
		t.Errorf("Status = %+v, %v", st, err)
	}
	if st.State.String() != "RUNNING" {
		t.Errorf("state %q", st.State)
	}
	if n, err := d.Delay(); err != nil || n != 4321 {
		t.Errorf("Delay = %d, %v", n, err)
	}
	if err := d.Start(); ReasonOf(err) != ReasonBadState {
		t.Errorf("Start on a running stream: %v, want %s", err, ReasonBadState)
	}
	_ = d.Close()
}
