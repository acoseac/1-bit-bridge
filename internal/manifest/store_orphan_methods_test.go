package manifest

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestStoreFolderPaths pins the simple-shape contract: returns every
// folder path, sorted. Mirror of TrackPaths' existing test coverage.
func TestStoreFolderPaths(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC()
	for _, p := range []string{"b/album", "a/album", "a/album/disc1"} {
		if err := s.UpsertFolder(context.Background(), &Folder{Path: p, ModTime: now}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.FolderPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/album", "a/album/disc1", "b/album"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FolderPaths = %v, want %v", got, want)
	}
}

// TestStoreDeleteFolder pins the idempotent-delete contract. Missing
// rows are not an error so the scanner's deletion-pass loop doesn't
// fail mid-iteration on a row a sibling writer (admin "remove root")
// already cleaned up.
func TestStoreDeleteFolder(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC()
	if err := s.UpsertFolder(context.Background(), &Folder{Path: "album", ModTime: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(context.Background(), "album"); err != nil {
		t.Fatalf("DeleteFolder existing: %v", err)
	}
	if got, _ := s.FolderPaths(context.Background()); len(got) != 0 {
		t.Errorf("post-delete FolderPaths = %v, want empty", got)
	}
	// Idempotent: deleting again is a no-op, not an error.
	if err := s.DeleteFolder(context.Background(), "album"); err != nil {
		t.Errorf("DeleteFolder missing returned error: %v", err)
	}
	// Likewise for a path that never existed.
	if err := s.DeleteFolder(context.Background(), "never-existed"); err != nil {
		t.Errorf("DeleteFolder unknown returned error: %v", err)
	}
}

// TestStoreTrackPathsUnder covers the LIKE-prefix scope plus the three
// normalizations (empty, ".", "<base>/.") plus LIKE-escape correctness
// for directories whose names contain SQL-special characters.
func TestStoreTrackPathsUnder(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC()
	for _, p := range []string{
		"a/album1/song.flac",
		"a/album1/disc1/song.flac",
		"a/album2/song.flac",
		"b/album/song.flac",
		// LIKE-escape edge: directory whose name contains "%". Without
		// the ESCAPE clause, "100%" would be interpreted as a wildcard
		// and the under-"100%" query would also match siblings like
		// "100abc". The escape-handling here pins the contract.
		"100%/x/song.flac",
		"100abc/x/song.flac",
	} {
		// Ensure each parent dir's folder row exists; harmless for the
		// per-track query but mirrors the real on-disk shape.
		if err := s.UpsertFolder(context.Background(), &Folder{Path: filepath.Dir(p), ModTime: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertTrack(context.Background(), &Track{Path: p, ModTime: now}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		dir  string
		want []string
	}{
		{"empty returns all", "", []string{
			"100%/x/song.flac", "100abc/x/song.flac",
			"a/album1/disc1/song.flac", "a/album1/song.flac",
			"a/album2/song.flac", "b/album/song.flac",
		}},
		{"dot returns all", ".", []string{
			"100%/x/song.flac", "100abc/x/song.flac",
			"a/album1/disc1/song.flac", "a/album1/song.flac",
			"a/album2/song.flac", "b/album/song.flac",
		}},
		{"single dir scope", "a/album1", []string{
			"a/album1/disc1/song.flac", "a/album1/song.flac",
		}},
		{"deeper scope", "a/album1/disc1", []string{
			"a/album1/disc1/song.flac",
		}},
		{"no descendants", "a/album2/disc99", nil},
		// LIKE-escape: querying under "100%" must NOT match "100abc".
		{"like-escape", "100%", []string{"100%/x/song.flac"}},
		// Multi-root whole-root sentinel: relPath returns "<base>/."
		// for the root itself in multi-root mode.
		{"multi-root sentinel", "a/.", []string{
			"a/album1/disc1/song.flac",
			"a/album1/song.flac",
			"a/album2/song.flac",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.TrackPathsUnder(context.Background(), tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TrackPathsUnder(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

// TestStoreFolderPathsUnder covers the same scopes as the track variant
// PLUS the contract that relDir's own folder row is included when
// present. This is the "rename-in-place" fix path: ScanSubtree on the
// parent must reap the row for the renamed-away child directory.
func TestStoreFolderPathsUnder(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC()
	// Seed a mix of single-root form rows ("b", "b/album") and multi-root
	// form rows ("a/." for the rootBase, plus its descendants). In a real
	// multi-root deployment the walker upserts rootBase as "a/." (not bare
	// "a") via relPath(root, root, true) — see scanner.go's walkRoot.
	for _, p := range []string{
		"a/.",
		"a/album1",
		"a/album1/disc1",
		"a/album2",
		"b",
		"b/album",
		"100%",
		"100%/x",
		"100abc",
		"100abc/x",
	} {
		if err := s.UpsertFolder(context.Background(), &Folder{Path: p, ModTime: now}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		dir  string
		want []string
	}{
		{"single dir scope includes self", "a/album1", []string{
			"a/album1", "a/album1/disc1",
		}},
		{"leaf scope is just self", "a/album1/disc1", []string{
			"a/album1/disc1",
		}},
		{"like-escape", "100%", []string{"100%", "100%/x"}},
		// Whole-library returns every folder. Note "a/." sorts before
		// "a/album1" because '.' < 'a' in ASCII.
		{"dot returns all", ".", []string{
			"100%", "100%/x", "100abc", "100abc/x",
			"a/.", "a/album1", "a/album1/disc1", "a/album2", "b", "b/album",
		}},
		// Multi-root whole-root sentinel: relPath returns "<base>/."
		// for the root itself in multi-root mode. Mirrors the
		// TestStoreTrackPathsUnder coverage so a regression in
		// FolderPathsUnder's same-shape branch is caught here too.
		// Includes the rootBase row itself ("a/.") via the exact-match
		// arm of the predicate, plus every descendant via the LIKE arm.
		{"multi-root sentinel", "a/.", []string{
			"a/.", "a/album1", "a/album1/disc1", "a/album2",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FolderPathsUnder(context.Background(), tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FolderPathsUnder(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}
