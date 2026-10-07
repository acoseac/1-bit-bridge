package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	reproT0 = time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	reproT1 = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	reproT2 = time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	reproT3 = time.Date(2022, 6, 7, 8, 9, 10, 0, time.UTC)
)

func clockAt(s *Store, at time.Time) { s.now = func() time.Time { return at } }

func listedName(t *testing.T, dir, want string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == want {
			return e.Name()
		}
	}
	return ""
}

func firstIndexedTime(t *testing.T, s *Store, path string) time.Time {
	t.Helper()
	got, err := s.GetTrack(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FirstIndexedAt == nil {
		t.Fatalf("%s has no first-indexed date", path)
	}
	return got.FirstIndexedAt.UTC()
}

// scanSeededAlbum indexes Artist/Album/song.flac at the given clock and
// returns the root the scan walked.
func scanSeededAlbum(t *testing.T, at time.Time) (string, *Store, *Scanner) {
	t.Helper()
	root := t.TempDir()
	seedTrackDirs(t, filepath.Join(root, "Artist", "Album"))
	store, sc := newScanFixture(t, root)
	clockAt(store, at)
	scanOnce(t, sc, "first")
	return root, store, sc
}

// recordAndWipe saves the dates for a root flip and wipes the filesystem
// rows, the two steps that sit next to each other before a rescan.
func recordAndWipe(t *testing.T, store *Store, fromMultiRoot bool, base string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.RecordFirstIndexedCarry(ctx, fromMultiRoot, base); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
}

func renameAlbumCase(t *testing.T, root string) {
	t.Helper()
	if err := os.Rename(filepath.Join(root, "Artist", "Album"), filepath.Join(root, "Artist", "album")); err != nil {
		t.Fatal(err)
	}
	if listedName(t, filepath.Join(root, "Artist"), "album") != "album" {
		t.Skip("this volume did not store the renamed case")
	}
}

func carryCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func explainPlan(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " | ")
}
