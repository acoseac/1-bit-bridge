package api

import (
	"io/fs"
	"os"
	"testing"
	"time"
)

// entryInfo is the whole fs.FileInfo surface resolveEntryInfo and the
// /v1/list row built from it read: the mode decides the branch, and
// IsDir/Size/ModTime are what the row carries. IsDir derives FROM the
// mode, as os.fileStat's does, so one field drives the decision.
type entryInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (e entryInfo) Name() string       { return e.name }
func (e entryInfo) Size() int64        { return e.size }
func (e entryInfo) Mode() fs.FileMode  { return e.mode }
func (e entryInfo) ModTime() time.Time { return e.mod }
func (e entryInfo) IsDir() bool        { return e.mode.IsDir() }
func (e entryInfo) Sys() any           { return nil }

// linkSize / targetSize are deliberately different so an assertion says
// WHICH of the two infos came back, not merely that the fields agree.
const linkSize, targetSize = 31, 4096

// TestResolveEntryInfoHandlesAWindowsJunction.
//
// /v1/list resolved its rows by testing the ModeSymlink BIT, and the
// shape that matters most on Windows does not carry it. Since Go 1.23's
// winsymlink change, isReparseTagNameSurrogate is true for a mount
// point, so Lstat gives a directory JUNCTION (`mklink /J`,
// IO_REPARSE_TAG_MOUNT_POINT) ModeIrregular and withholds ModeDir — it
// reports IsDir() false with no ModeSymlink bit, so the entry was
// returned unresolved and an album parked on another volume listed as a
// non-directory iOS cannot open, while /v1/stat and /v1/download called
// the same path a directory. A junction is the ORDINARY way to park one
// there: a real symlink needs SeCreateSymbolicLinkPrivilege that a
// service account usually lacks.
//
// Driven as (info, stat) because the Windows shape cannot be built on
// any other platform, and a test that skips everywhere but one CI leg
// looks exactly like one that passed. The package's other link tests
// (files_symlink_test.go) are `//go:build !windows` for exactly that
// reason and cover the wire end of the same path.
func TestResolveEntryInfoHandlesAWindowsJunction(t *testing.T) {
	targetMod := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	dir := func() (os.FileInfo, error) {
		return entryInfo{size: targetSize, mode: fs.ModeDir | 0o755, mod: targetMod}, nil
	}
	file := func() (os.FileInfo, error) {
		return entryInfo{size: targetSize, mode: 0o644, mod: targetMod}, nil
	}
	dangling := func() (os.FileInfo, error) { return nil, fs.ErrNotExist }
	walled := func() (os.FileInfo, error) { return nil, fs.ErrPermission }
	// The other non-regular POSIX kinds stat to THEMSELVES, so the stat
	// answers with the entry's own shape — which is what makes "widening
	// the test leaves their row alone" a claim the table can check.
	itself := func(mode fs.FileMode) func() (os.FileInfo, error) {
		return func() (os.FileInfo, error) {
			return entryInfo{size: linkSize, mode: mode}, nil
		}
	}

	for _, tc := range []struct {
		name     string
		mode     fs.FileMode
		stat     func() (os.FileInfo, error)
		wantDir  bool
		wantSize int64
	}{
		// The live defect: a Windows junction pointing at a directory.
		// It must list as a browsable folder, which is what /v1/stat and
		// /v1/download already call it.
		{"junction to a directory", fs.ModeIrregular, dir, true, targetSize},
		{"junction to a file", fs.ModeIrregular, file, false, targetSize},
		// A POSIX symlink: unchanged from the behaviour this widens.
		{"symlink to a directory", fs.ModeSymlink, dir, true, targetSize},
		{"symlink to a file", fs.ModeSymlink, file, false, targetSize},
		// A link whose target cannot be reached stays VISIBLE in the
		// listing, on the Readdir info, rather than vanishing from the
		// directory or failing the whole request.
		{"dangling symlink", fs.ModeSymlink, dangling, false, linkSize},
		{"junction that cannot be stat'd", fs.ModeIrregular, walled, false, linkSize},
		// Widening the test must leave every field of these rows alone.
		{"a named pipe", fs.ModeNamedPipe, itself(fs.ModeNamedPipe), false, linkSize},
		{"a socket", fs.ModeSocket, itself(fs.ModeSocket), false, linkSize},
		{"a device node", fs.ModeDevice, itself(fs.ModeDevice), false, linkSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEntryInfo(
				entryInfo{name: "Album", size: linkSize, mode: tc.mode}, tc.stat)
			if got.IsDir() != tc.wantDir {
				t.Errorf("IsDir() = %v, want %v — /v1/stat and /v1/download resolve "+
					"this path through os.Stat, so the listing must agree with them",
					got.IsDir(), tc.wantDir)
			}
			if got.Size() != tc.wantSize {
				t.Errorf("Size() = %d, want %d", got.Size(), tc.wantSize)
			}
		})
	}
}

// Neither shape Readdir has already settled pays for a stat: this runs
// once per entry on a directory that can hold thousands, often over a
// network mount where the second syscall is the expensive one. A real
// directory needs no resolving either — os.Stat of one returns the same
// directory.
func TestResolveEntryInfoDoesNotStatWhatReaddirAlreadySettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode fs.FileMode
	}{
		{"a plain file", 0o644},
		{"a plain directory", fs.ModeDir | 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statted := false
			got := resolveEntryInfo(
				entryInfo{name: "Album", size: linkSize, mode: tc.mode},
				func() (os.FileInfo, error) {
					statted = true
					return entryInfo{size: targetSize, mode: tc.mode}, nil
				})
			if statted {
				t.Error("the target was stat'd for an entry Readdir had already classified")
			}
			if got.Size() != linkSize || got.IsDir() != tc.mode.IsDir() {
				t.Errorf("got {size %d, isDir %v}, want the entry's own {%d, %v}",
					got.Size(), got.IsDir(), linkSize, tc.mode.IsDir())
			}
		})
	}
}

// ModTime comes off the same stat as IsDir and Size, so a regression
// that resolved only the first two would pass the table above.
func TestResolveEntryInfoReportsTheTargetModTime(t *testing.T) {
	linkMod := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	targetMod := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)

	got := resolveEntryInfo(
		entryInfo{name: "Album", mode: fs.ModeIrregular, mod: linkMod},
		func() (os.FileInfo, error) {
			return entryInfo{mode: fs.ModeDir | 0o755, mod: targetMod}, nil
		})
	if !got.ModTime().Equal(targetMod) {
		t.Errorf("ModTime() = %s, want the target's %s", got.ModTime(), targetMod)
	}
}
