package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// deniedOpen is what the real backend answers a Start or a Resume whose DAC
// node this process may not open (B676): engine.ErrDeviceDenied beside
// alsapcm's permission refusal, behind the backend's words.
func deniedOpen() error {
	return fmt.Errorf("the stand-in could not open its DAC: %w", errors.Join(
		errors.New("alsapcm: card 5 device 0: open: permission_denied: this process may not open /dev/snd/pcmC5D0p (the audio group owns it) (EACCES)"),
		engine.ErrDeviceDenied))
}

// deniedStandIn is the stand-in backend with a Start, and a Resume, that
// fail as the real backend's do for a DAC node this process may not open,
// while denyStart and denyResume are set.
type deniedStandIn struct {
	*playerStandIn
	denyStart, denyResume atomic.Bool
}

func (b *deniedStandIn) Start(ctx context.Context, req engine.StartRequest) error {
	if b.denyStart.Load() {
		return deniedOpen()
	}
	return b.playerStandIn.Start(ctx, req)
}

func (b *deniedStandIn) Resume(ctx context.Context, s engine.SessionID) (engine.ResumeResult, error) {
	if b.denyResume.Load() {
		return 0, deniedOpen()
	}
	return b.playerStandIn.Resume(ctx, s)
}

// TestThePlayersPermissionCauseReachesTheConsoleAndTheLog boots serve with
// its player on a stand-in whose opens are refused for permission (B676),
// through the real engine: a start refused is the console's permission_denied
// (GET /api/output/player's running.lastError), while GET /v1/outputs'
// lastError stays null, its closed list device_busy's alone; a start that
// succeeds clears it; a resume refused for permission, which moves no
// generation, is logged with its cause and its words, the session paused, and
// is the console's permission_denied again; a resume that succeeds clears it.
func TestThePlayersPermissionCauseReachesTheConsoleAndTheLog(t *testing.T) {
	needsDirectorySync(t)
	logs := watchLog(t)
	standIn := newPlayerStandIn()
	backend := &deniedStandIn{playerStandIn: standIn}
	var bearer string
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		before: func(dataDir string) { bearer = mintBearer(t, dataDir) },
		with: func(o *serveOpts) {
			o.playerBackend = func(cfg playback.Config) (playBackend, error) {
				if _, err := standIn.build(cfg); err != nil {
					return nil, err
				}
				return backend, nil
			}
		}})
	eng := cs.runningEngine(t)
	availableOutput(t, eng)
	consoleSays := func(what, want string) {
		t.Helper()
		if run := cs.status(t).Running; run.LastError != want {
			t.Fatalf("%s: the console's lastError %q, want %q (%+v)", what, run.LastError, want, run)
		}
	}
	consoleSays("before any play", "")

	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	backend.denyStart.Store(true)
	if err := eng.Play(context.Background()); !errors.Is(err, engine.ErrDeviceDenied) {
		t.Fatalf("a play on a node this process may not open answered %v", err)
	}
	awaitEngine(t, eng, "the denied start stopped", func(s *engine.Snapshot) bool {
		return s.State() == engine.Stopped && s.LastAttempt().Denied
	})
	wantElement(t, "after a denied start", bridgeElementOf(t, cs.playerServe, bearer), "stopped", "", "")
	consoleSays("after a denied start", "permission_denied")

	backend.denyStart.Store(false)
	if err := eng.Play(context.Background()); err != nil {
		t.Fatalf("play: %v", err)
	}
	wantElement(t, "playing", bridgeElementOf(t, cs.playerServe, bearer), "playing", "", "")
	consoleSays("after a start that succeeded", "")

	if err := eng.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	backend.denyResume.Store(true)
	if err := eng.Play(context.Background()); !errors.Is(err, engine.ErrDeviceDenied) {
		t.Fatalf("a resume on a node this process may not open answered %v", err)
	}
	if s := eng.Snapshot(); s.State() != engine.Paused || !s.LastAttempt().Resume {
		t.Fatalf("after the denied resume: %s, %+v; want paused, the attempt a resume", s.State(), s.LastAttempt())
	}
	line := logs.await(t, "player: a resume failed", "permission_denied")
	if !strings.Contains(line, "pcmC5D0p") {
		t.Errorf("the resume's line %q does not carry the refusal's words", line)
	}
	wantElement(t, "after a denied resume", bridgeElementOf(t, cs.playerServe, bearer), "paused", "", "")
	consoleSays("after a denied resume", "permission_denied")

	backend.denyResume.Store(false)
	if err := eng.Play(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	consoleSays("after a resume that succeeded", "")
	if n := strings.Count(logs.all(), "player: a resume failed"); n != 1 {
		t.Errorf("the denied resume was logged %d times, want once:\n%s", n, logs.all())
	}
	if code := cs.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, cs.stderr.String())
	}
}
