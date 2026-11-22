package alsapcm_test

import (
	"errors"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// nativePanicMessage is the line a native sink logs a panic it recovered
// with.
const nativePanicMessage = "player: a panic was recovered in the native DSD sink; it ends typed and its node is closed"

// opensNothing plays a PCM entry and fails the test unless it is refused and
// the device stays as it was: no open of the node.
func (d *dsdRig) opensNothing(what string) {
	d.t.Helper()
	opens := d.k.View().Opens
	d.files.add("z", wav(48000, 24, pattern(9, 20000, 24)))
	ids := d.insert("z")
	ctx, cancel := within()
	defer cancel()
	if err := d.eng.PlayID(ctx, ids[0]); err == nil {
		d.t.Errorf("%s answered nil", what)
	}
	if v := d.k.View(); v.Opens != opens || v.Open {
		d.t.Errorf("%s opened the device: %d opens, were %d", what, v.Opens, opens)
	}
}

// openPanicking has the next check after open panic, its node's close
// failing too where closeFails, plays the DSD entry a, and fails the test
// unless the play fails, the node is closed once and nothing is written.
func (d *dsdRig) openPanicking(closeFails bool) {
	d.t.Helper()
	d.files.add("a", dsd(playbacktest.DSD64, 1))
	d.insert("a")
	d.unit.mu.Lock()
	d.unit.panics = panicValue
	d.unit.mu.Unlock()
	if closeFails {
		d.k.FailNextClose(errors.New("the node's close failed"))
	}
	was := d.k.View()
	ctx, cancel := within()
	defer cancel()
	if err := d.eng.Play(ctx); err == nil {
		d.t.Fatal("the play whose open panicked answered nil")
	}
	if v := d.k.View(); v.Opens != was.Opens+1 || v.Closes != was.Closes+1 || v.Open || v.Writes != was.Writes {
		d.t.Errorf("the open whose check panicked: %d opens, %d closes, open %v, %d writes, from %d, %d and %d; want its node closed once, nothing written",
			v.Opens, v.Closes, v.Open, v.Writes, was.Opens, was.Closes, was.Writes)
	}
}

// TestABackendSurvivesAPanicOnANativeSinksGoroutines pins a panic on a native
// sink's goroutines through the engine's Run, the backend and the real native
// sink over the fake kernel (B728 B12 PR 0), each case in a child process
// (alsapcm.RunEachCaseInAChild). A release whose close panics is a release
// unconfirmed, after which nothing opens the device. An open whose check
// panics fails its native session's start, which faults it backend_error, the
// engine's word for a backend call that failed (ReasonBackend), nothing
// written and its node closed once, and the next play plays on; one whose
// node's close failed too fences the device as a release unconfirmed does. A
// writer that panics faults its session backend_failed, the backend's own
// fault (ReasonFailed), never the device's. Each panic is logged once, naming
// no value.
func TestABackendSurvivesAPanicOnANativeSinksGoroutines(t *testing.T) {
	unconfirmed := func(s *engine.Snapshot) bool {
		_, u := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && u
	}
	alsapcm.RunEachCaseInAChild(t, "TestABackendSurvivesAPanicOnANativeSinksGoroutines", map[string]func(t *testing.T){
		"a release whose close panics": func(t *testing.T) {
			logs := &lines{}
			d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), withLog(logs))
			d.playingDSD()
			d.k.PanicAtNextClose(panicValue)
			ctx, cancel := within()
			defer cancel()
			_ = d.eng.Stop(ctx)
			d.waitSnap("stopped, the release unconfirmed", unconfirmed)
			d.opensNothing("a play after the release its close's panic left unconfirmed")
			logs.panicLogged(t, nativePanicMessage, "the native sink's close")
		},
		"an open whose check panics": func(t *testing.T) {
			logs := &lines{}
			d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), withLog(logs))
			d.openPanicking(false)
			if f := d.waitStopped(engine.ReasonFault).Faults(); len(f) != 1 || f[0].Reason != engine.ReasonBackend {
				t.Errorf("faults %+v; want one, the start's failure (%s)", f, engine.ReasonBackend)
			}
			logs.panicLogged(t, nativePanicMessage, "the native sink's opener")
			d.playsOn("b")
		},
		"an open whose check panics, its close failing": func(t *testing.T) {
			logs := &lines{}
			d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), withLog(logs))
			d.openPanicking(true)
			d.waitSnap("stopped, the release unconfirmed", unconfirmed)
			d.opensNothing("a play after the open whose close failed")
			logs.panicLogged(t, nativePanicMessage, "the native sink's opener")
		},
		"a writer that panics as it holds a pause": func(t *testing.T) {
			logs := &lines{}
			d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), withLog(logs))
			d.playingDSD()
			ctx, cancel := within()
			defer cancel()
			if err := d.eng.Pause(ctx); err != nil {
				t.Fatalf("pause: %v", err)
			}
			d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
			// The writer's next read of the clock is its pause's.
			d.k.PanicAtNextClockRead(panicValue)
			d.k.Grant(time.Second)
			s := d.tickUntilStopped(engine.ReasonFault)
			if f := s.Faults(); len(f) != 1 || f[0].Reason != playback.ReasonFailed {
				t.Errorf("faults %+v; want one, the backend's own (%s)", f, playback.ReasonFailed)
			}
			d.letGo()
			logs.panicLogged(t, nativePanicMessage, "the native sink's writer")
			d.playsOn("b")
		},
	})
}
