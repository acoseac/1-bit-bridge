package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
)

// TestPlayerWatchRequestsReleaseWithoutPromisingItsOutcome exercises the
// watch's run.done arm before shutdown has attempted any bounded release.
func TestPlayerWatchRequestsReleaseWithoutPromisingItsOutcome(t *testing.T) {
	needsDirectorySync(t)
	rec := loggingtest.Record(t)
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: newPlayerStandIn(), DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
	if err != nil {
		t.Fatal(err)
	}
	sub := hotplug.NewFeed().Subscribe()
	defer sub.Close()
	done := make(chan struct{})
	close(done)
	p := &bridgePlayer{
		run:     &playerRun{eng: eng, done: done, sub: sub},
		live:    func() hotplug.Settings { return hotplug.Settings{Enabled: true} },
		changed: func() <-chan struct{} { return nil },
	}
	p.watch(context.Background())
	var lines []string
	for _, line := range rec.All() {
		if strings.Contains(line, "player: the engine stopped on its own") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "the backend is asked to let go of the device") {
		t.Errorf("watch logged %q; want a release request, with its outcome left to shutdown", lines)
	}
}
