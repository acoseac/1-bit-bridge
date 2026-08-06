package manifest

import (
	"context"
	"slices"
	"testing"
	"time"
	"unicode/utf8"
)

// illFormedPath is a real Linux filename shape: 0xE9 is Latin-1 'é',
// which a pre-UTF-8 locale wrote straight into the directory entry.
// Nothing between filepath.WalkDir and UpsertTrackBatch validates it and
// SQLite does not validate TEXT, so the byte sequence reaches the `path`
// PRIMARY KEY verbatim.
//
// Some filesystems refuse to CREATE such a name, so these tests exercise
// the store layer directly rather than staging a real walk.
const (
	illFormedPath   = "Alb\xe9um/track.flac"
	illFormedFolder = "Alb\xe9um"
)

func requireIllFormed(t *testing.T, s string) {
	t.Helper()
	if utf8.ValidString(s) {
		t.Fatalf("fixture %q is valid UTF-8 — this test proves nothing", s)
	}
}

func rowCount(t *testing.T, s *Store, query, path string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, path).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", path, err)
	}
	return n
}

func trackRowPresent(t *testing.T, s *Store, path string) bool {
	t.Helper()
	return rowCount(t, s, `SELECT COUNT(*) FROM tracks WHERE path = ?`, path) > 0
}

func trackMissingCount(t *testing.T, s *Store, path string) int {
	t.Helper()
	return rowCount(t, s, `SELECT missing_count FROM tracks WHERE path = ?`, path)
}

// A track whose path is not valid UTF-8 must still be reaped when it
// reaches the threshold.
//
// The scoped DELETE carries the missing set as a JSON array consumed by
// json_each. encoding/json replaces ill-formed UTF-8 with U+FFFD and
// returns NO error, so such a path re-emerges from json_each as a
// different string and `path IN (…)` can never match the row.
//
// The failure was silent in both directions: the per-row increment binds
// each path directly, so missing_count climbed normally and the
// "missing path did not match any row" diagnostic never fired — the row
// simply stayed in /v1/manifest forever, permanently inflating
// PendingDeletions. The pre-scoping bare `missing_count >= ?` predicate
// did reap it, so this is a regression of that change.
func TestThresholdDeleteReapsIllFormedUTF8Path(t *testing.T) {
	requireIllFormed(t, illFormedPath)

	s := openTestStore(t)
	ctx := context.Background()

	const wellFormed = "Music/Artist/Album/ok.flac"
	for _, p := range []string{illFormedPath, wellFormed} {
		if err := s.UpsertTrack(ctx, &Track{
			Path: p, Size: 1, ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	// Pass 1: both observed missing, below the threshold.
	if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(
		ctx, []string{illFormedPath, wellFormed}, 2); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	// Isolates which half is broken: the increment binds the path
	// directly, so it matches even when the JSON round-trip cannot.
	if got := trackMissingCount(t, s, illFormedPath); got != 1 {
		t.Fatalf("missing_count for the ill-formed path = %d, want 1 — the "+
			"per-row increment failed to match, so the DELETE is not the "+
			"only thing at fault here", got)
	}

	// Pass 2: both reach the threshold and must be reaped together.
	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(
		ctx, []string{illFormedPath, wellFormed}, 2)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if trackRowPresent(t, s, wellFormed) {
		t.Error("the well-formed row should have been reaped at the threshold")
	}
	if trackRowPresent(t, s, illFormedPath) {
		t.Fatal("a track whose path is not valid UTF-8 was never reaped: " +
			"json.Marshal substitutes U+FFFD without erroring, so the path " +
			"json_each yields is not the path stored in the PRIMARY KEY and " +
			"`path IN (…)` cannot match it — a phantom row in /v1/manifest " +
			"forever, with no log line")
	}
}

// The routed anti-join is layer 2 of the PR #370 two-layer guard and no
// caller may threshold-delete a routed row regardless of accumulated
// counters. The ill-formed-path fallback is a second DELETE statement,
// so it needs the guard restated — a fallback that only carried the
// threshold would be a hole in an invariant the batch form upholds.
func TestThresholdDeleteSparesRoutedIllFormedUTF8Path(t *testing.T) {
	requireIllFormed(t, illFormedPath)

	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertTrack(ctx, &Track{
		Path: illFormedPath, Size: 1, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: illFormedPath,
		ServerUDN:  "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
		ObjectID:   "64$0$0$0",
		ResURL:     "http://192.168.0.62:8200/MediaItems/1.flac",
		LastSeenAt: time.Unix(1_700_000_000, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	seedMissingCount(t, s, illFormedPath, 5)

	deleted, err := s.IncrementMissingTracksAndDeleteAtThreshold(
		ctx, []string{illFormedPath}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 — a routed row must never be "+
			"threshold-deleted", deleted)
	}
	if !trackRowPresent(t, s, illFormedPath) {
		t.Fatal("a UPnP-routed row was threshold-deleted through the " +
			"ill-formed-path fallback; its lifecycle belongs solely to the " +
			"ingest's last_seen_at reap")
	}
}

// The folders twin marshals its missing set the same way, so a directory
// name that is not valid UTF-8 lingers in the listing surface for the
// same reason.
func TestFolderThresholdDeleteReapsIllFormedUTF8Path(t *testing.T) {
	requireIllFormed(t, illFormedFolder)

	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertFolder(ctx, &Folder{
		Path: illFormedFolder, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IncrementMissingFoldersAndDeleteAtThreshold(
		ctx, []string{illFormedFolder}, 2); err != nil {
		t.Fatalf("pass 1: %v", err)
	}

	deleted, err := s.IncrementMissingFoldersAndDeleteAtThreshold(
		ctx, []string{illFormedFolder}, 2)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if rowCount(t, s, `SELECT COUNT(*) FROM folders WHERE path = ?`, illFormedFolder) > 0 {
		t.Fatal("a folder whose path is not valid UTF-8 was never reaped — " +
			"same json_each mismatch as the tracks twin")
	}
}

func TestSplitIllFormedUTF8Paths(t *testing.T) {
	t.Run("all well-formed returns the input untouched", func(t *testing.T) {
		in := []string{"a.flac", "b/c.flac"}
		valid, illFormed := splitIllFormedUTF8Paths(in)
		if illFormed != nil {
			t.Errorf("illFormed = %q, want nil", illFormed)
		}
		if len(valid) != len(in) || &valid[0] != &in[0] {
			t.Error("the common case must return the input slice as-is, no copy")
		}
	})

	t.Run("partitions in order and never mutates the caller's slice", func(t *testing.T) {
		in := []string{"a.flac", "b\xe9.flac", "c.flac", "d\xff.flac"}
		before := append([]string(nil), in...)

		valid, illFormed := splitIllFormedUTF8Paths(in)

		if want := []string{"a.flac", "c.flac"}; !slices.Equal(valid, want) {
			t.Errorf("valid = %q, want %q", valid, want)
		}
		if want := []string{"b\xe9.flac", "d\xff.flac"}; !slices.Equal(illFormed, want) {
			t.Errorf("illFormed = %q, want %q", illFormed, want)
		}
		// A `paths[:0]` style in-place filter would silently clobber the
		// scanner's own slice, which it still holds and logs from.
		if !slices.Equal(in, before) {
			t.Errorf("input was mutated: %q, want %q", in, before)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		valid, illFormed := splitIllFormedUTF8Paths(nil)
		if len(valid) != 0 || len(illFormed) != 0 {
			t.Errorf("valid = %q, illFormed = %q, want both empty", valid, illFormed)
		}
	})
}
