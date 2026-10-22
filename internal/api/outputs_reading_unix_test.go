//go:build unix

package api

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// TestAnIncludeThatIsANamedPipeEndsTheReadingInTime: an open of a named
// pipe waits for a writer and nothing cancels it, so mpd.conf's include of
// one is refused at once (conf.ReadFile opens without waiting) and the
// reading answers, the file read only in part, rather than wait for good.
func TestAnIncludeThatIsANamedPipeEndsTheReadingInTime(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	pipe := filepath.Join(filepath.Dir(rig.confPath), "pipe.conf")
	fsutiltest.MakeFIFO(t, pipe)
	rig.writeConf(volumioConf(t, rig.music) + "include \"pipe.conf\"\n")
	var got OutputReading
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, _, err = srv.localOutputs.Reading(context.Background(), id)
	}()
	if !fsutiltest.AwaitPastFIFOs(t, "a reading of an mpd.conf that includes a named pipe", 5*time.Second, done, pipe) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	want := []string{signalpath.MPDConfigIncomplete, signalpath.ALSAChainUnproven}
	if got.State != "whole" || !slices.Equal(got.Verdict.Reasons, want) {
		t.Fatalf("state %s verdict %+v, want %v", got.State, got.Verdict, want)
	}
}
