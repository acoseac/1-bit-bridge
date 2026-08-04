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
	// A NON-DEFAULT threshold on purpose. With the default of 3 this
	// test would still pass against a scanner that hardcoded 3 rather
	// than reading the config, which is not the contract — the contract
	// is that `bridge scan` honours whatever the operator configured.
	const threshold = 4
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " +
		filepath.Join(dir, "data") + "\nadminAddress: 127.0.0.1:0\n" +
		"scanner:\n  deleteAfterMissingScans: 4\n"
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

	trackRow := func() *manifest.Track {
		t.Helper()
		store, err := manifest.OpenStore(filepath.Join(dir, "data", "bridge.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		got, err := store.GetTrack(context.Background(), "song.flac")
		if err != nil {
			t.Fatalf("GetTrack: %v", err)
		}
		return got
	}

	runScan() // indexes the track
	if err := os.Remove(track); err != nil {
		t.Fatal(err)
	}

	// Below the boundary: the row must survive every scan up to
	// threshold-1. Pre-fix the CLI ran at the unwired fallback of 1 and
	// reaped it on the very first pass.
	for i := 1; i < threshold; i++ {
		runScan()
		if trackRow() == nil {
			t.Fatalf("`bridge scan` reaped the row after %d missing scan(s), "+
				"below the configured threshold of %d: the CLI is not honouring "+
				"scanner.deleteAfterMissingScans", i, threshold)
		}
	}

	// AT the boundary it must actually delete — otherwise a scanner
	// that simply never reaps would satisfy the assertions above.
	runScan()
	if trackRow() != nil {
		t.Fatalf("row survived %d missing scans; it must be reaped once the "+
			"configured threshold of %d is reached", threshold, threshold)
	}
}
