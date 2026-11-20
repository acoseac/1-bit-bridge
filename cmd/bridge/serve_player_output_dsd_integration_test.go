//go:build nativeintegration

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The DSD choice's route beside a real Start (B728 B9 PR 3b's follow-up):
// the route, serve's adapter, the real engine and the real backend, whose
// device is the ALSA fake the native-integration overlay lends this file
// (scripts/native-integration-overlay.py).

// fakeNodeLocator binds the fake kernel's node, with no check after open:
// the binding's own tests hold that check.
type fakeNodeLocator struct{}

func (fakeNodeLocator) Locate(context.Context) (playback.Unit, error) {
	return playback.Unit{Endpoint: alsapcm.FakeEndpoint(), Label: "the test's DAC"}, nil
}

// TestNativeIntegrationAStartCompletesWhileTheRefreshAWriteWaitedForIsParked
// (B728 B9 PR 3b's follow-up): a PUT whose route's time runs out while a
// refresh, parked in its reading of the DAC, holds the refreshes' lock answers
// 500 persist_failed, having let go of the engine's reservation and the
// backend's lease, and a Play's Start, real PCM through the fake device,
// completes while that refresh is still parked, the device opened once. Let
// go, the refresh ends, and the PUT that failed wrote nothing. A PUT while the
// Start's session plays is refused as the protocol table's step 8 says, 409
// playback_not_stopped; once a Stop is confirmed a PUT is written, 200, and
// nothing more was opened.
func TestNativeIntegrationAStartCompletesWhileTheRefreshAWriteWaitedForIsParked(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	box := &parkedChoiceBox{Box: playbacktest.NewBox(5), came: make(chan struct{}), gate: make(chan struct{})}
	store := playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative))
	p := playbacktest.Policy(5, box.Box, store)
	p.Box = box
	backend, err := playback.New(playback.Config{DSD: p, Resolver: dopContents{"a": qualifyWAV(44100)}, Locator: fakeNodeLocator{},
		OpenSink: k.OpenSink, Seams: &playback.Seams{DACTokenKey: goldenTokenKey}})
	if err != nil {
		t.Fatal(err)
	}
	backend.RefreshDSD(true)
	r := newChoiceRig(t, backend)
	queue(t, r.eng, 0, someReference(t, "a"))
	body := choiceBody(t, dacOf(t, backend.Policy()), dsdstate.ModeConvert)
	box.armed.Store(true)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		backend.RefreshDSD(true)
	}()
	drainLoopOnCleanup(t, box.let, refreshed, "the parked refresh")
	select {
	case <-box.came:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never parked in its reading of the DAC")
	}
	r.budget = 500 * time.Millisecond
	answer, _ := r.putAsync(t, context.Background(), body)
	wantAnswer(t, "the write", r.wait(t, answer, "the PUT's answer, the refresh still parked"), http.StatusInternalServerError, "persist_failed", 4)
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the engine's reservation is held once the PUT answered")
	}
	r.budget = 0
	// The Play answers once its Start has: through the backend's lease to the
	// device and its first frames in the sink, the refresh still parked. The
	// fake DAC is given no time, so nothing is heard and the session stays
	// starting.
	if err := answered(t, async(engineCmd(r.eng.Play)), "the Play, the refresh still parked"); err != nil {
		t.Fatalf("the Play, the refresh still parked: %v", err)
	}
	awaitEngine(t, r.eng, "playing", func(sn *engine.Snapshot) bool { return sn.State() == engine.Playing })
	if v := k.View(); v.Opens != 1 || !v.Open {
		t.Fatalf("the device was opened %d times (open now: %v) while the refresh is parked; want the Start's one, open", v.Opens, v.Open)
	}
	box.let()
	select {
	case <-refreshed:
	case <-serveGiveUp(t):
		t.Fatal("the refresh never ended once let go")
	}
	if n := store.Writes(); n != 0 {
		t.Errorf("the PUT that failed wrote the state %d times", n)
	}
	// The protocol table's step 8, the Start's session playing.
	wantAnswer(t, "a write while the Start's session plays", r.put(t, choiceBody(t, r.dac(t), dsdstate.ModeConvert)), http.StatusConflict, "playback_not_stopped", 4)
	if r.eng.Snapshot().ConfigurationReserved() {
		t.Fatal("the engine's reservation is held once the refused PUT answered")
	}
	if n := store.Writes(); n != 0 {
		t.Errorf("the refused PUT wrote the state %d times", n)
	}
	if err := r.eng.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	awaitEngine(t, r.eng, "stopped, the release confirmed", func(sn *engine.Snapshot) bool {
		_, unconfirmed := sn.ReleaseUnconfirmed()
		return sn.State() == engine.Stopped && !unconfirmed
	})
	wantAnswer(t, "a write once stopped", r.put(t, choiceBody(t, r.dac(t), dsdstate.ModeConvert)), http.StatusOK, "", 4)
	if n := store.Writes(); n != 1 {
		t.Errorf("the state was written %d times; want the one write made once stopped", n)
	}
	if v := k.View(); v.Opens != 1 || v.Open {
		t.Errorf("the device was opened %d times (open now: %v); want the Start's one, closed by the Stop", v.Opens, v.Open)
	}
	k.CheckClosedEach(t)
}
