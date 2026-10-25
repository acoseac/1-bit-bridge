package fsutil

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
)

// TestCheckResolvedDirRefusesOnlyALinkOrAMove: the directory a config is
// written in, as ResolveLinks answered it, is refused as a link still when its
// own entry is a name surrogate (a symbolic link, a junction or a mounted
// folder: IsReparseTagNameSurrogate's bit, not a list of two tags), or when
// its resolution, asked again, answers another directory, and as no
// directory; every other reparse point is a directory like any other, a
// OneDrive folder's cloud tag and a container layer's among them, and the
// check's own failures are its errors. Until B506's fifth round the check
// refused every reparse point os.Lstat calls ModeIrregular, so every writer
// of a config under a OneDrive folder on Windows was refused, and it never
// asked the resolution again.
func TestCheckResolvedDirRefusesOnlyALinkOrAMove(t *testing.T) {
	const dir, resolved = "cfg", "resolved"
	errEntry := errors.New("the entry cannot be read (for the test)")
	errAgain := errors.New("the directory cannot be resolved again (for the test)")
	for _, tc := range []struct {
		name string
		// tag, isDir and entryErr are what the directory's own entry reads
		// as; again and againErr what its resolution answers when asked
		// again.
		tag       uint32
		isDir     bool
		entryErr  error
		again     string
		againErr  error
		want      error
		namesPath bool
	}{
		{name: "a plain directory", isDir: true, again: resolved},
		{name: "a junction or a mounted folder", tag: reparseTagMountPoint, isDir: true, again: resolved, want: errLinkNotResolved, namesPath: true},
		{name: "a symbolic link", tag: reparseTagSymlink, isDir: true, again: resolved, want: errLinkNotResolved, namesPath: true},
		{name: "a WSL symbolic link", tag: 0xA000001D, isDir: true, again: resolved, want: errLinkNotResolved, namesPath: true},
		{name: "a OneDrive folder", tag: 0x9000101A, isDir: true, again: resolved},
		{name: "a cloud placeholder folder", tag: 0x9000001A, isDir: true, again: resolved},
		{name: "a container layer", tag: 0x80000018, isDir: true, again: resolved},
		{name: "a deduplicated folder", tag: reparseTagDedup, isDir: true, again: resolved},
		{name: "a directory whose resolution moves", isDir: true, again: "elsewhere", want: errLinkNotResolved, namesPath: true},
		{name: "a OneDrive folder whose resolution moves", tag: 0x9000101A, isDir: true, again: "elsewhere", want: errLinkNotResolved, namesPath: true},
		{name: "no directory", again: resolved, want: syscall.ENOTDIR, namesPath: true},
		{name: "an entry that cannot be read", entryErr: errEntry, want: errEntry},
		{name: "a resolution that fails when asked again", isDir: true, againErr: errAgain, want: errAgain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResolvedDir(dir, resolved, dirEntryOps{
				entry: func(p string) (uint32, bool, error) {
					if p != resolved {
						t.Errorf("the check read the entry of %s, want %s's", p, resolved)
					}
					return tc.tag, tc.isDir, tc.entryErr
				},
				resolve: func(p string) (string, error) {
					if p != resolved {
						t.Errorf("the check resolved %s again, want %s", p, resolved)
					}
					return tc.again, tc.againErr
				},
			})
			switch {
			case tc.want == nil && err != nil:
				t.Errorf("checkResolvedDir = %v, want a directory", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("checkResolvedDir = %v, want %v", err, tc.want)
			}
			var pe *fs.PathError
			if tc.namesPath && (!errors.As(err, &pe) || pe.Path != dir) {
				t.Errorf("checkResolvedDir = %v, want an *fs.PathError naming %s", err, dir)
			}
		})
	}
}
