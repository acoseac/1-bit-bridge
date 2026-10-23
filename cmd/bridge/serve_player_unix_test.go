//go:build unix

package main

import (
	"errors"
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

// TestServeStopWithThePlayersStorageStalled: the player's state storage
// stalls (the reservation file is a named pipe, so the engine's read of it
// at its start waits for a writer, as a write or an fsync that never
// returns would hold it): serve serves the library all the same, its stop
// abandons the player at its bound, says so and returns, exit 0, and keeps
// its data directory's lock while the stalled engine may still write
// there, so a second serve of that directory in this process is refused
// until the stall ends; then the lock is let go. The stall ends whichever
// comes first, the engine's read of the pipe or the test's release: in the
// second case the start is held before it reaches its storage (the engine's
// config, a seam) until the release is under way. Mkfifo comes from x/sys,
// which has it wherever the unix tag builds.
func TestServeStopWithThePlayersStorageStalled(t *testing.T) {
	for _, tc := range []struct {
		name string
		// later holds the start until the release is under way, so the
		// engine's read opens the pipe after it.
		later bool
	}{{name: "the read waits on the pipe before the release"}, {name: "the read reaches the pipe after the release", later: true}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			var fifo string
			var startOnce sync.Once
			starts := make(chan struct{})
			letStart := func() { startOnce.Do(func() { close(starts) }) }
			c := playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(),
				bounds: servePlayerBounds{engine: 300 * time.Millisecond, backend: 300 * time.Millisecond},
				before: func(dataDir string) {
					fifo = filepath.Join(dataDir, playerReservationFile)
					if err := unix.Mkfifo(fifo, 0o600); err != nil {
						t.Fatal(err)
					}
				}}
			if tc.later {
				c.seams = &servePlayerSeams{engineConfig: func(*engine.Config) { <-starts }}
			} else {
				letStart()
			}
			ps := bootPlayerServe(t, c)
			// unstall lets go of the stall, once: a writer opened on a
			// goroutine of its own blocks until the engine's read opens the
			// pipe, meets it whenever it does, and closes at once, so the read
			// ends at the end of the file. A writer that opens without waiting
			// fails (ENXIO) while no reader has the pipe open, which lost the
			// release when the engine's read came after it.
			var unstallOnce sync.Once
			unstalled := make(chan struct{})
			unstall := func() {
				unstallOnce.Do(func() {
					go func() {
						defer close(unstalled)
						if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
							_ = w.Close()
						}
					}()
				})
			}
			// The writer's goroutine is drained last: one the engine's read
			// never met (a test that failed before the engine reached it) is
			// met by a reader of the test's own, which opens without waiting
			// and is held open until the writer's open has returned (macOS
			// sends a writer back to sleep when the reader it woke for has
			// gone), and closed by the cleanup registered before the drain's,
			// which runs after it.
			var meetMu sync.Mutex
			var meet *os.File
			t.Cleanup(func() {
				meetMu.Lock()
				defer meetMu.Unlock()
				if meet != nil {
					_ = meet.Close()
				}
			})
			drainLoopOnCleanup(t, func() {
				select {
				case <-unstalled:
					return
				default:
				}
				if r, err := os.OpenFile(fifo, os.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
					meetMu.Lock()
					meet = r
					meetMu.Unlock()
				}
			}, unstalled, "the writer that lets go of the stall")
			// However the test ends, the stall is let go before its
			// directories go.
			t.Cleanup(func() {
				letStart()
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
		})
	}
}
