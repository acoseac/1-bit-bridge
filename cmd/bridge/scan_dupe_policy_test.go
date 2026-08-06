package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// `bridge scan` must wire the configured duplicates policy, exactly as
// `bridge serve` does.
//
// Scan's success tail ALWAYS runs the duplicate stamping pass, and an
// unwired Scanner reports dupes.FilterOff (the documented fail-open
// default for tests and bare CLI use). Under FilterOff PlanSuppression
// returns nil for every group, so every currently-suppressed row is
// diffed back to served: the stamps are cleared, indexed_at
// strict-advances on each cleared row — pushing the whole suppressed
// population into every paired device's next delta — and the persisted
// dupe_summary is rewritten claiming `policy: "off"`. One manual
// `bridge scan` on a bridge running the default policy was enough.
//
// This is the same failure mode SetDupePolicy's "wire it BEFORE the
// scan starts" contract exists to prevent in `serve`, reached through
// the other entry point.
//
// Fixture: a self-nested pair — the same track uploaded twice, once one
// directory deeper. Both files derive identical Title/Album/Artist from
// their paths (`fillFromPath` reads the immediate parent and
// grandparent, which the extra "Artist" segment leaves unchanged), so
// they share a client key; and their collapsed paths are equal, which is
// what puts them in the self-nested tier. That tier is classified from
// filesystem facts alone, so it needs no real audio geometry — the
// format-derived tiers would demote these placeholder files to
// `inconclusive`, which is never suppressed by design.
func TestScanCmdHonoursConfiguredDuplicatesFilter(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	// Shallow copy — the nest-twin winner (fewest path segments).
	shallow := filepath.Join(lib, "Artist", "Album", "01 Song.flac")
	// The accidental re-upload, one level deeper. Collapses to the same
	// path, so it is the twin the policy suppresses.
	deep := filepath.Join(lib, "Artist", "Artist", "Album", "01 Song.flac")
	for _, p := range []string{shallow, deep} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a real flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A NON-DEFAULT policy on purpose: `same-format` also suppresses
	// self-nested twins, so a scanCmd that hardcoded the
	// `highest-quality` default rather than reading the operator's
	// config would still pass — which is not the contract. The contract
	// is that the CLI honours what is configured. (Pinning the value
	// through to the summary below is what closes that gap.)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " +
		filepath.Join(dir, "data") + "\nadminAddress: 127.0.0.1:0\n" +
		"duplicates:\n  filter: " + config.DuplicatesFilterSameFormat + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var so, se bytes.Buffer
	if code := scanCmd(context.Background(),
		[]string{"--config", cfgPath}, &so, &se); code != 0 {
		t.Fatalf("scanCmd exit %d: %s", code, se.String())
	}

	ctx := context.Background()
	store, err := manifest.OpenStore(filepath.Join(dir, "data", "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Both rows are indexed — suppression is a serve-time filter, the
	// store keeps every copy.
	total, err := store.CountTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("indexed %d tracks, want 2 (fixture did not land as expected)", total)
	}

	served, err := store.ListServedTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 {
		paths := make([]string, 0, len(served))
		for _, tr := range served {
			paths = append(paths, tr.Path)
		}
		t.Fatalf("served %d of 2 rows (%v), want 1: `bridge scan` did not wire "+
			"the duplicates policy, so its stamping pass ran under FilterOff and "+
			"un-suppressed the library", len(served), paths)
	}
	if want := "Artist/Album/01 Song.flac"; served[0].Path != want {
		t.Errorf("served path = %q, want the shallowest twin %q", served[0].Path, want)
	}

	// The persisted summary records the policy the pass ran under. This
	// is what pins that the CONFIGURED value reached the scanner rather
	// than any hardcoded default.
	sum, err := store.LoadDupeSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum == nil {
		t.Fatal("no dupe summary persisted by the scan")
	}
	if sum.Policy != config.DuplicatesFilterSameFormat {
		t.Errorf("dupe_summary policy = %q, want %q — the scan stamped under a "+
			"policy the operator did not configure", sum.Policy, config.DuplicatesFilterSameFormat)
	}
	if sum.Suppressed != 1 {
		t.Errorf("dupe_summary suppressed = %d, want 1", sum.Suppressed)
	}
}
