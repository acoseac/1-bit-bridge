package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// writeFileForTest writes a few bytes at path.
func writeFileForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// resolvedForTest is where path's links lead.
func resolvedForTest(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestEntryInTheWaySaysWhatStandsThere pins the words bridge serve's refusal
// of a data directory and bridge doctor's lines give each kind of entry in the
// way (B606, B659): a regular file or another kind of entry, a link that leads
// to one, with where, one whose links could not be resolved, without, and a
// link to nothing.
func TestEntryInTheWaySaysWhatStandsThere(t *testing.T) {
	for _, tc := range []struct {
		in   EntryInTheWay
		want string
	}{
		{EntryInTheWay{Entry: "/srv/data", Kind: "a regular file"}, "/srv/data is a regular file, not a directory"},
		{EntryInTheWay{Entry: "/srv/data", Kind: "a named pipe"}, "/srv/data is a named pipe, not a directory"},
		{EntryInTheWay{Entry: "/srv/data", Link: true, Target: "/mnt/file", Kind: "a regular file"}, "/srv/data leads to /mnt/file, a regular file, not a directory"},
		{EntryInTheWay{Entry: "/srv/data", Link: true, Kind: "a regular file"}, "/srv/data is a symbolic link to a regular file, not a directory"},
		{EntryInTheWay{Entry: "/srv/data", Link: true}, "/srv/data is a symbolic link to nothing"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%+v says %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNotADirectoryNamesAnEntryThatIsNoDirectory: an entry that is there and
// is no directory, a regular file, a link to one (with where it leads) or a
// link to nothing, is named, as it is; a directory and a link to one are
// nothing in the way; an entry that is not there is no answer, its Lstat's
// fs.ErrNotExist.
func TestNotADirectoryNamesAnEntryThatIsNoDirectory(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	writeFileForTest(t, file)
	dir := filepath.Join(base, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("a regular file", func(t *testing.T) {
		in, err := NotADirectory(file)
		if want := (EntryInTheWay{Entry: file, Kind: "a regular file"}); err != nil || in == nil || *in != want {
			t.Errorf("NotADirectory(a regular file) = %+v, %v; want %+v", in, err, want)
		}
	})
	t.Run("a directory", func(t *testing.T) {
		if in, err := NotADirectory(dir); in != nil || err != nil {
			t.Errorf("NotADirectory(a directory) = %+v, %v; want nothing in the way", in, err)
		}
	})
	t.Run("not there", func(t *testing.T) {
		if in, err := NotADirectory(filepath.Join(base, "nothing")); in != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("NotADirectory(nothing there) = %+v, %v; want no answer and fs.ErrNotExist", in, err)
		}
	})
	t.Run("a link to a regular file", func(t *testing.T) {
		link := filepath.Join(base, "to-file")
		symlinkOrSkip(t, file, link)
		in, err := NotADirectory(link)
		if want := (EntryInTheWay{Entry: link, Link: true, Target: resolvedForTest(t, link), Kind: "a regular file"}); err != nil || in == nil || *in != want {
			t.Errorf("NotADirectory(a link to a regular file) = %+v, %v; want %+v", in, err, want)
		}
	})
	t.Run("a link to nothing", func(t *testing.T) {
		link := filepath.Join(base, "to-nothing")
		symlinkOrSkip(t, filepath.Join(base, "nothing"), link)
		in, err := NotADirectory(link)
		if want := (EntryInTheWay{Entry: link, Link: true}); err != nil || in == nil || *in != want {
			t.Errorf("NotADirectory(a link to nothing) = %+v, %v; want %+v", in, err, want)
		}
	})
	t.Run("a link to a directory", func(t *testing.T) {
		link := filepath.Join(base, "to-dir")
		symlinkOrSkip(t, dir, link)
		if in, err := NotADirectory(link); in != nil || err != nil {
			t.Errorf("NotADirectory(a link to a directory) = %+v, %v; want nothing in the way", in, err)
		}
	})
}

// TestBlockingEntryFindsTheEntryInThePathsWay walks up each shape bridge serve
// refuses a data directory for (B606), and those it takes: the first entry of
// the path that is there is named nearest, and what stands there when it is no
// directory (a regular file, a link to one, a link to nothing, at the path or
// above it, through every entry an Lstat finds not there or below an entry that
// is no directory) is the blocker; a directory there, or a link to one, is no
// blocker, the path itself or below it. The walk makes nothing.
func TestBlockingEntryFindsTheEntryInThePathsWay(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lay lays the case out below base, and returns the path walked, the
		// entry the walk stops at, and what stands there, nil for none.
		lay func(t *testing.T, base string) (path, nearest string, blocker *EntryInTheWay)
	}{
		{"a regular file at the path", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			p := filepath.Join(base, "data")
			writeFileForTest(t, p)
			return p, p, &EntryInTheWay{Entry: p, Kind: "a regular file"}
		}},
		{"an ancestor that is a regular file", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			a := filepath.Join(base, "config")
			writeFileForTest(t, a)
			return filepath.Join(a, "sub", "data"), a, &EntryInTheWay{Entry: a, Kind: "a regular file"}
		}},
		{"an ancestor that is a link to a regular file", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			file := filepath.Join(base, "file")
			writeFileForTest(t, file)
			a := filepath.Join(base, "config")
			symlinkOrSkip(t, file, a)
			return filepath.Join(a, "sub", "data"), a, &EntryInTheWay{Entry: a, Link: true, Target: resolvedForTest(t, a), Kind: "a regular file"}
		}},
		{"an ancestor that is a link to nothing", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			a := filepath.Join(base, "config")
			symlinkOrSkip(t, filepath.Join(base, "nothing"), a)
			return filepath.Join(a, "data"), a, &EntryInTheWay{Entry: a, Link: true}
		}},
		{"a link to nothing at the path", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			p := filepath.Join(base, "data")
			symlinkOrSkip(t, filepath.Join(base, "nothing"), p)
			return p, p, &EntryInTheWay{Entry: p, Link: true}
		}},
		{"a directory at the path", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			p := filepath.Join(base, "data")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			return p, p, nil
		}},
		{"not made yet below a directory", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			return filepath.Join(base, "config", "data"), base, nil
		}},
		{"not made yet below a link to a directory", func(t *testing.T, base string) (string, string, *EntryInTheWay) {
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "link")
			symlinkOrSkip(t, target, link)
			return filepath.Join(link, "sub", "data"), link, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, wantNearest, wantBlocker := tc.lay(t, t.TempDir())
			nearest, blocker, err := BlockingEntry(path)
			switch {
			case err != nil:
				t.Fatalf("BlockingEntry(%s) = %v", path, err)
			case nearest != wantNearest:
				t.Errorf("BlockingEntry(%s) stopped at %s, want %s", path, nearest, wantNearest)
			case (blocker == nil) != (wantBlocker == nil) || blocker != nil && *blocker != *wantBlocker:
				t.Errorf("BlockingEntry(%s) found %+v in the way, want %+v", path, blocker, wantBlocker)
			}
			if _, err := os.Lstat(path); wantBlocker == nil && wantNearest != path && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the walk made %s (lstat: %v), want nothing made", path, err)
			}
		})
	}
}
