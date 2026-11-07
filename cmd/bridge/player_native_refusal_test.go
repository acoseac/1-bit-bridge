package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

type refusedNativeResolver []byte

func (f refusedNativeResolver) Open(context.Context, engine.Reference) (playback.Content, error) {
	return playback.Content{Data: bytes.NewReader(f), Size: int64(len(f))}, nil
}

type refusedNativeLocator struct{ calls *int }

func (l refusedNativeLocator) Locate(context.Context) (playback.Unit, error) {
	*l.calls++
	return playback.Unit{}, errors.New("unexpected hardware resolution")
}

// playOneOnEngine runs an engine over b until the test ends, queues the
// reference name, makes the output available and asks the engine to play:
// Play's answer is the caller's to check, beside the engine it ran on. what
// names the engine in its drain's timeout message.
func playOneOnEngine(t *testing.T, b playBackend, name, what string) (*engine.Engine, error) {
	t.Helper()
	return playOnEngine(t, b, what, name)
}

// playOnEngine is playOneOnEngine with the references names queued in order.
func playOnEngine(t *testing.T, b playBackend, what string, names ...string) (*engine.Engine, error) {
	t.Helper()
	dir := t.TempDir()
	eng, e := engine.New(engine.Config{Backend: b, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if e := b.Wait(c); e != nil {
			t.Error(e)
		}
	})
	drainLoopOnCleanup(t, cancel, done, what)
	var after uint32
	for _, name := range names {
		ref, _ := engine.NewReference(name)
		if after, e = eng.InsertAfter(ctx, after, ref, nil); e != nil {
			t.Fatal(e)
		}
	}
	if e = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); e != nil {
		t.Fatal(e)
	}
	return eng, eng.Play(ctx)
}

// TestNativeProductionConstructorsRefuse: serve's and output play's
// constructors, given the effective DSD policy as their wiring gives it
// (B728 B7), refuse DSD the policy does not play before any hardware is
// located or opened, by the policy's word: the bound DAC with no fresh
// reading of it is dsd_dac_unread, and a DAC no profile knows, with no choice
// of its own, converts under the bridge's auto, which is dsd_not_in_this_release
// until the player converts (B11).
func TestNativeProductionConstructorsRefuse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	persistentMedium(t)
	policies := map[engine.Reason]func() *playback.DSDPolicy{
		playback.ReasonDACUnread: func() *playback.DSDPolicy {
			p := playbacktest.Policy(5, playbacktest.NewBox(5), playbacktest.NewMemStore(playbacktest.ChoiceState(dsdstate.ModeNative)))
			p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
			return p
		},
		playback.ReasonDSD: func() *playback.DSDPolicy {
			return playbacktest.Policy(5, playbacktest.NewBox(5), playbacktest.NewMemStore(dsdstate.Defaults()))
		},
	}
	for _, mode := range []string{"serve", "output-play"} {
		for _, file := range []string{"lsb_dsd64_partial_block.dsf", "dsd64.dff"} {
			for want, policy := range policies {
				t.Run(mode+"/"+file+"/"+string(want), func(t *testing.T) {
					data, e := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", file))
					if e != nil {
						t.Fatal(e)
					}
					calls, opens := 0, 0
					cfg := playback.Config{DSD: policy(), Resolver: refusedNativeResolver(data), Locator: refusedNativeLocator{&calls},
						OpenSink: func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
							opens++
							return nil, errors.New("unexpected open")
						},
						OpenNative: func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
							opens++
							return nil, errors.New("unexpected open")
						}}
					build := newServePlayerBackend
					if mode == "output-play" {
						build = newOutputPlayBackend
					}
					b, e := build(cfg)
					if e != nil {
						t.Fatal(e)
					}
					eng, e := playOneOnEngine(t, b, "native", "native refusal engine")
					if e == nil {
						t.Fatal("production played DSD its policy does not play")
					}
					skipped := eng.Snapshot().Skips()
					if len(skipped) != 1 {
						t.Fatalf("named production refusal absent: %+v", skipped)
					}
					for _, skip := range skipped {
						if skip.Reason != want {
							t.Fatalf("wrong refusal: %+v, want %s", skip, want)
						}
					}
					if calls != 0 || opens != 0 {
						t.Fatalf("production refusal reached hardware: locate=%d opens=%d", calls, opens)
					}
				})
			}
		}
	}
}
