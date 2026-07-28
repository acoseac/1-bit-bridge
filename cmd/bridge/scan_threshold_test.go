package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// `bridge scan` must honour the same missing_count grace period as
// `bridge serve`. Pre-fix scanCmd constructed a Scanner and never
// called SetDeleteThreshold, so effectiveDeleteThreshold fell back to
// its unwired default of 1 — immediate delete — while serve ran at the
// configured default of 3.
//
// The consequence: a manual `bridge scan` during a transient NAS drop,
// permission flap, or antivirus lock reaped rows on the FIRST pass,
// with none of the grace the resilience work exists to provide. Same
// class as the "bridge lost my library" reports, just reachable from
// the CLI instead of the daemon.
//
// Two scans with the file removed: at threshold 3 the row must survive
// both (counter reaches 2). At the pre-fix threshold of 1 it is gone
// after the first.
func TestScanCmdHonoursConfiguredDeleteThreshold(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(lib, "song.flac")
	if err := os.WriteFile(track, []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A surviving sibling keeps the root non-empty. Without it the
	// walk observes zero entries and the empty-root sentinel spares
	// EVERY row from the deletion pass — the test would then pass at
	// any threshold and pin nothing.
	if err := os.WriteFile(filepath.Join(lib, "keeper.flac"),
		[]byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " +
		filepath.Join(dir, "data") + "\nadminAddress: 127.0.0.1:0\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	runScan := func() {
		t.Helper()
		var so, se bytes.Buffer
		if code := scanCmd(context.Background(),
			[]string{"--config", cfgPath}, &so, &se); code != 0 {
			t.Fatalf("scanCmd exit %d: %s", code, se.String())
		}
	}

	runScan() // indexes the track
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}
	runScan() // missing_count 1
	runScan() // missing_count 2 — still below the default threshold of 3

	store, err := manifest.OpenStore(filepath.Join(dir, "data", "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetTrack(context.Background(), "song.flac")
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if got == nil {
		t.Fatal("`bridge scan` reaped the row before the configured " +
			"missing-scan threshold: SetDeleteThreshold is not wired")
	}
}
