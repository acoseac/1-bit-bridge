package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// walkedNames walks what WalkableRoot answers for root, and returns every
// path the walk hands its callback below the root, relative to root as a
// caller computes it (filepath.Rel against root, the configured spelling),
// and whether any of them was spelled other than under root.
func walkedNames(t *testing.T, root string) (names []string, foreign []string) {
	t.Helper()
	walkPath, err := WalkableRoot(root)
	if err != nil {
		t.Fatalf("WalkableRoot(%s): %v", root, err)
	}
	prefix := filepath.Clean(root) + string(filepath.Separator)
	err = filepath.WalkDir(walkPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == walkPath {
			return nil
		}
		if !strings.HasPrefix(p, prefix) {
			foreign = append(foreign, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk of %s: %v", walkPath, err)
	}
	sort.Strings(names)
	return names, foreign
}

// requireWalksLibraryTree asserts that a walk from WalkableRoot's answer for
// root visits exactly wantLibraryTree, every path under root's own spelling.
func requireWalksLibraryTree(t *testing.T, root string) {
	t.Helper()
	names, foreign := walkedNames(t, root)
	if strings.Join(names, " ") != strings.Join(wantLibraryTree, " ") {
		t.Errorf("walk through %s visited %v, want %v", root, names, wantLibraryTree)
	}
	if len(foreign) > 0 {
		t.Errorf("walk through %s reported paths outside its spelling: %v", root, foreign)
	}
}

// libraryTree makes a directory holding Artist/Album/01.flac.
func libraryTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	album := filepath.Join(dir, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(album, "01.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// symlinkOrSkip links link to target, and skips on a host that cannot.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}
}

var wantLibraryTree = []string{"Artist", "Artist/Album", "Artist/Album/01.flac"}

// TestWalkableRootDescendsALinkedRoot is the defect: filepath.WalkDir Lstats
// its root, so a root that is a link to a directory was walked as one entry.
// Through WalkableRoot the walk descends, a link to a link as well, and every
// path it reports stays under the root's configured spelling, never the
// target's.
func TestWalkableRootDescendsALinkedRoot(t *testing.T) {
	target := libraryTree(t)
	base := t.TempDir()
	link := filepath.Join(base, "music")
	symlinkOrSkip(t, target, link)
	chain := filepath.Join(base, "chained")
	symlinkOrSkip(t, link, chain)

	for _, root := range []string{link, chain} {
		// The defect, measured the way the scanner walked: one entry.
		var raw []string
		_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error {
			raw = append(raw, p)
			return nil
		})
		if len(raw) != 1 {
			t.Fatalf("premise: a walk of the link %s itself visited %v, want the link alone", root, raw)
		}

		requireWalksLibraryTree(t, root)
	}
}

// TestWalkableRootLeavesEverythingElseAlone: an ordinary directory, a root
// spelled with a trailing separator, and anything that is not a directory
// even through a link, come back unchanged, so those walks are what they
// were.
func TestWalkableRootLeavesEverythingElseAlone(t *testing.T) {
	dir := libraryTree(t)
	file := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []string{dir, dir + string(filepath.Separator), file}
	fileLink := filepath.Join(t.TempDir(), "song-link.flac")
	if err := os.Symlink(file, fileLink); err == nil {
		cases = append(cases, fileLink)
	}
	for _, root := range cases {
		got, err := WalkableRoot(root)
		if err != nil || got != root {
			t.Errorf("WalkableRoot(%s) = %q, %v; want it unchanged", root, got, err)
		}
	}
}

// TestWalkableRootRefusesWhatItCannotSee: a missing root, and a link whose
// target is gone (a mount that went away), answer an error and "", never the
// root unresolved: a walk that could not see the directory must say so, and
// walking the link itself is the defect.
func TestWalkableRootRefusesWhatItCannotSee(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	dangling := filepath.Join(base, "dangling")
	symlinkOrSkip(t, filepath.Join(base, "unmounted"), dangling)
	for _, root := range []string{missing, dangling} {
		got, err := WalkableRoot(root)
		if !errors.Is(err, fs.ErrNotExist) || got != "" {
			t.Errorf("WalkableRoot(%s) = %q, %v; want \"\" and a not-exist error", root, got, err)
		}
	}
}

// stubInfo is a stat of the given mode, for a shape the host cannot make.
type stubInfo fs.FileMode

func (m stubInfo) Name() string       { return "root" }
func (m stubInfo) Size() int64        { return 0 }
func (m stubInfo) Mode() fs.FileMode  { return fs.FileMode(m) }
func (m stubInfo) ModTime() time.Time { return time.Time{} }
func (m stubInfo) IsDir() bool        { return fs.FileMode(m).IsDir() }
func (m stubInfo) Sys() any           { return nil }

// TestWalkableRootTakesAWindowsJunction drives the decision with the stats a
// Windows host gives, on every host. Since Go 1.23 os.Lstat reports a
// directory junction and a volume mounted in a folder ModeIrregular without
// ModeDir, so the walk saw one entry that is not a directory; os.Stat follows
// it to the directory, and os.Lstat of the path with a trailing separator
// follows it too (lstatNolog's followSurrogates). A separator that does not
// lead into the directory must fail the walk, not empty it.
func TestWalkableRootTakesAWindowsJunction(t *testing.T) {
	root := filepath.Join("vol", "Music")
	walkPath := root + string(filepath.Separator)
	dir := stubInfo(fs.ModeDir | 0o755)
	junction := stubInfo(fs.ModeIrregular | 0o666)
	stat := func(string) (fs.FileInfo, error) { return dir, nil }

	followed := func(p string) (fs.FileInfo, error) {
		if p == walkPath {
			return dir, nil
		}
		return junction, nil
	}
	if got, err := walkableRoot(root, followed, stat); err != nil || got != walkPath {
		t.Errorf("a junction: walkableRoot = %q, %v; want %q", got, err, walkPath)
	}

	notFollowed := func(string) (fs.FileInfo, error) { return junction, nil }
	if got, err := walkableRoot(root, notFollowed, stat); !errors.Is(err, ErrRootNotEntered) || got != "" {
		t.Errorf("a separator that does not lead in: walkableRoot = %q, %v; want \"\" and ErrRootNotEntered", got, err)
	}

	gone := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	if got, err := walkableRoot(root, notFollowed, gone); !errors.Is(err, fs.ErrNotExist) || got != "" {
		t.Errorf("a junction to a volume that is gone: walkableRoot = %q, %v; want \"\" and a not-exist error", got, err)
	}
}
