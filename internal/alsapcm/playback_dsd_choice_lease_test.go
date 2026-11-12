package alsapcm_test

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A choice's write beside the player, through the engine's Run, the backend
// and the real sinks over the fake kernel (B728 B9 PR 2: "Stop playback to
// change DSD"): the configuration lease refuses a write in every state the
// player is not stopped in, a session's record holding it or a release left
// unconfirmed, and lets it land once a Stop is confirmed; and a Start that
// comes while a write holds the lease waits, and plays by the choice it wrote.

// choiceOf is a write of mode for the bound DAC, from the revision the
// backend publishes once it has read the policy, as serve's supervisor has
// before any request comes.
func (d *dsdRig) choiceOf(mode string) playback.ChoiceRequest {
	d.t.Helper()
	d.be.RefreshDSD(true)
	p := d.be.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		d.t.Fatalf("the backend publishes no DAC: %+v", p.DSD)
	}
	return playback.ChoiceRequest{DACKey: p.DSD.DAC.Key, Choice: policy.Choice{BaseRevision: p.DSD.DAC.Choice.Revision, Mode: mode, By: dsdstate.ByApp}}
}

// write asks a write of mode, bounded by the watchdog.
func (d *dsdRig) write(mode string) playback.ChoiceResult {
	d.t.Helper()
	ctx, cancel := within()
	defer cancel()
	return d.be.ChooseDSD(ctx, d.choiceOf(mode))
}

// refusedNotStopped fails the test unless a write is refused as
// playback_not_stopped with nothing written and nothing published: the
// state's bytes, its writes and the choice's revision as they were.
func (d *dsdRig) refusedNotStopped(what string) {
	d.t.Helper()
	d.be.RefreshDSD(true)
	state, writes, published := d.store.State(), d.store.Writes(), d.be.Policy()
	res := d.write(dsdstate.ModeConvert)
	if res.Outcome != playback.ChoicePlaybackNotStopped {
		d.t.Errorf("%s: the write answered %s; want playback_not_stopped", what, res.Outcome)
	}
	if d.store.Writes() != writes || !reflect.DeepEqual(d.store.State(), state) {
		d.t.Errorf("%s: a refused write changed the state (%d writes, was %d)", what, d.store.Writes(), writes)
	}
	if got, was := res.Policy.DSD.DAC.Choice.Revision, published.DSD.DAC.Choice.Revision; got != was {
		d.t.Errorf("%s: a refused write moved the published revision to %d from %d", what, got, was)
	}
}

// stopAndLand stops playback through the engine, waits for the release to be
// confirmed, and fails the test unless a write then lands.
func (d *dsdRig) stopAndLand() {
	d.t.Helper()
	ctx, cancel := within()
	defer cancel()
	if err := d.eng.Stop(ctx); err != nil {
		d.t.Fatalf("stop: %v", err)
	}
	d.waitSnap("stopped, the release confirmed", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	if st := d.be.Stats(); st.Sessions != 0 {
		d.t.Fatalf("%d sessions' records once the Stop was confirmed", st.Sessions)
	}
	if res := d.write(dsdstate.ModeConvert); res.Outcome != playback.ChoiceWritten {
		d.t.Errorf("a write after a confirmed Stop answered %s; want written", res.Outcome)
	}
}

// playingPCM plays a second of 16-bit PCM and waits for its writer to wait on
// the DAC, which the test holds.
func (d *dsdRig) playingPCM() {
	d.t.Helper()
	d.files.add("a", wav(44100, 16, pattern(1, 44100, 16)))
	d.insert("a")
	d.play()
	d.k.Await(d.t, "the PCM writer waiting on the DAC", runningAndWaiting)
}

// playingDSD plays a second of DSD64, native, and waits for its sink.
func (d *dsdRig) playingDSD() {
	d.t.Helper()
	d.files.add("a", dsd(playbacktest.DSD64, 1))
	d.insert("a")
	d.play()
	d.waitSnap("playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if f := d.openFormats(); len(f) != 1 {
		d.t.Fatalf("the native sinks opened: %v; want one, the DSD64 track's", f)
	}
}

// TestEachStateThePlayerIsNotStoppedInRefusesTheWrite (B728 B9 PR 2): a
// choice's write is refused as playback_not_stopped, the state's bytes and
// revision unchanged, while PCM plays, native DSD plays, a pause holds the
// device open, a pause's release has let it go, a release plays out a native
// tail, and a release was left unconfirmed (no session's record left, the
// backend's own refusal); once a Stop is confirmed the write lands.
func TestEachStateThePlayerIsNotStoppedInRefusesTheWrite(t *testing.T) {
	t.Run("PCM playing", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.playingPCM()
		d.refusedNotStopped("PCM playing")
		d.stopAndLand()
	})
	t.Run("native DSD playing", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.playingDSD()
		d.refusedNotStopped("native DSD playing")
		d.stopAndLand()
	})
	t.Run("paused with the device open", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.playingPCM()
		d.k.Play(t, 100*time.Millisecond)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("pause: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		if v := d.k.View(); !v.Open {
			t.Fatal("the pause let the device go: the case needs it held")
		}
		d.refusedNotStopped("paused with the device open")
		d.stopAndLand()
	})
	t.Run("paused, the pause's release having let the device go", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.pauseRelease = time.Second })
		d.playingPCM()
		d.k.Play(t, 100*time.Millisecond)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("pause: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		d.releaseDevice()
		d.refusedNotStopped("paused, the device let go")
		d.stopAndLand()
	})
	t.Run("a release playing out a native tail", func(t *testing.T) {
		w := newLifecycleSeams()
		d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), nil, func(o *rigOptions) { o.seams = w.seams(nil) })
		d.playingDSD()
		rel := w.holdReleasing(t)
		done := make(chan error, 1)
		go func() {
			done <- d.be.ReleaseNative(bg, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: d.sessionOf()}, Mode: engine.ReleaseTrackChange})
		}()
		awaitOrdered(t, rel.in, "the release, its native tail retiring, before the session's life ends")
		d.refusedNotStopped("a release playing out a native tail")
		rel.letGo()
		d.k.Free()
		select {
		case <-done:
		case <-time.After(alsapcm.Watchdog()):
			alsapcm.LogGoroutines(t)
			t.Fatal("the release did not end")
		}
		if res := d.write(dsdstate.ModeConvert); res.Outcome != playback.ChoiceWritten {
			t.Errorf("a write once the release ended answered %s; want written", res.Outcome)
		}
	})
	t.Run("a release left unconfirmed", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.mu.Lock()
		d.cleanup = "close"
		d.mu.Unlock()
		d.playingDSD()
		ctx, cancel := within()
		defer cancel()
		_ = d.eng.Stop(ctx)
		d.waitSnap("stopped", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
		if st := d.be.Stats(); st.Sessions != 0 {
			t.Fatalf("%d sessions' records once the release ended: the case needs none, the refusal the release's alone", st.Sessions)
		}
		d.refusedNotStopped("a release left unconfirmed")
	})
}

// rigHeldStore is the rig's state in memory whose next Update once armed
// waits until it is let go, came closed as it comes.
type rigHeldStore struct {
	*playbacktest.MemStore
	armed      atomic.Bool
	came, gate chan struct{}
	once       sync.Once
}

func (h *rigHeldStore) Update(fn func(*dsdstate.State) error) error {
	if h.armed.CompareAndSwap(true, false) {
		close(h.came)
		<-h.gate
	}
	return h.MemStore.Update(fn)
}

func (h *rigHeldStore) let() { h.once.Do(func() { close(h.gate) }) }

// TestAStartDuringAWritePlaysByTheChoiceItWrote (B728 B9 PR 2): a Start the
// engine makes while a choice's write is held inside the state's write waits
// for the configuration lease before it registers, opening nothing, and once
// the write has published plays by the choice it wrote: a DSD64 track the
// state as it was plays natively is given to the DAC converted to PCM, no
// native sink opened.
func TestAStartDuringAWritePlaysByTheChoiceItWrote(t *testing.T) {
	held := &rigHeldStore{came: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(held.let)
	var waitOnce sync.Once
	waiting := make(chan struct{})
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), func(p *playback.DSDPolicy) {
		held.MemStore = p.Store.(*playbacktest.MemStore)
		p.Store = held
	}, func(o *rigOptions) {
		o.seams = &playback.Seams{Leased: func(engine.SessionID) { waitOnce.Do(func() { close(waiting) }) }}
	})
	d.files.add("a", dsd(playbacktest.DSD64, 0.5))
	d.insert("a")
	req := d.choiceOf(dsdstate.ModeConvert)
	held.armed.Store(true)
	written := make(chan playback.ChoiceResult, 1)
	go func() {
		ctx, cancel := within()
		defer cancel()
		written <- d.be.ChooseDSD(ctx, req)
	}()
	awaitOrdered(t, held.came, "the write's state's write")
	played := make(chan error, 1)
	go func() {
		ctx, cancel := within()
		defer cancel()
		played <- d.eng.Play(ctx)
	}()
	awaitOrdered(t, waiting, "the Start's wait for the write's lease")
	if st := d.be.Stats(); st.Sessions != 0 || len(d.openFormats()) != 0 || d.pcmOpens() != 0 {
		t.Fatalf("a Start waiting for the lease: %d sessions, native opens %v, %d PCM opens; want nothing", st.Sessions, d.openFormats(), d.pcmOpens())
	}
	held.let()
	select {
	case res := <-written:
		if res.Outcome != playback.ChoiceWritten {
			t.Fatalf("the write answered %s; want written", res.Outcome)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatal("the write did not answer")
	}
	select {
	case err := <-played:
		if err != nil {
			t.Fatalf("play: %v", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatal("the play was not answered")
	}
	d.waitSnap("playing", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	d.waitCall("the Start's answer", func(c call) bool { return c.kind == "start" })
	if f, n := d.openFormats(), d.pcmOpens(); len(f) != 0 || n != 1 {
		t.Errorf("native opens %v and %d PCM opens; want the track converted to PCM, one PCM sink and no native one", f, n)
	}
	if _, live := d.be.Facts(); live == nil || live.Transport != playback.TransportConverted {
		t.Errorf("the occurrence observed %+v; want it converted", live)
	}
}
