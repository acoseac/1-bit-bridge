package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// The DSD check's admission at the device slot (B728 B12 PR 3), the step
// under the backend's lock that follows its reading of the policy: its
// refusals by word, in order, each leaving the record, the waiters and the
// count as they were; its waits for a probe and for a tone, counted among the
// waiters and bounded; and the words a probe, a tone and a choice's write
// meet while it holds the slot. Its run through the real sinks is
// internal/alsapcm's (playback_dsd_check_test.go), where the fake kernel is.

// checkTimer is one timer the backend armed, for a test to fire.
type checkTimer struct {
	d       time.Duration
	f       func()
	stopped chan struct{}
	once    sync.Once
}

// checkTimers is the backend's AfterFunc for these tests: every timer armed
// is sent on armed, and none fires but by the test; with hold set, the
// arming waits for hold to close, so a test makes the cases of the wait that
// follows it ready first.
type checkTimers struct {
	armed chan *checkTimer
	hold  chan struct{}
}

func newCheckTimers() *checkTimers { return &checkTimers{armed: make(chan *checkTimer, 16)} }

func (c *checkTimers) afterFunc(d time.Duration, f func()) func() bool {
	t := &checkTimer{d: d, f: f, stopped: make(chan struct{})}
	c.armed <- t
	if c.hold != nil {
		<-c.hold
	}
	return func() bool { t.once.Do(func() { close(t.stopped) }); return true }
}

// next is the next timer armed, under the watchdog.
func (c *checkTimers) next(t *testing.T, what string) *checkTimer {
	t.Helper()
	select {
	case tm := <-c.armed:
		return tm
	case <-time.After(10 * time.Second):
		t.Fatalf("no timer was armed for %s", what)
		return nil
	}
}

// checkBackend is a backend with an effective DSD policy that reads nothing
// (no box, no state: no DAC published), its set computed once, so a check
// passes its reading and meets the slot, where it is refused dac_changed at
// the end; timers is its AfterFunc.
func checkBackend(t *testing.T, timers *checkTimers) *Backend {
	t.Helper()
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, DSD: &DSDPolicy{}, AfterFunc: timers.afterFunc})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b
}

// checkRequest is a check well formed for the backend's DAC as no read side
// names it: a key and a token of the token's form.
func checkRequest() CheckRequest {
	return CheckRequest{DACKey: "d1-test", DACToken: "t1-0123456789abcdef0123456789abcdef", Transport: dsdstate.TransportNative,
		BitRate: dsdstate.DSD64, Stop: make(chan struct{})}
}

// refusedWith reports the word err refuses a check with, "" for none.
func refusedWith(err error) string {
	var r *CheckRefused
	if errors.As(err, &r) {
		return r.Word
	}
	return ""
}

// slotAsIs fails the test unless b's slot, record and waiters are as a
// refusal leaves them: the record rec, no check in the slot, no waiter, and
// no goroutine counted.
func slotAsIs(t *testing.T, b *Backend, rec *checkHold, what string) {
	t.Helper()
	b.mu.Lock()
	checking, got, waiters := b.checking, b.checkRec, b.waiters
	b.mu.Unlock()
	if checking != nil || got != rec || waiters != 0 {
		t.Errorf("%s: the check in the slot %v, the record %p (want %p), %d waiters", what, checking != nil, got, rec, waiters)
	}
	if n, w := b.Running(); n != 0 || w != 0 {
		t.Errorf("%s: %d goroutines and %d writes counted", what, n, w)
	}
}

// TestTheChecksGateReadsTheDeviceSlot: a check is refused, by word and before
// anything is counted, for a request that is none (bad_request), on a backend
// with no DSD policy (dsd_not_in_this_release) and once CloseChecks has run
// (serve_stopping); then, counted, past its reading of the policy, in one
// step under the backend's lock: the supervisor's gate shut, by its word (as
// it starts, for each shut, and closed by serve's stop), a release never
// confirmed (release_unconfirmed), a choice's lease, a session's record, the
// device's holder or another check (device_busy), and a slot free whose read
// side names no DAC (dac_changed). No refusal makes, replaces or changes the
// record, leaves a waiter or keeps the count.
func TestTheChecksGateReadsTheDeviceSlot(t *testing.T) {
	bad := func(edit func(*CheckRequest)) CheckRequest {
		r := checkRequest()
		edit(&r)
		return r
	}
	open := func(b *Backend) { b.ToneOutput(true, "") }
	for _, c := range []struct {
		name string
		set  func(b *Backend)
		req  CheckRequest
		want string
	}{
		{"a transport that is not DSD's", open, bad(func(r *CheckRequest) { r.Transport = "pcm" }), CheckBadRequest},
		{"a rate the check does not play", open, bad(func(r *CheckRequest) { r.BitRate = 2822401 }), CheckBadRequest},
		{"a token of no token's form", open, bad(func(r *CheckRequest) { r.DACToken = "t1-XYZ" }), CheckBadRequest},
		{"no stop signal", open, bad(func(r *CheckRequest) { r.Stop = nil }), CheckBadRequest},
		{"serve's stop", func(b *Backend) { open(b); b.CloseChecks() }, checkRequest(), string(CheckServeStopping)},
		{"the gate as it starts", func(*Backend) {}, checkRequest(), string(ToneOutputUnknown)},
		{"the gate shut withheld", func(b *Backend) { b.ToneOutput(false, ToneWithheld) }, checkRequest(), string(ToneWithheld)},
		{"the gate shut for an output gone", func(b *Backend) { b.ToneOutput(false, ToneOutputUnavailable) }, checkRequest(), string(ToneOutputUnavailable)},
		{"the gate shut for a unit that changed", func(b *Backend) { b.ToneOutput(false, ToneOutputChanged) }, checkRequest(), string(ToneOutputChanged)},
		{"the gate shut for a report the engine did not take", func(b *Backend) { open(b); b.ToneOutput(false, ToneOutputUnknown) }, checkRequest(), string(ToneOutputUnknown)},
		{"the gate shut for a stale snapshot", func(b *Backend) { b.ToneOutput(false, ToneSnapshotStale) }, checkRequest(), string(ToneSnapshotStale)},
		{"the gate closed by the tones' stop", func(b *Backend) { open(b); b.CloseTones() }, checkRequest(), string(ToneServeStopping)},
		{"a release never confirmed", func(b *Backend) { open(b); b.terminalUnconfirmed = true }, checkRequest(), string(CheckReleaseUnconfirmed)},
		{"a choice's lease", func(b *Backend) { open(b); b.lease = &configLease{free: make(chan struct{})} }, checkRequest(), string(CheckDeviceBusy)},
		{"a session's record", func(b *Backend) { open(b); b.sessions[1] = newSession(b, 1) }, checkRequest(), string(CheckDeviceBusy)},
		{"the device's holder", func(b *Backend) { open(b); b.holder = newSession(b, 1) }, checkRequest(), string(CheckDeviceBusy)},
		{"another check", func(b *Backend) {
			open(b)
			ctx, cancel := context.WithCancelCause(context.Background())
			b.checking = &checkHold{ctx: ctx, cancel: cancel, done: make(chan struct{})}
		}, checkRequest(), string(CheckDeviceBusy)},
		{"a free slot whose read side names no DAC", open, checkRequest(), CheckDACChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := checkBackend(t, newCheckTimers())
			c.set(b)
			// The last check's record, which a refusal leaves as it is.
			rec := &checkHold{id: "the last check's"}
			b.mu.Lock()
			b.checkRec = rec
			checking := b.checking
			b.mu.Unlock()
			st, err := b.Check(context.Background(), c.req)
			if w := refusedWith(err); w != c.want || st != (CheckStatus{}) {
				t.Errorf("the check answered %+v, %v; want refused %s", st, err, c.want)
			}
			b.mu.Lock()
			if b.checking != checking {
				t.Error("a refused check took the slot")
			}
			b.checking = nil
			b.mu.Unlock()
			slotAsIs(t, b, rec, c.name)
		})
	}
	t.Run("a backend with no DSD policy", func(t *testing.T) {
		b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}})
		if err != nil {
			t.Fatal(err)
		}
		b.ToneOutput(true, "")
		if _, err := b.Check(context.Background(), checkRequest()); refusedWith(err) != string(ReasonDSD) {
			t.Errorf("the check answered %v; want refused %s", err, ReasonDSD)
		}
		slotAsIs(t, b, nil, "no DSD policy")
	})
}

// TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters: a check that finds the
// silent probe in the slot waits for it, counted among the waiters and among
// the backend's goroutines from before its first wait, at most ProbeWait,
// then refused device_busy; one that finds the console's tone there ends it
// preempted and waits at most ToneYield; once the slot is free it is admitted
// in the same step it stops waiting, here refused dac_changed with no DAC
// published; its request's end ends the wait with its error. Every way out
// stops its timer and leaves no waiter and no count.
func TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters(t *testing.T) {
	type answer struct {
		st  CheckStatus
		err error
	}
	asked := func(b *Backend, ctx context.Context) <-chan answer {
		ch := make(chan answer, 1)
		go func() {
			st, err := b.Check(ctx, checkRequest())
			ch <- answer{st, err}
		}()
		return ch
	}
	got := func(t *testing.T, ch <-chan answer) answer {
		t.Helper()
		select {
		case a := <-ch:
			return a
		case <-time.After(10 * time.Second):
			t.Fatal("the check never answered")
			return answer{}
		}
	}
	waiting := func(t *testing.T, b *Backend, tm *checkTimer, want time.Duration) {
		t.Helper()
		if tm.d != want {
			t.Errorf("the check's wait was armed for %v; want %v", tm.d, want)
		}
		b.mu.Lock()
		waiters, checking := b.waiters, b.checking
		b.mu.Unlock()
		if waiters != 1 || checking != nil {
			t.Errorf("a check waiting for the slot: %d waiters, the slot taken %v; want one waiter and the slot not taken", waiters, checking != nil)
		}
		if n, _ := b.Running(); n != 1 {
			t.Errorf("a check waiting for the slot: %d goroutines counted; want the check's", n)
		}
	}
	stopped := func(t *testing.T, tm *checkTimer) {
		t.Helper()
		select {
		case <-tm.stopped:
		default:
			t.Error("the check's wait left its timer armed")
		}
	}
	probing := func(b *Backend) *probeHold {
		p := &probeHold{done: make(chan struct{})}
		b.mu.Lock()
		b.probing = p
		b.mu.Unlock()
		return p
	}
	t.Run("a probe that ends", func(t *testing.T) {
		timers := newCheckTimers()
		b := checkBackend(t, timers)
		b.ToneOutput(true, "")
		p := probing(b)
		ch := asked(b, context.Background())
		tm := timers.next(t, "the check's wait for the probe")
		waiting(t, b, tm, ProbeWait)
		b.endProbe(p)
		if a := got(t, ch); refusedWith(a.err) != CheckDACChanged {
			t.Errorf("the check once the probe ended answered %+v, %v; want it admitted at the slot, dac_changed with no DAC", a.st, a.err)
		}
		stopped(t, tm)
		slotAsIs(t, b, nil, "once the probe ended")
	})
	t.Run("a probe past ProbeWait", func(t *testing.T) {
		timers := newCheckTimers()
		b := checkBackend(t, timers)
		b.ToneOutput(true, "")
		probing(b)
		ch := asked(b, context.Background())
		tm := timers.next(t, "the check's wait for the probe")
		waiting(t, b, tm, ProbeWait)
		tm.f()
		if a := got(t, ch); refusedWith(a.err) != string(CheckDeviceBusy) {
			t.Errorf("the check past ProbeWait answered %v; want device_busy", a.err)
		}
		b.mu.Lock()
		b.probing = nil
		b.mu.Unlock()
		slotAsIs(t, b, nil, "past ProbeWait")
	})
	t.Run("a tone, ended and past ToneYield", func(t *testing.T) {
		timers := newCheckTimers()
		b := checkBackend(t, timers)
		b.ToneOutput(true, "")
		tn, why := b.reserveTone(context.Background(), toneRequest)
		if tn == nil {
			t.Fatalf("the tone was refused: %s", why)
		}
		ch := asked(b, context.Background())
		tm := timers.next(t, "the check's wait for the tone")
		waiting(t, b, tm, ToneYield)
		if w := tn.ended(); w != TonePreempted {
			t.Errorf("the tone the check came beside ended %s; want preempted", w)
		}
		tm.f()
		if a := got(t, ch); refusedWith(a.err) != string(CheckDeviceBusy) {
			t.Errorf("the check past ToneYield answered %v; want device_busy", a.err)
		}
		b.mu.Lock()
		b.toning = nil
		b.mu.Unlock()
		slotAsIs(t, b, nil, "past ToneYield")
	})
	t.Run("its request's end", func(t *testing.T) {
		timers := newCheckTimers()
		b := checkBackend(t, timers)
		b.ToneOutput(true, "")
		probing(b)
		ctx, cancel := context.WithCancel(context.Background())
		ch := asked(b, ctx)
		tm := timers.next(t, "the check's wait for the probe")
		waiting(t, b, tm, ProbeWait)
		cancel()
		if a := got(t, ch); !errors.Is(a.err, context.Canceled) {
			t.Errorf("the check whose request ended answered %v; want its context's error", a.err)
		}
		stopped(t, tm)
		b.mu.Lock()
		b.probing = nil
		b.mu.Unlock()
		slotAsIs(t, b, nil, "its request's end")
	})
}

// TestTheTonesAndTheProbesNewWordsAreWorded: the tone's new word,
// check_running, is one of ToneWords, right after tone_playing, and the
// console's PLAYER_TONE_WORDS gives it a sentence (cmd/bridge's
// TestEveryToneWordIsWorded holds every word to one, both ways); the probe's,
// check_holds, is one of the slot's refusals (probeSlotSkips), so a probe it
// skips stays owed.
func TestTheTonesAndTheProbesNewWordsAreWorded(t *testing.T) {
	if ToneCheckRunning != "check_running" || ProbeCheckHolds != "check_holds" {
		t.Errorf("the words are %q and %q", ToneCheckRunning, ProbeCheckHolds)
	}
	words := ToneWords()
	if i := slices.Index(words, ToneCheckRunning); i < 1 || words[i-1] != TonePlaying {
		t.Errorf("ToneWords holds check_running at %d (%q); want it right after tone_playing", i, words)
	}
	if !slices.Contains(probeSlotSkips, ProbeCheckHolds) {
		t.Errorf("probeSlotSkips %q leaves check_holds out: a probe the check skips would not be owed", probeSlotSkips)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "admin", "static", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(src, "const PLAYER_TONE_WORDS = {")
	end := -1
	if start >= 0 {
		end = strings.Index(src[start:], "\n};")
	}
	if end < 0 {
		t.Fatal("app.js has no PLAYER_TONE_WORDS")
	}
	if m := regexp.MustCompile(`(?m)^  check_running: "([^"]+)",$`).FindStringSubmatch(src[start : start+end]); m == nil || strings.TrimSpace(m[1]) == "" {
		t.Error("the console's PLAYER_TONE_WORDS gives check_running no sentence")
	}
}

// TestWhatTheSlotRefusesWhileTheCheckHoldsIt: while the DSD check holds the
// device slot, a probe is skipped check_holds, so it stays owed; a tone is
// refused check_running; a choice's write is refused playback_not_stopped;
// and a Start waits for the check as for a probe, at most ProbeWait, never
// ending it.
func TestWhatTheSlotRefusesWhileTheCheckHoldsIt(t *testing.T) {
	b := checkBackend(t, newCheckTimers())
	b.ToneOutput(true, "")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := &checkHold{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	b.mu.Lock()
	b.checking = c
	b.mu.Unlock()
	if p, why := b.reserveProbe(); p != nil || why != ProbeCheckHolds || !slices.Contains(probeSlotSkips, why) {
		t.Errorf("a probe beside the check: taken %v, %q; want check_holds, one of the slot's refusals", p != nil, why)
	}
	if tn, why := b.reserveTone(context.Background(), toneRequest); tn != nil || why != ToneCheckRunning {
		t.Errorf("a tone beside the check: taken %v, %q; want check_running", tn != nil, why)
	}
	if l, out, err := b.takeLease(context.Background(), &choiceJournal{}); l != nil || out != ChoicePlaybackNotStopped || err != nil {
		t.Errorf("a choice's lease beside the check: %v, %q, %v; want playback_not_stopped", l != nil, out, err)
	}
	b.mu.Lock()
	held, bound, by := b.slotHeldLocked()
	b.mu.Unlock()
	if held != c.done || bound != ProbeWait || by != "the DSD check" || ctx.Err() != nil {
		t.Errorf("a Start beside the check waits for %v at most %v (%q), the check ended %v; want its slot at most ProbeWait, never ended", held == c.done, bound, by, ctx.Err() != nil)
	}
}

// TestTheChecksPhasesAndReasonsAreTheirLists pins the words PR 4's answer
// publishes, in order: the phases, and the reasons, stopped's then failed's.
func TestTheChecksPhasesAndReasonsAreTheirLists(t *testing.T) {
	phases := []CheckPhase{"preparing", "playing", "complete", "stopping", "stopped", "failed"}
	if got := CheckPhases(); !slices.Equal(got, phases) {
		t.Errorf("CheckPhases() = %q; want %q", got, phases)
	}
	reasons := []CheckReason{"stop", "deleted", "serve_stopping", "timeout", "policy_changed", "output_gone", "output_unavailable",
		"output_changed", "device_error", "device_busy", "wrong_device", "release_unconfirmed", "backend_failed"}
	if got := CheckReasons(); !slices.Equal(got, reasons) {
		t.Errorf("CheckReasons() = %q; want %q", got, reasons)
	}
}

// TestABackendBuiltStartsNothingOfTheCheck: building a backend arms no
// timer, counts no goroutine and makes no check's record; the check's only
// timers, goroutines and record come from Check.
func TestABackendBuiltStartsNothingOfTheCheck(t *testing.T) {
	var armed int
	var mu sync.Mutex
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, DSD: &DSDPolicy{}, AfterFunc: func(d time.Duration, f func()) func() bool {
		mu.Lock()
		armed++
		mu.Unlock()
		return func() bool { return true }
	}})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := armed
	mu.Unlock()
	if n != 0 {
		t.Errorf("building a backend armed %d timers", n)
	}
	if g, w := b.Running(); g != 0 || w != 0 {
		t.Errorf("building a backend counted %d goroutines and %d writes", g, w)
	}
	if st, ok := b.CheckStatus(); ok {
		t.Errorf("a backend built has a check's record: %+v", st)
	}
}

// publishedBackend is a backend whose effective DSD set plays the fixture
// DAC natively, read through box (fixtureBox, or one over it), its gate open,
// after its AfterFunc, with no unit to locate: a check it admits ends
// output_gone once its chime is built, unless something ended it first.
func publishedBackend(t *testing.T, after func(time.Duration, func()) func() bool, box DSDBox) *Backend {
	t.Helper()
	pol := &DSDPolicy{Store: &fixtureStore{st: nativeFixture(t)}, Kernel: fixtureKernel, Box: box, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return fixtureT0 }}
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, DSD: pol, AfterFunc: after})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	b.ToneOutput(true, "")
	return b
}

// publishedRequest is a native DSD64 check of the DAC b publishes, with its
// stop signal, which the test closes.
func publishedRequest(t *testing.T, b *Backend) (CheckRequest, chan struct{}) {
	t.Helper()
	p := b.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		t.Fatalf("the backend publishes no DAC: %+v", p.DSD)
	}
	stop := make(chan struct{})
	return CheckRequest{DACKey: p.DSD.DAC.Key, DACToken: p.DSD.DAC.Token, Transport: dsdstate.TransportNative, BitRate: dsdstate.DSD64,
		Stop: stop}, stop
}

// heldBox is fixtureBox whose next reading of the unit, once armed
// (holdNext), is held until the test lets it go.
type heldBox struct {
	fixtureBox
	mu       sync.Mutex
	in, gate chan struct{}
}

// holdNext arms the hold: the channel closes as the next reading reaches it,
// and the function lets that reading go, once.
func (h *heldBox) holdNext() (<-chan struct{}, func()) {
	in, gate := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.in, h.gate = in, gate
	h.mu.Unlock()
	var once sync.Once
	return in, func() { once.Do(func() { close(gate) }) }
}

func (h *heldBox) UnitModel(card int) (alsa.UnitModel, error) {
	h.mu.Lock()
	in, gate := h.in, h.gate
	h.in, h.gate = nil, nil
	h.mu.Unlock()
	if in != nil {
		close(in)
		<-gate
	}
	return h.fixtureBox.UnitModel(card)
}

// checkAnswer is a check's answer.
type checkAnswer struct {
	st  CheckStatus
	err error
}

// askCheck asks b for req on a goroutine of its own.
func askCheck(b *Backend, ctx context.Context, req CheckRequest) <-chan checkAnswer {
	ch := make(chan checkAnswer, 1)
	go func() {
		st, err := b.Check(ctx, req)
		ch <- checkAnswer{st, err}
	}()
	return ch
}

// answered is a check's answer, under the watchdog.
func answered(t *testing.T, ch <-chan checkAnswer) checkAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("the check never answered")
		return checkAnswer{}
	}
}

// reached waits for ch to close, under the watchdog.
func reached(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("never reached: %s", what)
	}
}

// TestAnEndedRequestAdmitsNothing: a check whose request has ended is refused
// with the request's error, leaving the slot, the waiters, the count and the
// record as they were, wherever its request ends: before Check, the slot
// free (where the read side names no DAC it was refused dac_changed) or the
// console's tone in it, which it leaves playing, never preempted; during its
// reading of the policy, the tone in the slot, asked under the backend's lock
// before the slot is looked at; and as its wait for the slot ends, the probe
// it waited for ended or its bound passed with the request's end ready beside
// it: the wait's arming is held until both are ready, and each such case runs
// in rounds, its select picking at random among the cases ready.
func TestAnEndedRequestAdmitsNothing(t *testing.T) {
	ended := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	refusedEnded := func(t *testing.T, a checkAnswer, what string) {
		t.Helper()
		if !errors.Is(a.err, context.Canceled) || a.st != (CheckStatus{}) {
			t.Errorf("%s: the check answered %+v, %v; want its request's error", what, a.st, a.err)
		}
	}
	toning := func(t *testing.T, b *Backend) *toneHold {
		t.Helper()
		tn, why := b.reserveTone(context.Background(), toneRequest)
		if tn == nil {
			t.Fatalf("the tone was refused: %s", why)
		}
		return tn
	}
	toneKept := func(t *testing.T, b *Backend, tn *toneHold, what string) {
		t.Helper()
		if tn.ctx.Err() != nil {
			t.Errorf("%s: the console's tone ended %s; want it playing", what, tn.ended())
		}
		b.mu.Lock()
		holder := b.toning
		b.toning = nil
		b.mu.Unlock()
		if holder != tn {
			t.Errorf("%s: the console's tone no longer holds the slot", what)
		}
		slotAsIs(t, b, nil, what)
	}
	t.Run("before Check, the slot free", func(t *testing.T) {
		b := checkBackend(t, newCheckTimers())
		b.ToneOutput(true, "")
		rec := &checkHold{id: "the last check's"}
		b.mu.Lock()
		b.checkRec = rec
		b.mu.Unlock()
		refusedEnded(t, answered(t, askCheck(b, ended(), checkRequest())), "a request ended before Check")
		slotAsIs(t, b, rec, "a request ended before Check")
	})
	t.Run("before Check, the console's tone in the slot", func(t *testing.T) {
		b := checkBackend(t, newCheckTimers())
		b.ToneOutput(true, "")
		tn := toning(t, b)
		refusedEnded(t, answered(t, askCheck(b, ended(), checkRequest())), "a request ended before Check beside the tone")
		toneKept(t, b, tn, "a request ended before Check beside the tone")
	})
	t.Run("during its reading of the policy, the console's tone in the slot", func(t *testing.T) {
		box := &heldBox{}
		b := publishedBackend(t, newCheckTimers().afterFunc, box)
		req, _ := publishedRequest(t, b)
		tn := toning(t, b)
		in, letGo := box.holdNext()
		t.Cleanup(letGo)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := askCheck(b, ctx, req)
		reached(t, in, "the check's reading of the policy held in the box")
		cancel()
		letGo()
		refusedEnded(t, answered(t, ch), "a request ended during its reading")
		toneKept(t, b, tn, "a request ended during its reading")
	})
	for _, c := range []struct {
		name  string
		ready func(b *Backend, p *probeHold, tm *checkTimer)
	}{
		{"as the probe it waits for ends", func(b *Backend, p *probeHold, _ *checkTimer) { b.endProbe(p) }},
		{"as its wait's bound passes", func(_ *Backend, _ *probeHold, tm *checkTimer) { tm.f() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			for round := range 32 {
				timers := newCheckTimers()
				timers.hold = make(chan struct{})
				b := checkBackend(t, timers)
				b.ToneOutput(true, "")
				p := &probeHold{done: make(chan struct{})}
				b.mu.Lock()
				b.probing = p
				b.mu.Unlock()
				ctx, cancel := context.WithCancel(context.Background())
				ch := askCheck(b, ctx, checkRequest())
				tm := timers.next(t, "the check's wait for the probe")
				c.ready(b, p, tm)
				cancel()
				close(timers.hold)
				if a := answered(t, ch); !errors.Is(a.err, context.Canceled) || a.st != (CheckStatus{}) {
					t.Fatalf("round %d: a check whose request ended %s answered %+v, %v; want its request's error", round, c.name, a.st, a.err)
				}
				b.mu.Lock()
				b.probing = nil
				b.mu.Unlock()
				slotAsIs(t, b, nil, c.name)
			}
		})
	}
}

// TestTheCheckAnswersItsOwnRecord: Check answers the record of the check it
// admitted, built in its admission's step under the backend's lock, never the
// slot's last record read after it: here a check admitted once the probe it
// waited for ended, whose wait's stop, run on its caller's goroutine as its
// admission returns, puts another record in the last record's place.
func TestTheCheckAnswersItsOwnRecord(t *testing.T) {
	var b *Backend
	armed := make(chan struct{}, 1)
	admitted := ""
	after := func(d time.Duration, f func()) func() bool {
		if d != ProbeWait {
			return func() bool { return true }
		}
		armed <- struct{}{}
		return func() bool {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c := b.checking; c != nil && admitted == "" {
				admitted = c.id
				ctx, cancel := context.WithCancelCause(context.Background())
				b.checkRec = &checkHold{id: "another check's", ctx: ctx, cancel: cancel, done: make(chan struct{})}
			}
			return true
		}
	}
	b = publishedBackend(t, after, fixtureBox{})
	req, stop := publishedRequest(t, b)
	p := &probeHold{done: make(chan struct{})}
	b.mu.Lock()
	b.probing = p
	b.mu.Unlock()
	ch := askCheck(b, context.Background(), req)
	select {
	case <-armed:
	case <-time.After(10 * time.Second):
		t.Fatal("the check never waited for the probe")
	}
	b.endProbe(p)
	a := answered(t, ch)
	if a.err != nil {
		t.Fatalf("the check was refused: %v", a.err)
	}
	b.mu.Lock()
	own := admitted
	b.mu.Unlock()
	if own == "" || a.st.ID != own || a.st.Phase != CheckPreparing || a.st.Transport != req.Transport || a.st.BitRate != req.BitRate || a.st.Token != req.DACToken {
		t.Errorf("Check answered the record %+v; want the check it admitted, %q, preparing", a.st, own)
	}
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestTheChecksCountIsGivenBackOnEveryWayOut: a check counted among the
// backend's goroutines from before its reading of the policy gives the count
// back on every way out before its hand-over to its run: a panic on its way
// (here its wait's AfterFunc, a probe in the slot), which leaves no waiter
// and no check in the slot, so Wait answers; and a refusal, then an
// admission, its run ended, each leaving nothing counted.
func TestTheChecksCountIsGivenBackOnEveryWayOut(t *testing.T) {
	t.Run("a panic in its wait for a probe", func(t *testing.T) {
		b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, DSD: &DSDPolicy{},
			AfterFunc: func(time.Duration, func()) func() bool { panic("the wait's timer") }})
		if err != nil {
			t.Fatal(err)
		}
		b.RefreshDSD(true)
		b.ToneOutput(true, "")
		b.mu.Lock()
		b.probing = &probeHold{done: make(chan struct{})}
		b.mu.Unlock()
		raised := func() (r any) {
			defer func() { r = recover() }()
			_, _ = b.Check(context.Background(), checkRequest())
			return nil
		}()
		if raised == nil {
			t.Fatal("the AfterFunc's panic was not raised to Check's caller")
		}
		b.mu.Lock()
		waiters, checking := b.waiters, b.checking
		b.probing = nil
		b.mu.Unlock()
		if waiters != 0 || checking != nil {
			t.Errorf("a check whose wait panicked left %d waiters, the slot taken %v", waiters, checking != nil)
		}
		if n, w := b.Running(); n != 0 || w != 0 {
			t.Fatalf("a check whose wait panicked left %d goroutines and %d writes counted", n, w)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.Wait(ctx); err != nil {
			t.Error(err)
		}
	})
	t.Run("a refusal, then an admission", func(t *testing.T) {
		b := publishedBackend(t, newCheckTimers().afterFunc, fixtureBox{})
		other, _ := publishedRequest(t, b)
		other.DACKey = "d1-another"
		if _, err := b.Check(context.Background(), other); refusedWith(err) != CheckDACChanged {
			t.Fatalf("a check of another DAC answered %v; want dac_changed", err)
		}
		if n, w := b.Running(); n != 0 || w != 0 {
			t.Errorf("a refused check left %d goroutines and %d writes counted", n, w)
		}
		req, stop := publishedRequest(t, b)
		if _, err := b.Check(context.Background(), req); err != nil {
			t.Fatalf("the check was refused: %v", err)
		}
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := b.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if n, w := b.Running(); n != 0 || w != 0 {
			t.Errorf("an admitted check's run ended left %d goroutines and %d writes counted", n, w)
		}
	})
}
