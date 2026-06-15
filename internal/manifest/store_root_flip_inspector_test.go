package manifest

import (
	"context"
	"testing"
	"time"
)

// seedFSTrack inserts a plain filesystem-sourced track (no UPnP routing row).
func seedFSTrack(t *testing.T, s *Store, p string) {
	t.Helper()
	tr := &Track{Path: p, Size: 100, ModTime: time.Unix(0, 0).UTC()}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("seed fs track %q: %v", p, err)
	}
}

// seedRoutedTrack (track + upnp_track_routing row) lives in
// scanner_upnp_rows_test.go — reused here.

// TestWipeFilesystemTracks_SparesUPnPRoutedRows pins the library-root
// add/remove storage-form-flip fix: the wipe clears filesystem tracks +
// all folder rows but MUST NOT touch UPnP-routed tracks or their routing
// rows. Pre-fix the flip called WipeAllTracks, which CASCADE-destroyed the
// entire upstream library + its cached enrichment on every FS root toggle.
func TestWipeFilesystemTracks_SparesUPnPRoutedRows(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedFSTrack(t, s, "music/Artist/a.flac")
	seedFSTrack(t, s, "medialibtest/Band/b.flac")
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO folders(path, mtime_ns) VALUES ('medialibtest/Band', 0)`); err != nil {
		t.Fatalf("seed folder: %v", err)
	}
	seedRoutedTrack(t, s, "2go/Server/c.flac")

	if got, _ := s.CountTracks(ctx); got != 3 {
		t.Fatalf("precondition: CountTracks = %d, want 3", got)
	}

	if err := s.WipeFilesystemTracks(ctx); err != nil {
		t.Fatalf("WipeFilesystemTracks: %v", err)
	}

	// Only the routed row survives.
	if got, _ := s.CountTracks(ctx); got != 1 {
		t.Errorf("after wipe: CountTracks = %d, want 1 (routed row only)", got)
	}
	if tr, err := s.GetTrack(ctx, "2go/Server/c.flac"); err != nil || tr == nil {
		t.Errorf("routed track was wiped (err=%v, tr=%v) — must be spared", err, tr)
	}
	if r, err := s.GetUPnPRouting(ctx, "2go/Server/c.flac"); err != nil || r == nil {
		t.Errorf("routing row was wiped (err=%v) — must be spared", err)
	}
	if tr, _ := s.GetTrack(ctx, "music/Artist/a.flac"); tr != nil {
		t.Error("filesystem track music/Artist/a.flac survived the wipe")
	}
	// Folder rows are filesystem-only and flip form with the root count, so
	// all are cleared (rebuilt by the rescan).
	var nFolders int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders`).Scan(&nFolders); err != nil {
		t.Fatalf("count folders: %v", err)
	}
	if nFolders != 0 {
		t.Errorf("folders not wiped: %d rows remain", nFolders)
	}
}

// TestListChildFoldersRoot_MultiRootDerivesFSRootsHidesUPnP pins the
// inspector root-browse fix: in multi-root mode the top level must list the
// filesystem root basenames (derived from track paths, since the scanner
// never inserts a bare "<basename>" folder row) and MUST NOT surface the
// UPnP-routed subtree. Pre-fix the folders-table query found zero rows and
// the inspector root rendered empty.
func TestListChildFoldersRoot_MultiRootDerivesFSRootsHidesUPnP(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedFSTrack(t, s, "music/Pink Floyd/Money.flac")
	seedFSTrack(t, s, "medialibtest/Diana Krall/01.flac")
	seedFSTrack(t, s, "medialibtest/Diana Krall/02.flac")
	seedRoutedTrack(t, s, "2go/Server/x.flac")
	seedRoutedTrack(t, s, "2go/Server/y.flac")

	folders, err := s.ListChildFoldersPage(ctx, "", "", 500)
	if err != nil {
		t.Fatalf("ListChildFoldersPage root: %v", err)
	}
	got := make([]string, len(folders))
	counts := map[string]int{}
	for i, f := range folders {
		got[i] = f.Path
		counts[f.Path] = f.TrackCount
	}
	want := []string{"medialibtest", "music"} // ORDER BY path ASC
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("root folders = %v, want %v (2go must be hidden)", got, want)
	}
	if counts["medialibtest"] != 2 {
		t.Errorf("medialibtest rollup TrackCount = %d, want 2", counts["medialibtest"])
	}
	if counts["music"] != 1 {
		t.Errorf("music rollup TrackCount = %d, want 1", counts["music"])
	}

	if n, err := s.CountChildFolders(ctx, ""); err != nil || n != 2 {
		t.Errorf("CountChildFolders(root) = %d (err=%v), want 2", n, err)
	}
}

// TestListChildFoldersRoot_SingleRootDerivesAlbumFolders is the regression
// guard that the tracks-derived root still works in single-root mode, where
// paths carry no basename prefix (top level = artist folders).
func TestListChildFoldersRoot_SingleRootDerivesAlbumFolders(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedFSTrack(t, s, "Abdullah Ibrahim/The Balance/01.flac")
	seedFSTrack(t, s, "Diana Krall/Live in Paris/01.flac")
	seedFSTrack(t, s, "Diana Krall/Live in Paris/02.flac")

	folders, err := s.ListChildFoldersPage(ctx, "", "", 500)
	if err != nil {
		t.Fatalf("ListChildFoldersPage root: %v", err)
	}
	got := make([]string, len(folders))
	for i, f := range folders {
		got[i] = f.Path
	}
	want := []string{"Abdullah Ibrahim", "Diana Krall"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("single-root top-level folders = %v, want %v", got, want)
	}
}
