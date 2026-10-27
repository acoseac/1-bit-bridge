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
func TestNativeProductionConstructorsRefuse(t *testing.T) {
	for _, mode := range []string{"serve", "output-play"} {
		for _, file := range []string{"lsb_dsd64_partial_block.dsf", "dsd64.dff"} {
			t.Run(mode+"/"+file, func(t *testing.T) {
				data, e := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", file))
				if e != nil {
					t.Fatal(e)
				}
				calls, opens := 0, 0
				cfg := playback.Config{Resolver: refusedNativeResolver(data), Locator: refusedNativeLocator{&calls}, OpenSink: func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
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
				dir := t.TempDir()
				eng, e := engine.New(engine.Config{Backend: b, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
				if e != nil {
					t.Fatal(e)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); _ = eng.Run(ctx) }()
				t.Cleanup(func() {
					cancel()
					<-done
					c, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if e := b.Wait(c); e != nil {
						t.Error(e)
					}
				})
				ref, _ := engine.NewReference("native")
				if _, e = eng.InsertAfter(ctx, 0, ref, nil); e != nil {
					t.Fatal(e)
				}
				if e = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); e != nil {
					t.Fatal(e)
				}
				if e = eng.Play(ctx); e == nil {
					t.Fatal("production played native DSD")
				}
				skipped := eng.Snapshot().Skips()
				if len(skipped) != 1 {
					t.Fatalf("named production refusal absent: %+v", skipped)
				}
				for _, skip := range skipped {
					if skip.Reason != playback.ReasonDSD {
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
