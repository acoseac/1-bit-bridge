package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
func TestNativeProductionConstructorsRefuse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	persistentMedium(t)
	for _, mode := range []string{"serve", "output-play"} {
		for _, file := range []string{"lsb_dsd64_partial_block.dsf", "dsd64.dff"} {
			t.Run(mode+"/"+file, func(t *testing.T) {
				data, e := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", file))
				if e != nil {
					t.Fatal(e)
				}
				calls, opens := 0, 0
				cfg := playback.Config{Authorization: &playback.Authorization{Mode: "native", StateDir: t.TempDir(), Record: "missing-approved.json"}, Resolver: refusedNativeResolver(data), Locator: refusedNativeLocator{&calls}, OpenSink: func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
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
					t.Fatal("production played native DSD")
				}
				skipped := eng.Snapshot().Skips()
				if len(skipped) != 1 {
					t.Fatalf("named production refusal absent: %+v", skipped)
				}
				// dsdMode native asks for DSD, and the approved record it names
				// is not there: the refusal names the approval (B470 2d.2).
				for _, skip := range skipped {
					if skip.Reason != playback.ReasonDSDApproval {
						t.Fatalf("wrong refusal: %+v", skip)
					}
				}
				if calls != 0 || opens != 0 {
					t.Fatalf("production refusal reached hardware: locate=%d opens=%d", calls, opens)
				}
			})
		}
	}
}
