//go:build !windows

// Symlink creation needs no special privilege on POSIX; on Windows it
// requires Developer Mode or SeCreateSymbolicLinkPrivilege, so the
// fixture can't be staged there. The production code path is
// platform-independent (os.FileInfo.Mode()&os.ModeSymlink).

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// findEntry returns the named entry from a /v1/list response.
func findEntry(t *testing.T, entries []Entry, name string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("entry %q not in listing %+v", name, entries)
	return Entry{}
}

// /v1/list built its rows from `os.File.Readdir`, whose values are
// documented as "as would be returned by Lstat" — while /v1/stat and
// /v1/download reach the same path through the resolver's `os.Stat`,
// which FOLLOWS the link. A symlinked album directory therefore listed
// as `{"isDir": false, "size": <length of the link's target string>}`
// while /v1/stat called it a directory and /v1/download rejected it as
// one: three endpoints disagreeing about the same path. `internal/fs`'s
// package doc states symlinked content inside a root is trusted and
// served, so this was an oversight, not a carve-out.
func TestListFollowsSymlinksMatchingStat(t *testing.T) {
	hs, tok, root := fileFixture(t)

	// A symlinked album directory next to the real one.
	if err := os.Symlink(filepath.Join(root, "Artist", "Album"),
		filepath.Join(root, "Artist", "LinkedAlbum")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	// …and a symlinked track file, whose Size must be the target's.
	if err := os.Symlink(filepath.Join(root, "Artist", "Single.flac"),
		filepath.Join(root, "Artist", "LinkedSingle.flac")); err != nil {
		t.Fatal(err)
	}

	resp := authGet(t, hs, "/v1/list?path=Artist", tok)
	defer resp.Body.Close()
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}

	linkedDir := findEntry(t, entries, "LinkedAlbum")
	if !linkedDir.IsDir {
		t.Errorf("LinkedAlbum.isDir = false; /v1/stat calls it a directory "+
			"and /v1/download 400s it as one (entry: %+v)", linkedDir)
	}

	// `targetEntry`, not `real` — the latter shadows a Go predeclared
	// identifier (golangci-lint `predeclared`).
	linkedFile := findEntry(t, entries, "LinkedSingle.flac")
	targetEntry := findEntry(t, entries, "Single.flac")
	if linkedFile.Size != targetEntry.Size {
		t.Errorf("LinkedSingle.flac size = %d, want %d (the TARGET's size; "+
			"an Lstat size is the length of the link's path string)",
			linkedFile.Size, targetEntry.Size)
	}
	// ModTime comes off the same stat, so a regression that resolved only
	// IsDir/Size would pass without this.
	if !linkedFile.ModTime.Equal(targetEntry.ModTime) {
		t.Errorf("LinkedSingle.flac mtime = %s, want the target's %s",
			linkedFile.ModTime, targetEntry.ModTime)
	}
	if linkedFile.IsDir {
		t.Errorf("LinkedSingle.flac should not be a directory: %+v", linkedFile)
	}

	// The listing must agree with what /v1/stat says about the same path.
	statResp := authGet(t, hs, "/v1/stat?path=Artist/LinkedAlbum", tok)
	defer statResp.Body.Close()
	var st StatResponse
	if err := json.NewDecoder(statResp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.IsDir != linkedDir.IsDir {
		t.Errorf("/v1/list isDir=%v disagrees with /v1/stat isDir=%v for the same path",
			linkedDir.IsDir, st.IsDir)
	}
}

// A dangling symlink must still APPEAR in the listing — falling back to
// the Readdir info on a stat failure is what keeps a broken link (or a
// target on a mount that just went away) visible instead of silently
// vanishing from the directory.
func TestListKeepsDanglingSymlinkVisible(t *testing.T) {
	hs, tok, root := fileFixture(t)
	if err := os.Symlink(filepath.Join(root, "Artist", "Nope.flac"),
		filepath.Join(root, "Artist", "Dangling.flac")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	resp := authGet(t, hs, "/v1/list?path=Artist", tok)
	defer resp.Body.Close()
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	findEntry(t, entries, "Dangling.flac") // fails the test if absent
}
