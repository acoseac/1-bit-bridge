package admin

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/acoseac/1-bit-bridge/internal/trash"
)

// spellingVariants are two spellings of one folder that a case-insensitive,
// normalization-insensitive volume (APFS, the macOS default) opens as the same
// directory: the spelling on disk, and the one a client sends. A volume that
// tells them apart (ext4, or NTFS for the normalization pair) opens only the
// first, and the tests below skip there.
var spellingVariants = []struct{ name, disk, client string }{
	{"case", "Artist/Album", "artist/ALBUM"},
	{"normalization", norm.NFC.String("Café/Album"), norm.NFD.String("Café/Album")},
}

// newSpellingServer is newTestServer with uploads and deletes switched on and
// the trash wired as production wires it, and the library root.
func newSpellingServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	enableUploads(t, srv)
	resetSpaceCacheForTest()
	t.Cleanup(resetSpaceCacheForTest)
	wireTrash(t, srv)
	enableDelete(t, srv, true)
	return srv, cfg.LibraryRoots[0]
}

// seedAndScan writes each of rels under root and runs a full scan, so the
// rows carry the spelling the walk reads.
func seedAndScan(t *testing.T, srv *Server, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		seedLibraryFile(t, root, rel, "audio of "+rel)
	}
	if _, err := srv.deps.Scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// skipUnlessOneFolder skips where the volume under root does not open the
// client's spelling of a folder as the folder on disk.
func skipUnlessOneFolder(t *testing.T, root, disk, client string) {
	t.Helper()
	a, err := os.Stat(filepath.Join(root, filepath.FromSlash(disk)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(root, filepath.FromSlash(client)))
	if err != nil || !os.SameFile(a, b) {
		t.Skipf("this volume does not open %+q as %+q", client, disk)
	}
}

// linkTheRealFolder makes root/Linked a link to root/Real, or skips where no
// link can be made.
func linkTheRealFolder(t *testing.T, root string) {
	t.Helper()
	if err := os.Symlink(filepath.Join(root, "Real"), filepath.Join(root, "Linked")); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
}

// uploadAll uploads files into the library in one session through the real
// session, chunk and commit handlers, and returns the commit's answer once
// the scan it spawned has finished.
func uploadAll(t *testing.T, srv *Server, files map[string][]byte) map[string]any {
	t.Helper()
	declared := make([]map[string]any, 0, len(files))
	for rel, body := range files {
		declared = append(declared, map[string]any{"path": rel, "size": len(body)})
	}
	sess := createSession(t, srv, declared, nil)
	sid, _ := sess["id"].(string)
	listed, _ := sess["files"].([]any)
	if len(listed) != len(files) {
		t.Fatalf("the session took %d files of %d: %v", len(listed), len(files), sess)
	}
	for _, f := range listed {
		f, _ := f.(map[string]any)
		fid, _ := f["id"].(string)
		rel, _ := f["path"].(string)
		if code, out := putChunk(t, srv.Handler(), sid, fid, 0, files[rel], nil); code != http.StatusOK {
			t.Fatalf("chunk of %q = %d, body %v", rel, code, out)
		}
	}
	var commit map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/upload/sessions/"+sid+"/commit", nil, &commit); code != http.StatusOK {
		t.Fatalf("commit = %d, body %v", code, commit)
	}
	if n, _ := commit["committed"].(float64); int(n) != len(files) {
		t.Fatalf("committed = %v, want %d (%v)", commit["committed"], len(files), commit)
	}
	srvBgScansWait(srv)
	return commit
}

// trashPaths deletes paths through the real handler and waits for the scan
// it spawned.
func trashPaths(t *testing.T, srv *Server, paths ...string) {
	t.Helper()
	var res map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/trash",
		map[string]any{"paths": paths}, &res); code != http.StatusOK {
		t.Fatalf("trash = %d (%v)", code, res)
	}
	if ok, _ := res["ok"].(float64); int(ok) != len(paths) {
		t.Fatalf("trash result = %v, want %d trashed", res, len(paths))
	}
	srvBgScansWait(srv)
}

// restoreTheOneEntry restores the trash's one entry through the real
// handlers, waits for the scan the restore spawned, and returns the entry as
// the listing showed it.
func restoreTheOneEntry(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	var listed []map[string]any
	doJSON(t, srv.Handler(), "GET", "/api/library/trash", nil, &listed)
	if len(listed) != 1 {
		t.Fatalf("trash listing has %d entries, want 1", len(listed))
	}
	id, _ := listed[0]["id"].(string)
	var res map[string]any
	if code := doJSON(t, srv.Handler(), "POST", "/api/library/trash/restore",
		map[string]any{"ids": []string{id}}, &res); code != http.StatusOK {
		t.Fatalf("restore = %d (%v)", code, res)
	}
	if ok, _ := res["ok"].(float64); int(ok) != 1 {
		t.Fatalf("restore result = %v", res)
	}
	srvBgScansWait(srv)
	return listed[0]
}

// wantRows fails unless the store holds exactly want (sorted) as its tracks.
func wantRows(t *testing.T, srv *Server, when string, want ...string) {
	t.Helper()
	paths, err := srv.deps.Manifest.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, want) {
		t.Errorf("rows %s: %+q, want %+q", when, paths, want)
	}
}

// TestAnUploadRescanIndexesNoSecondSpellingOfTheFolder: the scan an upload
// commit runs reads the folder the files landed in under the spelling its
// parent lists, never the spelling the client sent. The scanner makes each
// row's path from the spelling of the directory it is handed, and a volume
// that opens the client's spelling as the folder on disk (APFS: case- and
// normalization-insensitive) put the upload in that folder, so on main at
// 7e324642 the commit's subtree scan indexed every file of the folder a
// second time under the client's spelling (backlog B219): both rows of each
// file reach every paired device until the next full scan.
func TestAnUploadRescanIndexesNoSecondSpellingOfTheFolder(t *testing.T) {
	for _, tc := range spellingVariants {
		t.Run(tc.name, func(t *testing.T) {
			srv, root := newSpellingServer(t)
			seedAndScan(t, srv, root, tc.disk+"/01.flac")
			skipUnlessOneFolder(t, root, tc.disk, tc.client)

			commit := uploadAll(t, srv, map[string][]byte{tc.client + "/02.flac": []byte("second track")})

			wantRows(t, srv, "after the upload", tc.disk+"/01.flac", tc.disk+"/02.flac")
			if dirs, _ := commit["scanDirs"].([]any); len(dirs) != 1 || dirs[0] != tc.disk {
				t.Errorf("scanDirs = %+q, want [%+q], the folder as it is spelled on disk", dirs, tc.disk)
			}
		})
	}
}

// TestAnUploadThroughALinkedFolderIsIndexedWhereTheWalkFindsIt: a folder the
// upload reached through a link to a directory has no on-disk spelling, since
// a walk from the root does not descend links below it, so the commit scans
// the library rather than the folder under the link's name. On main at
// 7e324642 the subtree scan walked through the link and indexed the target
// folder's every file a second time under the link's name.
func TestAnUploadThroughALinkedFolderIsIndexedWhereTheWalkFindsIt(t *testing.T) {
	srv, root := newSpellingServer(t)
	seedAndScan(t, srv, root, "Real/Album/01.flac")
	linkTheRealFolder(t, root)

	commit := uploadAll(t, srv, map[string][]byte{"Linked/Album/02.flac": []byte("second track")})

	wantRows(t, srv, "after the upload", "Real/Album/01.flac", "Real/Album/02.flac")
	if full, _ := commit["fullScan"].(bool); !full {
		t.Errorf("fullScan = false, scanDirs %q: a folder reached through a link is left to a full scan", commit["scanDirs"])
	}
}

// TestAnUploadOfOneFolderUnderTwoSpellingsScansItOnce: a commit whose files
// name one folder under two spellings scans it once, as its parent lists it.
func TestAnUploadOfOneFolderUnderTwoSpellingsScansItOnce(t *testing.T) {
	srv, root := newSpellingServer(t)
	seedAndScan(t, srv, root, "Artist/Album/01.flac")
	skipUnlessOneFolder(t, root, "Artist/Album", "artist/ALBUM")

	commit := uploadAll(t, srv, map[string][]byte{
		"Artist/Album/02.flac": []byte("second track"),
		"artist/ALBUM/03.flac": []byte("third track"),
	})

	if dirs, _ := commit["scanDirs"].([]any); len(dirs) != 1 || dirs[0] != "Artist/Album" {
		t.Errorf("scanDirs = %q, want [Artist/Album]: the two spellings are one folder", dirs)
	}
	wantRows(t, srv, "after the upload", "Artist/Album/01.flac", "Artist/Album/02.flac", "Artist/Album/03.flac")
}

// TestATrackTrashedThroughALinkedFolderLeavesNoRowsUnderTheLink: a delete
// that reaches a track through a link to a directory has no on-disk spelling
// to rescan under, so the library is rescanned instead (trash.Result's
// FullScan). On main at 7e324642 the folder was rescanned through the link,
// which indexed the target folder's other files a second time under the
// link's name.
func TestATrackTrashedThroughALinkedFolderLeavesNoRowsUnderTheLink(t *testing.T) {
	srv, root := newSpellingServer(t)
	seedAndScan(t, srv, root, "Real/Album/01.flac", "Real/Album/02.flac")
	linkTheRealFolder(t, root)

	trashPaths(t, srv, "Linked/Album/01.flac")

	got, err := srv.deps.Manifest.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "Real/Album/02.flac") || slices.ContainsFunc(got, func(p string) bool {
		return strings.HasPrefix(p, "Linked/")
	}) {
		t.Errorf("rows after the delete: %q, want Real/Album/02.flac and none under Linked/: "+
			"the folder was rescanned through the link", got)
	}
}

// TestATrackTrashedUnderAnotherSpellingLeavesNoSecondSpelling: a delete names
// the track as the client spelled it, and a volume that opens that spelling
// moves the track on disk all the same. On main at 7e324642 the row was then
// retired under the client's spelling, which names no row, so the track's
// row stayed until full scans counted it missing, and the folder was rescanned
// under the client's spelling, which indexed its other files a second time.
// The trash now records, retires and rescans the track as its folder lists
// it, so a restore puts it back under that spelling too.
func TestATrackTrashedUnderAnotherSpellingLeavesNoSecondSpelling(t *testing.T) {
	for _, tc := range spellingVariants {
		t.Run(tc.name, func(t *testing.T) {
			srv, root := newSpellingServer(t)
			seedAndScan(t, srv, root, tc.disk+"/01.flac", tc.disk+"/02.flac")
			skipUnlessOneFolder(t, root, tc.disk, tc.client)

			trashPaths(t, srv, tc.client+"/01.flac")
			wantRows(t, srv, "after the delete", tc.disk+"/02.flac")

			entry := restoreTheOneEntry(t, srv)
			if got := entry["originalPath"]; got != tc.disk+"/01.flac" {
				t.Errorf("originalPath = %+q, want %+q, the track as its folder listed it", got, tc.disk+"/01.flac")
			}
			wantRows(t, srv, "after the restore", tc.disk+"/01.flac", tc.disk+"/02.flac")
		})
	}
}

// TestARestoreOfAnEntryTrashedUnderAnotherSpellingIndexesNoSecondSpelling: an
// entry an earlier build trashed is recorded under the spelling its delete
// was sent with, and its restore lands in the folder that spelling opens. The
// restore's scan reads that folder as its parent lists it; on main at
// 7e324642 it read it under the recorded spelling and indexed the folder's
// every file a second time.
func TestARestoreOfAnEntryTrashedUnderAnotherSpellingIndexesNoSecondSpelling(t *testing.T) {
	for _, tc := range spellingVariants {
		t.Run(tc.name, func(t *testing.T) {
			srv, root := newSpellingServer(t)
			seedLibraryFile(t, filepath.Join(root, trash.DirName, "1700000000000000000"), tc.client+"/01.flac", "first track")
			seedAndScan(t, srv, root, tc.disk+"/02.flac")
			skipUnlessOneFolder(t, root, tc.disk, tc.client)

			restoreTheOneEntry(t, srv)

			wantRows(t, srv, "after the restore", tc.disk+"/01.flac", tc.disk+"/02.flac")
		})
	}
}
