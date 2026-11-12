package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// FormatFolding is folding as one letter a folder, in its order: s for
// CaseSensitive, i for CaseInsensitive and u for CaseUnknown. A data dir's
// record keeps it (backlog B741's round 2): keyed by each folder's whole
// path, as round 1 kept it, a record grew as the square of its data dir's
// depth, past what its reader takes at a depth an ordinary path reaches
// (Codex on #1441: 66,186 bytes for a path of 495 bytes in 240 folders),
// where one letter a folder grows with the path alone.
func FormatFolding(folding []CaseFolding) string {
	b := make([]byte, len(folding))
	for i, f := range folding {
		switch f {
		case CaseSensitive:
			b[i] = 's'
		case CaseInsensitive:
			b[i] = 'i'
		default:
			b[i] = 'u'
		}
	}
	return string(b)
}

// ParseFolding reads what FormatFolding writes, a letter it does not know as
// CaseUnknown.
func ParseFolding(s string) []CaseFolding {
	if s == "" {
		return nil
	}
	folding := make([]CaseFolding, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 's':
			folding[i] = CaseSensitive
		case 'i':
			folding[i] = CaseInsensitive
		}
	}
	return folding
}

// FoldingOnTheWay is how each folder on the way to the absolute path p
// compares the names in it, as probe answers (FoldingIn, but for a test's
// stand-in): one answer a folder, from p's volume's root down to p's own
// folder, the i-th the folder p's (i+1)-th name is looked up in (absElements:
// filepath.Join of p's first i+1 elements); nil for a path that is not
// absolute. A folder is asked where its names are compared, so the folder a
// volume is mounted on is asked of the mounted volume's names, never of its
// own name in the folder above. Each folder is asked once, from p's own up:
// asked keeps the answers given, keyed by the folder as p spells it, and a
// folder it holds is not asked again (a second spelling of one data dir
// shares the folders above where the two part).
//
// It is what SameAsRecorded compares a case-only difference by, read while
// the folders are there: once a volume is away every folder below its mount
// point is gone with it, and a probe then answers for the folder left at the
// mount point, on the volume above (backlog B741's gap 3).
func FoldingOnTheWay(p string, probe func(dir string) CaseFolding, asked map[string]CaseFolding) []CaseFolding {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return nil
	}
	e := absElements(p)
	folding := make([]CaseFolding, len(e)-1)
	for i := len(e) - 1; i >= 1; i-- {
		dir := filepath.Join(e[:i]...)
		f, ok := asked[dir]
		if !ok {
			f = probe(dir)
			asked[dir] = f
		}
		folding[i-1] = f
	}
	return folding
}

// SameAsRecorded reports whether the absolute path now names the absolute
// path recorded, by the folding recorded of recorded's folders
// (FoldingOnTheWay), probing nothing: element by element alike, or alike but
// for case where the folder the element is looked up in was not read as
// case-sensitive (one read as folding case, and one that could not tell,
// CaseUnknown: two spellings that may name one directory are taken for one,
// for a refusal that protects a data dir whose volume is away); a volume's
// root, which Windows never tells apart by case, alike but for case. A
// folding that does not hold one answer for each of recorded's folders (none,
// or one of another path) says nothing of any of them: each is CaseUnknown. A
// path that is not absolute is alike only as written.
func SameAsRecorded(recorded, now string, folding []CaseFolding) bool {
	recorded, now = filepath.Clean(recorded), filepath.Clean(now)
	if !filepath.IsAbs(recorded) || !filepath.IsAbs(now) {
		return recorded == now
	}
	r, n := absElements(recorded), absElements(now)
	if len(r) != len(n) || !strings.EqualFold(r[0], n[0]) {
		return false
	}
	folding = foldingOf(folding, len(r))
	for i := 1; i < len(r); i++ {
		if !alikeByRecord(folding, r, i, n[i]) {
			return false
		}
	}
	return true
}

// foldingOf is folding where it holds one answer for each folder a spelling
// of that many elements (absElements) passes through, and nil where it does
// not: a folding of another spelling, its resolution among them, or none,
// says nothing of any of them, each CaseUnknown.
func foldingOf(folding []CaseFolding, elements int) []CaseFolding {
	if len(folding) != elements-1 {
		return nil
	}
	return folding
}

// alikeByRecord reports whether name, looked up in the folder recorded's i-th
// element is looked up in (i at least 1, after the volume's root), names what
// that element names there by the record: the same name, or the same but for
// case where folding (foldingOf's of recorded) does not hold that folder as
// CaseSensitive.
func alikeByRecord(folding []CaseFolding, recorded []string, i int, name string) bool {
	switch {
	case recorded[i] == name:
		return true
	case !strings.EqualFold(recorded[i], name):
		return false
	}
	return folding == nil || folding[i-1] != CaseSensitive
}

// absElements is the clean absolute path p's elements: its volume's root
// first, as one element ("/", `C:\`, `\\server\share\`), then each name below
// it, so that filepath.Join of its first i elements is the folder its i-th is
// looked up in, as filepath.Dir spells it.
func absElements(p string) []string {
	vol := filepath.VolumeName(p)
	out := []string{vol + string(filepath.Separator)}
	for _, e := range strings.Split(p[len(vol):], string(filepath.Separator)) {
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// foldingProbeEntries is how much of a folder's listing FoldingIn reads
// first, and so how many of its entries it asks at most. A folder its first
// read was all of is read again for at most one entry more, which tells it
// still within the bound from one that grew past it; only one its first read
// was not all of is read whole again (listsSpellings).
const foldingProbeEntries = 256

// FoldingIn reports how the folder dir compares the names in it, judged in the
// folder itself: an entry of its listing whose name holds an ASCII letter is
// looked up again with the case of those letters swapped (Lstat), and the
// folder folds case when that finds the same entry, and does not when it finds
// nothing or another entry. The same file found is one entry only where the
// swapped spelling is no name of its own in the listing (a hard link's second
// name, on a case-sensitive volume, is one), by a listing read after that
// entry's own lookups, never one read before them: neither the probe's first
// read nor a re-read made for an entry asked earlier holds a second name made
// since (B738: a hard link FOLDER.JPG made beside folder.jpg between the first
// read and the lookups made a case-sensitive folder answer case-insensitive).
// The re-read takes at most foldingProbeEntries+1 entries of a folder its
// first read was all of, and one that finds more, the folder grown past that
// bound since, settles nothing, as one that cannot be read; a folder its first
// read was not all of is read whole again. A re-read answers only where it
// still holds the probed name: one that holds the swapped spelling alone, or
// neither, says the entry changed in between, and the next is asked (B651's
// tenth round), and one that holds the probed name alone answers only once a
// lookup of the swapped spelling after it still finds the file (B651's
// eleventh round); and an answer comes only from lookups that agree with
// themselves, the entry still the file first found, so an entry removed or
// saved over between the lookups, or one os.SameFile cannot open (Windows),
// passes the question to the next (B651's ninth round). An entry asked is
// looked up at most five times and the listing read again for it at most
// once, so a probe reads the listing at most foldingProbeEntries+1 times,
// however the folder changes under it. The names in a folder are compared by
// the volume the folder is on, which is not the volume of the folder's own
// name where a volume is mounted on it: an exFAT stick mounted on an ext4
// folder folds the names in it, while its mount point's name, in the folder
// above, is compared as ext4 compares (B651's seventh round, Codex on round
// 6, where the probe looked up the folder's own name in its parent).
//
// It writes nothing. A folder whose listing offers no such name (an empty one,
// a bare mount point among them, or one whose names hold no letter), and one
// that cannot be read or is not there, answer CaseUnknown, never a guess: a
// probe that made a name to look up would write into a library root, or a
// folder above one, which B651 forbids, on a volume that may be read-only,
// and the library watcher would see the write.
func FoldingIn(dir string) CaseFolding { return foldingIn(osFolding{}, dir) }

// foldingFS is what FoldingIn reads: the os package's calls, or a test's
// stand-in for a volume this host does not have.
type foldingFS interface {
	readDir(dir string, n int) ([]fs.DirEntry, error)
	lstat(name string) (fs.FileInfo, error)
	sameFile(a, b fs.FileInfo) bool
}

// osFolding is foldingFS over the os package. Its listing and lookups are
// where CodeQL's go/path-injection follows a client's path into FoldingIn:
// the scanner asks it only of a folder above a configured root or inside the
// root that holds the path, and it looks up no name but one the folder's own
// listing gives, as listed or with its letters' case swapped (manifest's
// Scanner.lstat says how that path is confined).
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
	// The entries asked are the first read's, which still offers what it read
	// where an error cut it short (readListing).
	entries, whole := readListing(fsys, dir, foldingProbeEntries)
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
			// second name is on a case-sensitive volume, and the listing that
			// says so is read again now, after this entry's lookups: the
			// first read, and a re-read made for an entry asked before, came
			// before them and need not hold a second name made since (B738,
			// Codex on #1397). A listing read again that no longer holds the
			// probed name says the entry changed since the lookups (renamed
			// to its other spelling, or gone), and tells nothing either way
			// (B651's tenth round, Codex and Claude on round 9). One that
			// lacks the swapped spelling answers only once a lookup of it
			// after the listing still finds the file: on a case-sensitive
			// volume a hard link's second name unlinked since the lookups is
			// gone from the listing and from a lookup alike, where a volume
			// that folds still finds the entry (B651's eleventh round, Codex
			// and Claude on round 10).
			hasName, hasSwapped, known := listsSpellings(fsys, dir, whole, name, swapped)
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

// listsSpellings reports whether the folder dir's listing, read again after
// the lookups it settles, holds name and swapped, each byte for byte, and
// whether that is known: read for at most foldingProbeEntries+1 entries where
// the probe's first read was all of it (small), and known only where it ends
// before that bound, a folder grown past it since never read further; read
// whole where the first read was not all of it; and not known where it cannot
// be read whole.
func listsSpellings(fsys foldingFS, dir string, small bool, name, swapped string) (hasName, hasSwapped, known bool) {
	n := -1
	if small {
		n = foldingProbeEntries + 1
	}
	read, whole := readListing(fsys, dir, n)
	if !whole {
		return false, false, false
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

// readListing reads at most n entries of the folder dir's listing, all of it
// where n is not positive, and reports whether they are the whole of it: a
// listing cut short by an error still offers what it read, and is not the
// whole of it; one that ends before n entries is, and so is one that ends
// with io.EOF, where nothing was left to read.
func readListing(fsys foldingFS, dir string, n int) ([]fs.DirEntry, bool) {
	entries, err := fsys.readDir(dir, n)
	return entries, err == nil && (n <= 0 || len(entries) < n) || n > 0 && errors.Is(err, io.EOF)
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
