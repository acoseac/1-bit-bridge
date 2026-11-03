package main

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// TestThePlayersElementSaysWhatASeekTakes pins bridge.timeSeek's facts
// (B674): pcm supported and dsd unsupported, read from the engine's TimeSeek,
// which its seek's refusal reads too, in every state of the player, an
// unavailable one included (what the player implements, never whether a seek
// can be made now), and none while no element is listed.
func TestThePlayersElementSaysWhatASeekTakes(t *testing.T) {
	if !engine.TimeSeek(false) || engine.TimeSeek(true) {
		t.Fatalf("the engine seeks in PCM %v and in DSD %v, want PCM alone", engine.TimeSeek(false), engine.TimeSeek(true))
	}
	if pcm, dsd := timeSeekWord(engine.TimeSeek(false)), timeSeekWord(engine.TimeSeek(true)); pcm != api.TimeSeekSupported || dsd != api.TimeSeekUnsupported {
		t.Fatalf("the words: pcm %q, dsd %q", pcm, dsd)
	}
	needsDirectorySync(t)
	cfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Device = config.PlayerDevice{} })
	plan := planServePlayer(cfg, "linux", true)
	p := startPlayerDirect(t, plan, cfg, nil, nil)
	out := newBridgeOutput(plan, p, cfg)
	if f := out.Facts(context.Background()); f.Listed || f.TimeSeekPCM != "" || f.TimeSeekDSD != "" {
		t.Fatalf("a player with no identity: %+v, want no element and no facts", f)
	}
	seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
	f := out.Facts(context.Background())
	if !f.Listed || f.State != bridgeStateUnavailable || f.TimeSeekPCM != api.TimeSeekSupported || f.TimeSeekDSD != api.TimeSeekUnsupported {
		t.Errorf("an unavailable player's facts: listed %v, %s, timeSeek pcm %q dsd %q; want supported and unsupported whatever its state",
			f.Listed, f.State, f.TimeSeekPCM, f.TimeSeekDSD)
	}
}
