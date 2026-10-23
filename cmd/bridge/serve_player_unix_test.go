//go:build unix

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestServeStopWithThePlayersStorageStalled: the player's state storage
// stalls (the reservation file is a named pipe, so the engine's read of it
// at its start waits for a writer, as a write or an fsync that never
// returns would hold it): serve serves the library all the same, its stop
// abandons the player at its bound, says so and returns, exit 0, and keeps
// its data directory's lock while the stalled engine may still write
// there, so a second serve of that directory in this process is refused
// until the stall ends; then the lock is let go. Mkfifo comes from x/sys,
// which has it wherever the unix tag builds.
func TestServeStopWithThePlayersStorageStalled(t *testing.T) {
	rec := loggingtest.Record(t)
	var fifo string
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
		bounds: servePlayerBounds{engine: 300 * time.Millisecond, backend: 300 * time.Millisecond},
		before: func(dataDir string) {
			fifo = filepath.Join(dataDir, playerReservationFile)
			if err := unix.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
		}})
	// Let go of the stall however the test ends, before its directories
	// go: a writer that opens without waiting finds the engine's read, or
	// nobody.
	unstall := func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	}
	t.Cleanup(func() {
		unstall()
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
	unstall()
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
