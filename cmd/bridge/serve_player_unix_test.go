//go:build unix

package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// TestServeStopWithThePlayersStorageStalled: the player's start stalls on
// its state storage, as a write or an fsync that never returns holds it
// (held here before the engine is built, at the engine's config, a seam):
// serve serves the library all the same, its stop abandons the player at its
// bound, says so and returns, exit 0, and keeps its data directory's lock
// while the stalled start may still write there, so a second serve of that
// directory in this process is refused until the stall ends; then the lock
// is let go, and the engine the start went on to build never runs.
func TestServeStopWithThePlayersStorageStalled(t *testing.T) {
	rec := loggingtest.Record(t)
	var startOnce sync.Once
	starts := make(chan struct{})
	letStart := func() { startOnce.Do(func() { close(starts) }) }
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
		bounds: servePlayerBounds{engine: 300 * time.Millisecond, backend: 300 * time.Millisecond},
		seams:  &servePlayerSeams{engineConfig: func(*engine.Config) { <-starts }}})
	// However the test ends, the stall is let go before its directories go.
	t.Cleanup(func() {
		letStart()
		select {
		case <-ps.player.settled:
		case <-serveGiveUp(t):
			t.Errorf("the player did not end once its stall was let go")
		}
	})
	if code := ps.health(t); code != http.StatusOK {
		t.Fatalf("GET /v1/health while the player's storage stalls: %d", code)
	}
	if closedNow(ps.player.ready) {
		t.Fatalf("the player is ready, its storage stalled: %+v, %q", ps.player.plan, ps.player.failed)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	if lines := rec.Failures("player: did not stop within its bound: its start or its state storage is stalled; it is abandoned and ends with the process, and serve keeps its data directory's lock until then"); len(lines) != 1 {
		t.Errorf("the abandonment logged %q, want once", lines)
	}
	release, err := lockServeDataDir(ps.dataDir)
	release()
	var held *serveDataDirHeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second serve's lock with the player stalled: %v, want it held", err)
	}
	letStart()
	select {
	case <-ps.player.settled:
	case <-serveGiveUp(t):
		t.Fatalf("the player did not end once its stall was let go\nserve's goroutines:\n%s", serveStacks())
	}
	release, err = lockServeDataDir(ps.dataDir)
	release()
	if err != nil {
		t.Errorf("a second serve's lock once the stall ended: %v, want it taken", err)
	}
	if ps.player.eng != nil {
		t.Error("an engine stopped while it started ran")
	}
}

// TestANamedPipeAtThePlayersFilesDoesNotHoldItsStart: a named pipe at the
// engine's reservation path or its queue path is no file: the engine refuses
// it at once and moves it aside, as any bad file, and the player starts,
// where its start waited for a writer to the pipe. Mkfifo comes from x/sys,
// which has it wherever the unix tag builds.
func TestANamedPipeAtThePlayersFilesDoesNotHoldItsStart(t *testing.T) {
	needsDirectorySync(t)
	for _, name := range []string{playerReservationFile, playerQueueFile} {
		t.Run(name, func(t *testing.T) {
			var fifo string
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
				before: func(dataDir string) {
					fifo = filepath.Join(dataDir, name)
					if err := unix.Mkfifo(fifo, 0o600); err != nil {
						t.Fatal(err)
					}
				}})
			ps.runningEngine(t)
			aside, _ := filepath.Glob(fifo + ".bad-*")
			if len(aside) != 1 {
				t.Fatalf("moved aside: %v, want the pipe", aside)
			}
			if info, err := os.Lstat(aside[0]); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
				t.Fatalf("what was moved aside is not the pipe: %v, %v", info, err)
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
			}
		})
	}
}
