package manifest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// rolledBackUpsertSQL is the track upsert of the bridge before B513 (main at
// b6dd72d7, prepareTrackUpsert), captured as it was written there. It names
// no sample_format column: its insert leaves a new row's kind NULL, and its
// conflict arm keeps whatever kind the row held while it rewrites the row's
// size, mtime and tags for a file that changed.
const rolledBackUpsertSQL = `
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at,
		                   sample_rate, bits_per_sample, is_dsd, codec,
		                   extractor_version, audio_md5, compression,
		                   folder_art_key, extract_refused, first_indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size          = excluded.size,
			mtime_ns      = excluded.mtime_ns,
			tags_json     = excluded.tags_json,
			-- The bound indexed_at is the stamp readNextDeltaStamp computed
			-- before this statement. One value for every row of the
			-- transaction, already past the watermark, so the conflict
			-- arm stores it as it stands.
			indexed_at    = excluded.indexed_at,
			enriched_at   = 0,
			-- missing_count reset is UNCONDITIONAL on confirm: the row
			-- being upserted is by definition "seen this scan", which
			-- is exactly the signal the counter measures. A pure
			-- mtime-equal path that skipped the reset would leak
			-- counter increments across scans and cause spurious
			-- deletes on long-stable libraries. See migration v5 doc.
			missing_count = 0,
			-- A re-scan whose artworkMBID CHANGED can't keep the old premium
			-- cover version (it belonged to the previous MBID) — clear it so the
			-- manifest falls back to the new MBID and the refetch re-establishes
			-- a version. An UNCHANGED MBID keeps it so a tag-only re-tag doesn't
			-- needlessly re-fetch. SET RHS reads the pre-update row (tracks.* is
			-- the OLD value); IS is NULL-safe.
			artwork_version = CASE
				WHEN json_extract(excluded.tags_json, '$.artworkMBID')
				     IS json_extract(tracks.tags_json, '$.artworkMBID')
				THEN tracks.artwork_version ELSE NULL
			END,
			-- v25 format-fact columns: stamped on every upsert so the
			-- coverage rollups' eligibility SQL never needs json_extract
			-- on the browse hot path. tags_json stays the read-truth for
			-- Go readers (see the v25 migration docblock).
			sample_rate     = excluded.sample_rate,
			bits_per_sample = excluded.bits_per_sample,
			is_dsd          = excluded.is_dsd,
			codec           = excluded.codec,
			-- v43 compression accelerator — same discipline as the four above.
			compression     = excluded.compression,
			-- extractor_version stamped on every upsert (constant per build).
			-- The excluded.extractor_version assignment is MANDATORY: without
			-- it a re-extracted (conflict) row keeps its stale stamp and would
			-- re-extract on every subsequent scan.
			-- The v31 dupe stamps (dupe_group_id / dupe_tier /
			-- dupe_suppressed) are deliberately NOT touched here: a
			-- changed file keeps its stamps until the next stamping pass
			-- re-evaluates, and a fresh INSERT defaults to served. That
			-- rests on every writer of these rows running a stamping pass
			-- in its own tail — true of Scan, and true of ScanSubtree only
			-- since the tail was added there; a future upsert caller
			-- without one leaves a retagged row suppressed with no served
			-- twin, which is NOT fail-open.
			extractor_version = excluded.extractor_version,
			-- audio_md5 is UNCONDITIONAL (excluded, empty included): this
			-- row runs because size or mtime CHANGED, so a stale hash
			-- would assert old audio for new bytes. The opposite
			-- semantics — keep-if-fresh-empty — belong to the
			-- version-stale stamp leg (StampExtractorVersionBatch), which
			-- only runs when the row was proved byte-identical.
			audio_md5 = excluded.audio_md5,
			-- v49: the folder art this extraction was given, by identity
			-- (Track.folderArtKey), which the skip gate compares.
			folder_art_key = excluded.folder_art_key,
			-- v50: whether this extraction refused the file
			-- (Track.extractRefused), unconditional like the version it
			-- rides with: a file that reads now clears it.
			extract_refused = excluded.extract_refused
			-- v53 first_indexed_at is insert-only. The conflict arm leaves it.
	`

// upsertAsTheRolledBackBinary writes tr's row as the bridge before B513 does:
// its upsert, its delta stamp, and the extractor version this binary stamps,
// which that binary stamps too (27 in both), so this binary's skip gate never
// re-reads the file. The folder-art key is the one the row holds, which that
// binary computes the same way.
func upsertAsTheRolledBackBinary(t *testing.T, s *Store, tr *Track) {
	t.Helper()
	raw, err := marshalForStorage(tr)
	if err != nil {
		t.Fatal(err)
	}
	rate, bits, isDSD, codec, compression := formatColumnBinds(tr)
	var artKey string
	if err := s.db.QueryRow(`SELECT COALESCE(folder_art_key, '') FROM tracks WHERE path = ?`, tr.Path).Scan(&artKey); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	var stamp int64
	if err := s.db.QueryRow(selectNextDeltaStampSQL, now).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(rolledBackUpsertSQL, tr.Path, tr.Size, tr.ModTime.UnixNano(), raw, stamp,
		rate, bits, isDSD, codec, ExtractorVersion, "", compression, artKey, 0, now); err != nil {
		t.Fatalf("the rolled-back binary's upsert of %s: %v", tr.Path, err)
	}
}

// rolledBackTrack is the row the rolled-back binary writes for the file at rel
// under root: the file's stat and the codec its extractor names.
func rolledBackTrack(t *testing.T, root, rel, codec string) *Track {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	pcm := false
	return &Track{Path: rel, Size: info.Size(), ModTime: info.ModTime(), Codec: codec, IsDSD: &pcm}
}

// maxIndexedAt is the newest delta cursor in s, the one a client holds once
// it has synced everything s served.
func maxIndexedAt(t *testing.T, s *Store) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT MAX(indexed_at) FROM tracks`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestARollbackLeavesNoKindItsHeaderDoesNotState runs a rollback through the
// rolled-back binary's own upsert. This binary records replaced.wav as float
// and finishes its backfill, so the clients have made their one full sync.
// The operator rolls back; replaced.wav is replaced by an integer render, and
// the older binary's scan rewrites its row, keeping the kind; it also indexes
// a new FLAC and a new float WAV, whose kinds it leaves NULL; the clients sync
// all of it. This binary is reinstalled: it opens the store, runs its boot
// pass and a scan. Each row then carries the kind its header states, and each
// row whose kind no client holds yet has advanced its delta cursor past the
// clients', so their next delta carries it. A row the rollback left alone
// keeps its cursor.
func TestARollbackLeavesNoKindItsHeaderDoesNotState(t *testing.T) {
	ctx := context.Background()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	root := t.TempDir()
	replacedRel := writeAlone(t, root, "replaced.wav", fx("wav/float.wav"))
	keptRel := writeAlone(t, root, "kept.flac", flacBytes(t, 44100, 16))
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "this binary's scan")
	resolve := bridgefs.New([]string{root}).ResolveChecked
	if _, err := store.BackfillSampleFormats(ctx, resolve); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{replacedRel: SampleFormatFloat, keptRel: SampleFormatInteger})
	keptCursor := trackIndexedAt(t, store, keptRel)

	// The rollback: replaced.wav becomes an integer render, a FLAC and a
	// float WAV arrive, and the older binary's scan writes all three.
	abs := filepath.Join(root, filepath.FromSlash(replacedRel))
	before, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureBytes(t, abs, fx("wav/af_i16.wav"))
	later := before.ModTime().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	addedFLAC := writeAlone(t, root, "added.flac", flacBytes(t, 48000, 24))
	addedWAV := writeAlone(t, root, "added.wav", fx("wav/af_f64.wav"))
	for _, w := range []struct{ rel, codec string }{{replacedRel, "WAV"}, {addedFLAC, "FLAC"}, {addedWAV, "WAV"}} {
		upsertAsTheRolledBackBinary(t, store, rolledBackTrack(t, root, w.rel, w.codec))
	}
	requireSampleFormats(t, store, map[string]string{replacedRel: SampleFormatFloat, addedFLAC: "NULL", addedWAV: "NULL"})
	cursor := maxIndexedAt(t, store)

	// This binary again: the open, the boot pass, and a scan.
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BackfillSampleFormats(ctx, resolve); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "the reinstalled binary's scan")

	want := map[string]string{
		replacedRel: SampleFormatInteger,
		addedFLAC:   SampleFormatInteger,
		addedWAV:    SampleFormatFloat,
		keptRel:     SampleFormatInteger,
	}
	requireSampleFormats(t, store, want)
	for _, rel := range []string{replacedRel, addedFLAC, addedWAV} {
		if got := trackIndexedAt(t, store, rel); got <= cursor {
			t.Errorf("%s got its kind after the clients' full sync and its cursor is %d, not past the clients' %d: no delta carries it", rel, got, cursor)
		}
	}
	if got := trackIndexedAt(t, store, keptRel); got != keptCursor {
		t.Errorf("%s: the rollback left it alone and its cursor moved %d -> %d", keptRel, keptCursor, got)
	}
	served := servedTracksByPath(t, store, root)
	for rel, w := range want {
		if got := served[rel]["sampleFormat"]; got != w {
			t.Errorf("the manifest serves %s with sampleFormat %#v, want %q", rel, got, w)
		}
	}
}
