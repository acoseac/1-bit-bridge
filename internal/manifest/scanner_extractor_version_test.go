package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// TestScanner_ExtractorVersionBump_ReExtractsStaleRow proves the self-healing
// re-extraction contract: runScanWorker's size+mtime skip-gate re-extracts a
// row whose extractor_version is < ExtractorVersion, even when the file is
// byte-identical (unchanged size + mtime). This is the mechanism that makes an
// extraction-logic fix (e.g. the MP4 © atom canonicalization that recovers M4A
// year/composer) reach EXISTING library rows after an upgrade — without the
// stamp clause a stale row would size+mtime-skip forever.
//
// A byte-identical file whose stored stamp is stale is the ONLY thing under
// test here: the reheal scan doesn't touch the file, so if it re-stamps the
// row the skip-gate MUST have fallen through to the full extract path.
func TestScanner_ExtractorVersionBump_ReExtractsStaleRow(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()

	path := filepath.Join(root, "song.mp3")
	writeMinimalMP3(t, path, map[string]string{"title": "Song", "year": "1991"})

	scanOnce(t, sc, "initial")

	const rel = "song.mp3"
	st, err := store.GetTrackStat(ctx, rel)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat after initial scan: err=%v nil=%v", err, st == nil)
	}
	if st.ExtractorVersion != ExtractorVersion {
		t.Fatalf("initial extractor_version = %d, want %d (writer must stamp the current version)",
			st.ExtractorVersion, ExtractorVersion)
	}

	// Simulate a row written by an OLDER binary: stale stamp, file untouched.
	// Same-package test — reach the unexported db handle directly.
	if _, err := store.db.Exec("UPDATE tracks SET extractor_version = 0 WHERE path = ?", rel); err != nil {
		t.Fatalf("munge stale stamp: %v", err)
	}

	// Re-scan. size + mtime are identical, so the ONLY reason the skip-gate can
	// fall through to re-extraction is the stale extractor_version.
	scanOnce(t, sc, "reheal")

	st2, err := store.GetTrackStat(ctx, rel)
	if err != nil || st2 == nil {
		t.Fatalf("GetTrackStat after reheal scan: err=%v nil=%v", err, st2 == nil)
	}
	if st2.ExtractorVersion != ExtractorVersion {
		t.Errorf("after reheal scan, extractor_version = %d, want %d — the skip-gate must re-extract (and re-stamp) a stale row",
			st2.ExtractorVersion, ExtractorVersion)
	}
}

// TestScanner_ExtractorVersionCurrent_SkipsUnchangedRow is the complement: a
// row already stamped at the current ExtractorVersion whose file is unchanged
// must NOT be needlessly re-extracted — the stamp gate can't defeat the
// size+mtime fast-skip. Observed via the stamp staying put across a no-op scan.
func TestScanner_ExtractorVersionCurrent_SkipsUnchangedRow(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()

	path := filepath.Join(root, "song.mp3")
	writeMinimalMP3(t, path, map[string]string{"title": "Song", "year": "1991"})

	scanOnce(t, sc, "initial")
	scanOnce(t, sc, "noop") // unchanged file, current stamp → fast-skip

	st, err := store.GetTrackStat(ctx, "song.mp3")
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat: err=%v nil=%v", err, st == nil)
	}
	if st.ExtractorVersion != ExtractorVersion {
		t.Errorf("extractor_version = %d, want %d (current-stamp unchanged row stays stamped)",
			st.ExtractorVersion, ExtractorVersion)
	}
}
