package manifest

import (
	"context"
	"testing"
	"time"
)

// seedAnalysisRow creates a track and its track_analysis row (the FK
// requires the parent) so a migration test has rows to backfill.
func seedAnalysisRow(t *testing.T, s *Store, path string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, &Track{
		Path: path, Size: 1, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("seed track %q: %v", path, err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath:    path,
		SourceMTimeNS: 1,
		SourceSize:    1,
		SchemaVersion: "wf4",
		CreatedAt:     time.Unix(1_700_000_000, 0).Unix(),
	}); err != nil {
		t.Fatalf("seed analysis %q: %v", path, err)
	}
}

func audioMD5Attempts(t *testing.T, s *Store, path string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT audio_md5_attempts FROM track_analysis WHERE source_path = ?`, path,
	).Scan(&n); err != nil {
		t.Fatalf("read audio_md5_attempts for %q: %v", path, err)
	}
	return n
}

// Migration v30's one-time backfill must run on EVERY attempt at the
// migration, not only on the attempt that adds the column.
//
// migrate() runs `sql` → `post` → the user_version bump entirely
// OUTSIDE a transaction, and v30's ALTER autocommits separately from
// its backfill UPDATE. So a backfill that fails on its own — SQLITE_BUSY
// from a second process in OpenStore (a `bridge duplicates` /
// `bridge status` run against a live DB), disk full, or the process
// being killed between the two statements — leaves the column present
// with the version still at 29. With the backfill inside the
// column-exists guard, the next start found the column, returned nil
// without touching a row, and stamped 30 over unbackfilled data.
//
// That is not cosmetic. Every non-FLAC track_analysis row then sits at
// 0 with an empty audio_md5_state, which is exactly what
// AnalysisRow.WantsAudioMD5Retry reads as "worth another attempt", so
// the analysis sweeper's skip gate re-enqueues the entire non-FLAC
// library for a full decode — the treadmill the migration's own
// docblock says this backfill exists to prevent.
//
// A test that only migrates a fresh DB passes both before and after the
// fix, so this drives the re-run explicitly: column present, data
// unbackfilled, version rewound to 29.
func TestMigrationV30BackfillSurvivesARerun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const (
		flacPath = "Music/Artist/Album/01 lossless.flac"
		mp3Path  = "Music/Artist/Album/02 lossy.mp3"
		m4aPath  = "Music/Artist/Album/03 lossy.m4a"
	)
	for _, p := range []string{flacPath, mp3Path, m4aPath} {
		seedAnalysisRow(t, s, p)
	}

	// The state a failed backfill leaves behind: the ALTER committed
	// (the column is there, at its DEFAULT 0) but no row was written.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE track_analysis SET audio_md5_attempts = 0`); err != nil {
		t.Fatalf("simulate unbackfilled rows: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 29`); err != nil {
		t.Fatalf("rewind user_version: %v", err)
	}

	// The operator restarts the bridge. This is the retry.
	if err := s.migrate(); err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}

	for _, p := range []string{mp3Path, m4aPath} {
		if got := audioMD5Attempts(t, s, p); got != AudioMD5MaxAttempts {
			t.Errorf("audio_md5_attempts for the non-FLAC row %q = %d, want %d — "+
				"the v30 backfill was skipped on the re-run because the "+
				"column-exists guard wrapped the data step as well as the "+
				"ALTER, so every non-FLAC row reads as retry-worthy and the "+
				"analysis sweeper re-decodes the whole library once",
				p, got, AudioMD5MaxAttempts)
		}
	}

	// FLAC rows are the population the retry budget is actually for:
	// they must keep their 0 and get their bounded round.
	if got := audioMD5Attempts(t, s, flacPath); got != 0 {
		t.Errorf("audio_md5_attempts for the FLAC row = %d, want 0 — the "+
			"backfill must cap only rows the verification pass can never "+
			"run on", got)
	}

	// The ladder still reaches head, so the retry is a real recovery
	// rather than a permanently-stuck start.
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := migrations[len(migrations)-1].version; version != want {
		t.Errorf("user_version after the re-run = %d, want %d", version, want)
	}
}
