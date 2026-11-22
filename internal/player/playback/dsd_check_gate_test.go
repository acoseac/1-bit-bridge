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
// is sent on armed, and none fires but by the test.
type checkTimers struct{ armed chan *checkTimer }

func newCheckTimers() *checkTimers { return &checkTimers{armed: make(chan *checkTimer, 16)} }

func (c *checkTimers) afterFunc(d time.Duration, f func()) func() bool {
	t := &checkTimer{d: d, f: f, stopped: make(chan struct{})}
	c.armed <- t
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
