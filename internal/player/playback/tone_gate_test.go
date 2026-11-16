package playback

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// toneRequest is a tone the console may ask for.
var toneRequest = ToneRequest{LevelDBFS: -40, Seconds: 10}

// TestTheTonesGateReadsTheDeviceSlot pins the gate a tone passes before it
// opens anything (B601), one step under the backend's lock, the lock a
// Start's hold and a probe take the slot under: the supervisor's gate shut,
// by its word; a release never confirmed; a probe under way; a choice's write
// holding the configuration lease; a Start waiting for the slot; a session's
// record, and the device's holder; another tone; and a request already ended
// each refuse it by its word, leaving the slot as it was. A free slot is
// taken, its record holding the caller's context, and the tone's end gives
// it back and says so.
func TestTheTonesGateReadsTheDeviceSlot(t *testing.T) {
	ended, end := context.WithCancel(context.Background())
	end()
	for _, c := range []struct {
		name string
		set  func(b *Backend)
		ctx  context.Context
		want ToneOutcome
	}{
		{"the gate as it starts, before any report", func(b *Backend) {}, nil, ToneOutputUnknown},
		{"the gate shut for a withheld output", func(b *Backend) { b.ToneOutput(false, ToneWithheld) }, nil, ToneWithheld},
		{"the gate shut for an output gone", func(b *Backend) { b.ToneOutput(false, ToneOutputUnavailable) }, nil, ToneOutputUnavailable},
		{"the gate shut for a unit that changed", func(b *Backend) { b.ToneOutput(false, ToneOutputChanged) }, nil, ToneOutputChanged},
		{"the gate shut for a stale snapshot", func(b *Backend) { b.ToneOutput(false, ToneSnapshotStale) }, nil, ToneSnapshotStale},
		{"the gate closed by serve's stop", func(b *Backend) { b.ToneOutput(true, ""); b.CloseTones(); b.ToneOutput(true, "") }, nil, ToneServeStopping},
		{"a release never confirmed", func(b *Backend) { b.ToneOutput(true, ""); b.terminalUnconfirmed = true }, nil, ToneReleaseUnconfirmed},
		{"a probe under way", func(b *Backend) { b.ToneOutput(true, ""); b.probing = &probeHold{done: make(chan struct{})} }, nil, ToneProbeRunning},
		{"a choice's write holding the lease", func(b *Backend) { b.ToneOutput(true, ""); b.lease = &configLease{free: make(chan struct{})} }, nil, ToneChoiceWrite},
		{"a Start waiting for the slot", func(b *Backend) { b.ToneOutput(true, ""); b.waiters = 1 }, nil, ToneStartWaiting},
		{"a session's record", func(b *Backend) { b.ToneOutput(true, ""); b.sessions[1] = newSession(b, 1) }, nil, ToneSessionHolds},
		{"the device's holder", func(b *Backend) { b.ToneOutput(true, ""); b.holder = newSession(b, 1) }, nil, ToneSessionHolds},
		{"another tone", func(b *Backend) {
			b.ToneOutput(true, "")
			if t, why := b.reserveTone(context.Background(), toneRequest); t == nil {
				panic("the first tone was refused: " + string(why))
			}
		}, nil, TonePlaying},
		{"a request ended already", func(b *Backend) { b.ToneOutput(true, "") }, ended, ToneRequestEnded},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := gateBackend(t, nil)
			c.set(b)
			before := b.toning
			ctx := c.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			if tn, why := b.reserveTone(ctx, toneRequest); tn != nil || why != c.want || b.toning != before {
				t.Errorf("the gate: taken %v, %q; want %q and the slot left as it was", tn != nil, why, c.want)
			}
		})
	}
	b := gateBackend(t, nil)
	b.ToneOutput(true, "")
	b.toneLast = ToneResult{Outcome: TonePlayed, LastError: ToneLastErrorBusy}
	tn, why := b.reserveTone(context.Background(), toneRequest)
	if tn == nil || why != "" || b.toning != tn {
		t.Fatalf("a free slot: taken %v, %q; want it taken by the tone", tn != nil, why)
	}
	if b.toneLast != (ToneResult{}) {
		t.Errorf("the last tone's result %+v is kept past the next tone's admission", b.toneLast)
	}
	if st := b.ToneStatus(); st.State != ToneStatePlaying || st.Request != toneRequest {
		t.Errorf("the status of a tone in the slot: %+v", st)
	}
	tn.end(TonePlayed)
	b.endTone(tn, &toneRun{}, true)
	if b.toning != nil {
		t.Error("the tone's end left the slot taken")
	}
	select {
	case <-tn.done:
	default:
		t.Error("the tone's end does not say it ended")
	}
	if st := b.ToneStatus(); st.State != ToneIdle || st.Last.Outcome != TonePlayed || !st.Last.Admitted {
		t.Errorf("the status after the tone's end: %+v", st)
	}
	if b.terminalUnconfirmed {
		t.Error("a tone whose close was confirmed fenced the device")
	}
}

// TestAToneBesideAProbeOrAWriteIsRefusedAtOnce (B601): a tone asked while the
// silent probe holds the device slot, or while a choice's write holds the
// configuration lease, is refused at once by its word, waiting for neither,
// and its count is given back.
func TestAToneBesideAProbeOrAWriteIsRefusedAtOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(b *Backend)
		want ToneOutcome
	}{
		{"a probe holding the slot", func(b *Backend) { b.probing = &probeHold{done: make(chan struct{})} }, ToneProbeRunning},
		{"a choice's write holding the lease", func(b *Backend) { b.lease = &configLease{free: make(chan struct{})} }, ToneChoiceWrite},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := gateBackend(t, nil)
			b.ToneOutput(true, "")
			c.set(b)
			answer := make(chan ToneResult, 1)
			go func() { answer <- b.Tone(context.Background(), toneRequest) }()
			res := receiveWithin(t, answer, "the tone's refusal")
			if res.Outcome != c.want || res.Admitted {
				t.Errorf("the tone answered %+v; want refused %q", res, c.want)
			}
			if n := b.Stats().Goroutines; n != 0 {
				t.Errorf("%d goroutines counted once the tone was refused: its count was not given back", n)
			}
		})
	}
}

// TestAChoicesWriteBesideAToneIsRefused (B601): a choice's write asked while
// the console's tone holds the device slot is refused as playback_not_stopped
// at once, the configuration lease never taken.
func TestAChoicesWriteBesideAToneIsRefused(t *testing.T) {
	b := gateBackend(t, nil)
	b.ToneOutput(true, "")
	tn, why := b.reserveTone(context.Background(), toneRequest)
	if tn == nil {
		t.Fatalf("the tone could not take a free slot: %q", why)
	}
	answer := make(chan ChoiceOutcome, 1)
	go func() {
		answer <- b.ChooseDSD(context.Background(), ChoiceRequest{DACKey: "k", Choice: policy.Choice{Mode: dsdstate.ModeNative, By: dsdstate.ByApp}}).Outcome
	}()
	if got := receiveWithin(t, answer, "the write's answer"); got != ChoicePlaybackNotStopped {
		t.Errorf("a write beside the tone answered %s; want playback_not_stopped", got)
	}
	b.mu.Lock()
	lease := b.lease
	b.mu.Unlock()
	if lease != nil {
		t.Error("the write refused beside the tone holds the lease")
	}
}
