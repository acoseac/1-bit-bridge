package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// CaseFolding is how a folder compares the names in it: whether two spellings
// of a name that differ in case alone name one entry there.
type CaseFolding int

const (
	// CaseUnknown: nothing read in the folder could tell, or the folder could
	// not be read. The caller decides which way that counts, and the two ways
	// differ: taking two folders for one is safe for what a scan spares and
	// unsafe for which root a path is, and the other way round for taking one
	// folder for two.
	CaseUnknown CaseFolding = iota
	// CaseSensitive: a name in the folder spelled in another case names
	// nothing there, or another entry.
	CaseSensitive
	// CaseInsensitive: a name in the folder spelled in another case names the
	// same entry.
	CaseInsensitive
)

func (c CaseFolding) String() string {
	switch c {
	case CaseSensitive:
		return "case-sensitive"
	case CaseInsensitive:
		return "case-insensitive"
	}
	return "unknown"
}

// foldingProbeEntries is how much of a folder's listing FoldingIn reads at
// most.
const foldingProbeEntries = 256

// FoldingIn reports how the folder dir compares the names in it, judged in the
// folder itself: an entry of its listing whose name holds an ASCII letter is
// looked up again with the case of those letters swapped (Lstat), and the
// folder folds case when that finds the same entry, and does not when it finds
// nothing or another entry. The names in a folder are compared by the volume
// the folder is on, which is not the volume of the folder's own name where a
// volume is mounted on it: an exFAT stick mounted on an ext4 folder folds the
// names in it, while its mount point's name, in the folder above, is compared
// as ext4 compares (B651's seventh round, Codex on round 6, where the probe
// looked up the folder's own name in its parent).
//
// It writes nothing. A folder whose listing offers no such name (an empty one,
// a bare mount point among them, or one whose names hold no letter), and one
// that cannot be read or is not there, answer CaseUnknown, never a guess: a
// probe that made a name to look up would write into a library root, which
// B651 forbids, on a volume that may be read-only, and the library watcher
// would see the write.
func FoldingIn(dir string) CaseFolding { return foldingIn(osFolding{}, dir) }

// foldingFS is what FoldingIn reads: the os package's calls, or a test's
// stand-in for a volume this host does not have.
type foldingFS interface {
	readDir(dir string, n int) ([]fs.DirEntry, error)
	lstat(name string) (fs.FileInfo, error)
	sameFile(a, b fs.FileInfo) bool
}

// osFolding is foldingFS over the os package.
type osFolding struct{}

func (osFolding) readDir(dir string, n int) ([]fs.DirEntry, error) {
	f, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(n)
}

func (osFolding) lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func (osFolding) sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// foldingIn is FoldingIn over fsys.
func foldingIn(fsys foldingFS, dir string) CaseFolding {
	// A listing cut short by an error still offers what it read.
	entries, _ := fsys.readDir(dir, foldingProbeEntries)
	for _, e := range entries {
		name := e.Name()
		swapped, ok := swapASCIICase(name)
		if !ok {
			continue
		}
		mine, err := fsys.lstat(filepath.Join(dir, name))
		if err != nil {
			continue // gone since the listing, or not to be read: ask another
		}
		other, err := fsys.lstat(filepath.Join(dir, swapped))
		switch {
		case err == nil:
			if fsys.sameFile(mine, other) {
				return CaseInsensitive
			}
			return CaseSensitive // both spellings there, as two entries
		case errors.Is(err, fs.ErrNotExist):
			return CaseSensitive
		}
		// Any other error says nothing of how the folder compares names.
	}
	return CaseUnknown
}

// swapASCIICase is name with the case of its ASCII letters swapped: upper
// where it holds a lower-case one, else lower; false where it holds no ASCII
// letter. ASCII alone, since every volume that folds case folds those alike,
// where Unicode's case rules differ from one volume to the next.
func swapASCIICase(name string) (string, bool) {
	b := []byte(name)
	upper := false
	for _, c := range b {
		if 'a' <= c && c <= 'z' {
			upper = true
			break
		}
	}
	changed := false
	for i, c := range b {
		switch {
		case upper && 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
			changed = true
		case !upper && 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
			changed = true
		}
	}
	return string(b), changed
}
