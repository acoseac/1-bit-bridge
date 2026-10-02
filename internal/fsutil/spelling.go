package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// errNotSpelled is wrapped by every refusal of Speller.Spell: rel names no
// entry, names one through something a walk from the root does not descend
// (a link to a directory, a junction, a file), or names one that no entry of
// its directory's listing turns out to be.
var errNotSpelled = errors.New("no on-disk spelling")

// Speller reads how paths below one directory are spelled on disk: Spell
// answers a slash-separated path below root with every component spelled as
// the directory holding it lists it, which is the spelling a walk from root
// hands the scanner for the entry the path opens.
//
// It exists because the two differ wherever a volume opens a name under more
// than one spelling (APFS, case- and normalization-insensitive; NTFS and SMB,
// case-insensitive), and the scanner makes each row's path from the spelling
// of the directory it is handed (ScanSubtree). An upload commit handed the
// scan the folder as the client spelled it, so an upload into "Café" sent in
// NFD, or into "artist/ALBUM", landed in the existing folder and its scan
// indexed every file there a second time under the client's spelling, until
// the next full scan (backlog B219). A path a client sends is mapped here
// before anything is scanned or retired under it.
//
// Each component is matched by IDENTITY, never by a fold of its name: the
// entry the listing names exactly, else the one os.SameFile says the
// client's spelling opens, both sides Lstat'd so a link is matched as the
// link. A fold of case and Unicode composition only picks the candidates
// worth a stat, the enricher's rule (relaxations in the query, strictness in
// the acceptance): a name the fold does not relate to the client's spelling,
// a Windows short name say, is still found, by identity, over the whole
// listing. A rule of names alone would call "artist" and "Artist" one folder
// on a case-sensitive volume, where they are two.
//
// Every component above the last must be a directory, not a link to one and
// not a junction (fs.FileInfo.IsDir from Lstat): a walk from root descends
// neither, so a path through one has no on-disk spelling, and the resolution
// never lists a directory outside root. The last component is spelled
// whatever it is.
//
// A Speller remembers each directory it has resolved above a path's last
// component, so a batch of paths in one folder lists the folders above it
// once; the folder holding a path's last component is listed for each path,
// as far as that component's name. Make one per batch: what it remembers is
// what each directory listed when it was read. It is not safe
// for concurrent use, which is also why it holds its own case folder: an
// x/text Caser is stateful and must not be shared between goroutines.
type Speller struct {
	root string
	sys  spellingSys
	fold cases.Caser
	dirs map[string]string // a directory below root, as asked → as listed
}

// NewSpeller returns a Speller for the paths below root.
func NewSpeller(root string) *Speller {
	return newSpeller(root, osSpelling{})
}

func newSpeller(root string, sys spellingSys) *Speller {
	return &Speller{root: root, sys: sys, fold: cases.Fold(), dirs: map[string]string{}}
}

// Spell returns rel as the listings below root spell it. rel is
// slash-separated and relative to root; "" and "." are root itself and come
// back as they are. A rel whose components are not each one name (plainName),
// an absolute one included, is refused: the answer must name an entry below
// root. A refusal wraps errNotSpelled and names the path library-relative,
// never absolute.
func (s *Speller) Spell(rel string) (string, error) {
	if rel == "" || rel == "." {
		return rel, nil
	}
	segs := strings.Split(rel, "/")
	for _, seg := range segs {
		if !plainName(seg) {
			return "", fmt.Errorf("%w: %q is not a path below the root", errNotSpelled, rel)
		}
	}
	spelled := ""
	for i := range segs {
		var err error
		if spelled, err = s.spellNext(spelled, segs[:i+1], i == len(segs)-1); err != nil {
			return "", err
		}
	}
	return spelled, nil
}

// spellNext spells the last component of asked inside spelled, the spelling
// of the components before it, and returns the path spelled so far. A
// component above the last must be a directory, and is remembered; a
// remembered directory answers for itself as a last component too.
func (s *Speller) spellNext(spelled string, asked []string, last bool) (string, error) {
	key := strings.Join(asked, "/")
	if known, ok := s.dirs[key]; ok {
		return known, nil
	}
	parent := filepath.Join(s.root, filepath.FromSlash(spelled))
	name, info, err := s.spellComponent(parent, asked[len(asked)-1])
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", errNotSpelled, key, err)
	}
	if spelled != "" {
		name = spelled + "/" + name
	}
	if !last {
		if !info.IsDir() {
			return "", fmt.Errorf("%w: %q is not a directory a walk descends into", errNotSpelled, key)
		}
		s.dirs[key] = name
	}
	return name, nil
}

// plainName reports whether seg names one entry of a directory: not empty,
// "." or "..", and holding nothing the OS reads as a separator or a volume
// (on Windows a backslash, or "C:"), nor a NUL. filepath.Join would read such
// a component as a path, which can name an entry outside the directory.
func plainName(seg string) bool {
	return seg != "" && seg != "." && seg != ".." &&
		!strings.ContainsRune(seg, filepath.Separator) && !strings.ContainsRune(seg, 0) &&
		filepath.VolumeName(seg) == ""
}

// spellComponent answers how dir lists the entry dir/seg opens, with that
// entry's Lstat. It reads the listing once, and only as far as an entry
// named exactly seg, which is the common answer.
func (s *Speller) spellComponent(dir, seg string) (string, fs.FileInfo, error) {
	sys := s.sys
	want, err := sys.lstat(filepath.Join(dir, seg))
	if err != nil {
		return "", nil, pathErrCause(err)
	}
	exact := false
	var listed []string
	err = sys.names(dir, func(name string) bool {
		if name == seg {
			exact = true
			return false
		}
		listed = append(listed, name)
		return true
	})
	if err != nil {
		return "", nil, pathErrCause(err)
	}
	if exact {
		return seg, want, nil
	}
	key := spellingFold(s.fold, seg)
	var candidates []string
	for _, name := range listed {
		if spellingFold(s.fold, name) == key {
			candidates = append(candidates, name)
		}
	}
	if name, info, ok := sameEntry(sys, dir, want, candidates); ok {
		return name, info, nil
	}
	// No name the fold relates to seg is the entry seg opens: the volume
	// relates names the fold does not (a Windows short name). Every entry,
	// by identity.
	if name, info, ok := sameEntry(sys, dir, want, listed); ok {
		return name, info, nil
	}
	return "", nil, errors.New("no entry of its directory's listing is the entry it opens")
}

// sameEntry returns the first of names whose Lstat is the entry want is.
func sameEntry(sys spellingSys, dir string, want fs.FileInfo, names []string) (string, fs.FileInfo, bool) {
	for _, name := range names {
		info, err := sys.lstat(filepath.Join(dir, name))
		if err == nil && sys.sameFile(want, info) {
			return name, info, true
		}
	}
	return "", nil, false
}

// spellingFold is the candidate key: Unicode case folding by fold, then NFC.
// Two names a volume opens as one usually share it; whether they ARE one is
// decided by identity, never by this.
func spellingFold(fold cases.Caser, name string) string {
	return norm.NFC.String(fold.String(name))
}

// pathErrCause drops the absolute path an *fs.PathError carries, so an error
// Spell returns names its path library-relative alone (a log line names a
// library file library-relative).
func pathErrCause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// spellingSys is what a Speller asks of the filesystem: a seam, so the
// matching can be driven on any platform over a volume that folds names as
// APFS does (spelling_test.go).
type spellingSys interface {
	lstat(name string) (fs.FileInfo, error)
	// names hands fn each name dir lists, in the listing's order, until fn
	// returns false.
	names(dir string, fn func(name string) bool) error
	sameFile(a, b fs.FileInfo) bool
}

type osSpelling struct{}

func (osSpelling) lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func (osSpelling) sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// names reads the listing in batches and in the directory's own order, so
// the common answer (the client's spelling IS the entry's name) stops at
// that entry rather than reading and sorting a library root's every name.
// Opened with OpenDir: a component replaced by a named pipe is refused
// rather than waited on.
func (osSpelling) names(dir string, fn func(name string) bool) error {
	f, err := OpenDir(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		batch, err := f.Readdirnames(256)
		for _, name := range batch {
			if !fn(name) {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
