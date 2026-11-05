package fsutil

import (
	"errors"
	"io"
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
// nothing or another entry. The same file found is one entry only where the
// swapped spelling is no name of its own in the listing (a hard link's second
// name, on a case-sensitive volume, is one), read whole for it where the
// probe's first read was not, and then only from a re-read that still holds
// the probed name: one that holds the swapped spelling alone, or neither, says
// the entry changed in between, and the next is asked (B651's tenth round);
// and an answer comes only from lookups that agree
// with themselves, the entry still the file first found, so an entry removed
// or saved over between the lookups, or one os.SameFile cannot open (Windows),
// passes the question to the next (B651's ninth round). The names in a folder
// are compared by the volume the folder is on, which is not the volume of the
// folder's own name where a volume is mounted on it: an exFAT stick mounted on
// an ext4 folder folds the names in it, while its mount point's name, in the
// folder above, is compared as ext4 compares (B651's seventh round, Codex on
// round 6, where the probe looked up the folder's own name in its parent).
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
	// A listing cut short by an error still offers what it read, and is not
	// the whole of it; one that ends before the batch is full is.
	entries, err := fsys.readDir(dir, foldingProbeEntries)
	whole := err == nil && len(entries) < foldingProbeEntries || errors.Is(err, io.EOF)
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
		other, otherErr := fsys.lstat(filepath.Join(dir, swapped))
		// Only lookups that agree with themselves answer: the entry is still
		// the file first found, as the other spelling is where it was found.
		// One removed, or replaced by a save, between the lookups says nothing
		// of how the folder compares names, and neither does a file
		// os.SameFile cannot open, which it answers false for on Windows
		// (B651's ninth round, Codex and Claude on round 8).
		if !stillFound(fsys, dir, name, mine) {
			continue
		}
		switch {
		case otherErr == nil:
			if !stillFound(fsys, dir, swapped, other) {
				continue
			}
			if !fsys.sameFile(mine, other) {
				return CaseSensitive // both spellings there, as two entries
			}
			// One file under both spellings is one entry unless the other
			// spelling is a name of its own in the listing, as a hard link's
			// second name is on a case-sensitive volume. A listing read again
			// that no longer holds the probed name says the entry changed
			// since the lookups (renamed to its other spelling, or gone), and
			// tells nothing either way (B651's tenth round, Codex and Claude
			// on round 9). One that lacks the swapped spelling answers only
			// once a lookup of it after the listing still finds the file: on
			// a case-sensitive volume a hard link's second name unlinked
			// since the lookups is gone from the listing and from a lookup
			// alike, where a volume that folds still finds the entry (B651's
			// eleventh round, Codex and Claude on round 10).
			hasName, hasSwapped, known := listsSpellings(fsys, dir, entries, whole, name, swapped)
			switch {
			case !known || !hasName:
				continue
			case hasSwapped:
				return CaseSensitive
			case !stillFound(fsys, dir, swapped, other):
				continue
			}
			return CaseInsensitive
		case errors.Is(otherErr, fs.ErrNotExist):
			return CaseSensitive
		}
		// Any other error says nothing of how the folder compares names.
	}
	return CaseUnknown
}

// stillFound reports whether a lookup of name in dir again finds the file
// first found.
func stillFound(fsys foldingFS, dir, name string, first fs.FileInfo) bool {
	again, err := fsys.lstat(filepath.Join(dir, name))
	return err == nil && fsys.sameFile(first, again)
}

// listsSpellings reports whether the folder dir's listing holds name and
// swapped, each byte for byte, and whether that is known: read from the
// entries the probe read where they are the whole listing (whole), which
// hold name, else from the whole listing read again, which may no longer, and
// not known where it cannot be read whole.
func listsSpellings(fsys foldingFS, dir string, read []fs.DirEntry, whole bool, name, swapped string) (hasName, hasSwapped, known bool) {
	if !whole {
		all, err := fsys.readDir(dir, -1)
		if err != nil {
			return false, false, false
		}
		read = all
	}
	for _, e := range read {
		switch e.Name() {
		case name:
			hasName = true
		case swapped:
			hasSwapped = true
		}
	}
	return hasName, hasSwapped, true
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
