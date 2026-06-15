package manifest

import (
	"context"
	"fmt"
	"path"
	"testing"
	"time"
)

// TestListChildFoldersPageCursorAdvance pins the cursor contract:
// passing the last seen path as `after` advances correctly to the
// next page. Builds a synthetic 12-folder set and pages at limit=5.
func TestListChildFoldersPageCursorAdvance(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Seed 12 folders + 1 track each so the rollup subqueries have
	// something to count and the folder is materialised in the
	// `folders` table via UpsertFolder.
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("Album%02d", i)
		if err := s.UpsertFolder(ctx, &Folder{Path: name, ModTime: now}); err != nil {
			t.Fatal(err)
		}
		tk := Track{
			Path:    path.Join(name, "01.flac"),
			Size:    100,
			ModTime: now,
			Title:   "T",
		}
		if err := s.UpsertTrack(ctx, &tk); err != nil {
			t.Fatal(err)
		}
	}

	// Page 1: after="" limit=5 → Album00..Album04.
	page1, err := s.ListChildFoldersPage(ctx, "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 5 {
		t.Fatalf("page 1: got %d rows, want 5", len(page1))
	}
	if page1[0].Path != "Album00" || page1[4].Path != "Album04" {
		t.Errorf("page 1 boundaries: %q..%q, want Album00..Album04",
			page1[0].Path, page1[4].Path)
	}

	// Page 2: after=Album04 → Album05..Album09.
	page2, err := s.ListChildFoldersPage(ctx, "", page1[len(page1)-1].Path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 5 {
		t.Fatalf("page 2: got %d rows, want 5", len(page2))
	}
	if page2[0].Path != "Album05" {
		t.Errorf("page 2 first row: %q, want Album05", page2[0].Path)
	}

	// Page 3: after=Album09 → Album10, Album11 (only 2 remaining).
	page3, err := s.ListChildFoldersPage(ctx, "", page2[len(page2)-1].Path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page3) != 2 {
		t.Fatalf("page 3: got %d rows, want 2", len(page3))
	}

	// Page 4: after=Album11 → empty (all consumed).
	page4, err := s.ListChildFoldersPage(ctx, "", page3[len(page3)-1].Path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page4) != 0 {
		t.Errorf("page 4 (past end): got %d rows, want 0", len(page4))
	}

	// Rollup must still be intact on a paginated page — same shape
	// the unpaginated path returns. Each folder has exactly 1 track.
	for _, row := range page1 {
		if row.TrackCount != 1 {
			t.Errorf("rollup TrackCount for %q: got %d, want 1",
				row.Path, row.TrackCount)
		}
	}
}

// TestListChildTracksPageCursorAdvance mirrors the folder test
// against ListChildTracksPage with tracks directly under root.
func TestListChildTracksPageCursorAdvance(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 8; i++ {
		// Root-level tracks (no slash in path) so they match the
		// empty-parent track-list query branch.
		name := fmt.Sprintf("track%02d.flac", i)
		tk := Track{Path: name, Size: 100, ModTime: now, Title: "T"}
		if err := s.UpsertTrack(ctx, &tk); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.ListChildTracksPage(ctx, "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 {
		t.Fatalf("page 1: got %d rows, want 3", len(page1))
	}
	page2, err := s.ListChildTracksPage(ctx, "", page1[2].Path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 3 {
		t.Fatalf("page 2: got %d rows, want 3", len(page2))
	}
	if page2[0].Path == page1[0].Path {
		t.Errorf("page 2 first row should not duplicate page 1: %q", page2[0].Path)
	}
}

// TestCountChildFolders confirms the count surface returns the
// immediate-child count without invoking the rollup subqueries that
// ListChildFolders runs per row. The empty-parent (root) count derives
// the top-level filesystem folders from track paths (see
// topLevelFSFolderSource), so each top-level folder is seeded WITH a
// track under it.
func TestCountChildFolders(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("Album%02d", i)
		if err := s.UpsertFolder(ctx, &Folder{Path: name, ModTime: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertTrack(ctx, &Track{
			Path: path.Join(name, "01.flac"), Size: 1, ModTime: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CountChildFolders(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("CountChildFolders root: got %d, want 7", n)
	}
}

// TestCountChildTracks pairs with the folder count.
func TestCountChildTracks(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Mix root-level + nested tracks to confirm the count is
	// immediate-children only (nested under a subfolder don't count
	// for an empty-parent query).
	for i := 0; i < 4; i++ {
		if err := s.UpsertTrack(ctx, &Track{
			Path: fmt.Sprintf("root%d.flac", i), Size: 1, ModTime: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := s.UpsertTrack(ctx, &Track{
			Path: path.Join("sub", fmt.Sprintf("n%d.flac", i)), Size: 1, ModTime: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rootCount, err := s.CountChildTracks(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if rootCount != 4 {
		t.Errorf("root track count: got %d, want 4 (nested 'sub/*' must not count)", rootCount)
	}
	subCount, err := s.CountChildTracks(ctx, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if subCount != 3 {
		t.Errorf("'sub' track count: got %d, want 3", subCount)
	}
}
