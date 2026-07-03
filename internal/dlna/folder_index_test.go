package dlna

import (
	"strconv"
	"strings"
	"testing"
)

// folderTrack returns a TrackInfo synthetic with the given relative
// path mapped under `/library/`. Convenience for the folder-index
// tests so each fixture line stays readable.
//
// Both `RelativePath` AND `AbsolutePath` are populated — the former
// is the preferred source for `BuildFolderIndex`, the latter is set
// for back-compat with helpers that read it (e.g. the
// sort-stable-by-AbsolutePath ordering inside the child-track
// emission path).
func folderTrack(id, relPath string) TrackInfo {
	return TrackInfo{
		TrackID:       id,
		AbsolutePath:  "/library/" + relPath,
		RelativePath:  relPath,
		Title:         id,
		FileExtension: ".flac",
	}
}

// --- FolderObjectID --------------------------------------------------

func Test_FolderObjectID_EmptyReturnsFoldersRoot(t *testing.T) {
	got := FolderObjectID("")
	if got != foldersRootObjectID {
		t.Fatalf("FolderObjectID(\"\") = %q, want %q", got, foldersRootObjectID)
	}
}

func Test_FolderObjectID_StableAcrossCalls(t *testing.T) {
	a := FolderObjectID("Artist A/Album 1")
	b := FolderObjectID("Artist A/Album 1")
	if a != b {
		t.Fatalf("FolderObjectID not stable: %q vs %q", a, b)
	}
}

func Test_FolderObjectID_DifferentPathsProduceDifferentIDs(t *testing.T) {
	a := FolderObjectID("Artist A/Album 1")
	b := FolderObjectID("Artist A/Album 2")
	if a == b {
		t.Fatalf("FolderObjectID collision: %q == %q for distinct paths", a, b)
	}
}

func Test_FolderObjectID_IsPurelyNumeric(t *testing.T) {
	// Per PR #315 invariant: mconnect/Cling parses ObjectIDs as
	// integers. Folder IDs MUST be parsable as uint64 — would-be
	// non-numeric output (hex-prefixed, etc.) regresses against the
	// drill-down invariant.
	for _, relPath := range []string{
		"Artist A",
		"Artist A/Album 1",
		"Deeply/Nested/Path/Album",
		"Artist with spaces & symbols!",
	} {
		got := FolderObjectID(relPath)
		if _, err := strconv.ParseUint(got, 10, 64); err != nil {
			t.Errorf("FolderObjectID(%q) = %q is not a uint64: %v", relPath, got, err)
		}
	}
}

func Test_FolderObjectID_AvoidsReservedRange(t *testing.T) {
	// We can't easily force a SHA-256 to land below the reserved
	// floor, but we can confirm no folder ID we produce in practice
	// collides with the reserved static IDs (0, 1, 2).
	reserved := map[string]bool{"0": true, "1": true, "2": true}
	for _, relPath := range []string{
		"a", "b", "c", "Artist A", "Artist A/Album 1",
		"Deeply/Nested/Path",
	} {
		got := FolderObjectID(relPath)
		if reserved[got] {
			t.Errorf("FolderObjectID(%q) = %q collides with reserved ID", relPath, got)
		}
	}
}

// --- BuildFolderIndex ------------------------------------------------

func Test_BuildFolderIndex_EmptyTrackList(t *testing.T) {
	idx := BuildFolderIndex(nil)
	if idx == nil {
		t.Fatal("BuildFolderIndex returned nil; want empty index")
	}
	if len(idx.Folders) != 0 {
		t.Errorf("Folders not empty: %v", idx.Folders)
	}
	if len(idx.TopLevelFolderIDs) != 0 {
		t.Errorf("TopLevelFolderIDs not empty: %v", idx.TopLevelFolderIDs)
	}
	if idx.TrackCount() != 0 {
		t.Errorf("TrackCount = %d, want 0", idx.TrackCount())
	}
}

func Test_BuildFolderIndex_SingleTopLevelFolder(t *testing.T) {
	idx := BuildFolderIndex([]TrackInfo{
		folderTrack("t1", "Artist A/track 01.flac"),
		folderTrack("t2", "Artist A/track 02.flac"),
	})

	if got := len(idx.TopLevelFolderIDs); got != 1 {
		t.Fatalf("TopLevelFolderIDs count = %d, want 1; ids=%v", got, idx.TopLevelFolderIDs)
	}
	topID := idx.TopLevelFolderIDs[0]
	node, ok := idx.Folders[topID]
	if !ok {
		t.Fatalf("top-level folder not found by ID %q", topID)
	}
	if node.Name != "Artist A" {
		t.Errorf("top-level folder Name = %q, want %q", node.Name, "Artist A")
	}
	if node.RelPath != "Artist A" {
		t.Errorf("top-level folder RelPath = %q, want %q", node.RelPath, "Artist A")
	}
	if node.ParentID != foldersRootObjectID {
		t.Errorf("top-level folder ParentID = %q, want %q", node.ParentID, foldersRootObjectID)
	}
	if got := len(node.ChildTrackIDs); got != 2 {
		t.Errorf("top-level folder ChildTrackIDs count = %d, want 2", got)
	}
}

func Test_BuildFolderIndex_NestedFolders(t *testing.T) {
	idx := BuildFolderIndex([]TrackInfo{
		folderTrack("t1", "Artist A/Album X/track 01.flac"),
		folderTrack("t2", "Artist A/Album X/track 02.flac"),
		folderTrack("t3", "Artist A/Album Y/track 01.flac"),
		folderTrack("t4", "Artist B/Album Z/track 01.flac"),
	})

	// Two top-level folders.
	if got := len(idx.TopLevelFolderIDs); got != 2 {
		t.Fatalf("TopLevelFolderIDs count = %d, want 2; ids=%v", got, idx.TopLevelFolderIDs)
	}

	// Find Artist A by walking the index.
	var artistA FolderNode
	for _, n := range idx.Folders {
		if n.RelPath == "Artist A" {
			artistA = n
			break
		}
	}
	if artistA.ObjectID == "" {
		t.Fatal("Artist A folder not present in index")
	}
	if got := len(artistA.ChildFolderIDs); got != 2 {
		t.Errorf("Artist A ChildFolderIDs count = %d, want 2 (Album X, Album Y)", got)
	}
	if got := len(artistA.ChildTrackIDs); got != 0 {
		t.Errorf("Artist A ChildTrackIDs count = %d, want 0 (no direct tracks)", got)
	}

	// Find Album X under Artist A.
	var albumX FolderNode
	for _, n := range idx.Folders {
		if n.RelPath == "Artist A/Album X" {
			albumX = n
			break
		}
	}
	if albumX.ObjectID == "" {
		t.Fatal("Artist A/Album X folder not present in index")
	}
	if albumX.ParentID != artistA.ObjectID {
		t.Errorf("Album X ParentID = %q, want %q (Artist A)", albumX.ParentID, artistA.ObjectID)
	}
	if got := len(albumX.ChildTrackIDs); got != 2 {
		t.Errorf("Album X ChildTrackIDs count = %d, want 2", got)
	}
}

func Test_BuildFolderIndex_TopLevelTracks(t *testing.T) {
	idx := BuildFolderIndex([]TrackInfo{
		folderTrack("t1", "loose-at-root.flac"),
		folderTrack("t2", "Artist A/track 01.flac"),
	})

	if got := len(idx.TopLevelTrackIDs); got != 1 {
		t.Errorf("TopLevelTrackIDs count = %d, want 1", got)
	}
	if got := len(idx.TopLevelFolderIDs); got != 1 {
		t.Errorf("TopLevelFolderIDs count = %d, want 1", got)
	}
}

func Test_BuildFolderIndex_StableOrdering(t *testing.T) {
	// Run the same input twice and verify identical output ordering.
	// Critical for pagination correctness.
	tracks := []TrackInfo{
		folderTrack("z1", "Z Artist/track.flac"),
		folderTrack("a1", "A Artist/track.flac"),
		folderTrack("m1", "M Artist/track.flac"),
	}
	a := BuildFolderIndex(tracks)
	b := BuildFolderIndex(tracks)
	if strings.Join(a.TopLevelFolderIDs, ",") != strings.Join(b.TopLevelFolderIDs, ",") {
		t.Errorf("TopLevelFolderIDs ordering not stable: %v vs %v", a.TopLevelFolderIDs, b.TopLevelFolderIDs)
	}
	// Verify sorted-by-Name ordering: A, M, Z.
	names := make([]string, 0, len(a.TopLevelFolderIDs))
	for _, id := range a.TopLevelFolderIDs {
		names = append(names, a.Folders[id].Name)
	}
	want := []string{"A Artist", "M Artist", "Z Artist"}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("TopLevelFolderIDs[%d] = %q, want %q (full order: %v)", i, n, want[i], names)
		}
	}
}

func Test_BuildFolderIndex_LookupTrackRoundtrip(t *testing.T) {
	idx := BuildFolderIndex([]TrackInfo{
		folderTrack("t1", "Artist A/track 01.flac"),
	})
	ti, ok := idx.LookupTrack("t1")
	if !ok {
		t.Fatal("LookupTrack(t1) returned not-found")
	}
	if ti.TrackID != "t1" {
		t.Errorf("LookupTrack returned wrong TrackID: %q", ti.TrackID)
	}
}

func TestRelParentDir(t *testing.T) {
	cases := []struct {
		name    string
		absPath string
		libRoot string
		want    string
	}{
		// F1: a Windows backslash path against the forward-slashed libRoot
		// (longestCommonPathPrefix always normalizes). The prefix strip must
		// run AFTER normalization or the raw "C:/lib" leaks into the hierarchy.
		{"windows_backslash", `C:\lib\Artist\Album\track.flac`, "C:/lib", "Artist/Album"},
		{"unix_forward_slash", "/library/Artist/Album/track.flac", "/library", "Artist/Album"},
		// Directly under the root → no folder component.
		{"track_directly_under_root", "/library/track.flac", "/library", ""},
		// Empty libRoot: normalize separators, no prefix strip.
		{"no_libroot_backslash", `D:\Artist\Album\x.flac`, "", "D:/Artist/Album"},
		{"empty_abspath", "", "/library", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relParentDir(tc.absPath, tc.libRoot); got != tc.want {
				t.Errorf("relParentDir(%q, %q) = %q, want %q", tc.absPath, tc.libRoot, got, tc.want)
			}
		})
	}
}
