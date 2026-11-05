package manifest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Library roots may nest: neither the config's validation nor the console's
// or `bridge library add`'s root add refuses a root configured inside another
// (B651's fifth round, CodeRabbit on #1397). A path under a nested root is the
// nested root's, the deepest root that holds it, whatever order the roots are
// listed in: the watcher's events, the looks its dispatches make and the scans
// it drops under a root it holds (rootOf), and a subtree scan's owning root
// (ScanSubtree), all go through deepestRootOf. Before, each took the first
// root listed that held the path, so with the outer root listed first a nested
// root's events, held or not, went to the outer one, and so did its subtree
// scans, indexing its files under the outer root's path. A walk of the outer
// root still passes through a nested root, as it always has, unless the nested
// root reads as a volume that is not mounted (protectNestedRoots).
//
// A path is compared with a root element by element, and an element that
// differs in case alone is judged by the folder it is looked up in, as that
// folder compares names (fsutil.FoldingIn: Sensitive, Insensitive or Unknown;
// B651's sixth and seventh rounds, Codex on rounds 5 and 6): a volume mounted
// inside another compares the names in it as it does, and Windows can make a
// single folder case-sensitive. The two questions asked count Unknown in
// opposite ways (Argon on the seventh round's brief). Which root a path is
// (caseLook.sameName) folds an element only where its folder is established to
// fold case, and never on a lookup there that failed (B651's eighth round) or
// that found one spelling alone (the ninth), so two roots spelled alike are
// never taken for one and a subtree scan is never sent into another root's
// folder. What a scan spares and a walk passes over
// (caseLook.mayBeOneName) folds an element wherever its folder may fold, so the
// rows of a nested root whose volume went are spared in every spelling the
// walks may have given them.

// foldingIn is how the folder dir compares the names in it
// (fsutil.FoldingIn), or a test's stand-in for it (foldingHook).
func (s *Scanner) foldingIn(dir string) fsutil.CaseFolding {
	if h := s.foldingHook; h != nil {
		return h(dir)
	}
	return fsutil.FoldingIn(dir)
}

// lstat is what a lookup of name finds, for how two spellings compare
// (os.Lstat), or a test's stand-in for it (lookupHook), which describes the
// volume foldingHook does.
func (s *Scanner) lstat(name string) (fs.FileInfo, error) {
	if h := s.lookupHook; h != nil {
		return h(name)
	}
	return os.Lstat(name)
}

// caseLook compares paths as their folders compare names, for the length of
// one task (a lookup of a path's root, a scan's protection, a hold's release),
// asking each folder once.
type caseLook struct {
	s    *Scanner
	seen map[string]fsutil.CaseFolding
}

func (s *Scanner) caseLook() *caseLook { return &caseLook{s: s} }

// folding is foldingIn of dir, asked once per caseLook.
func (c *caseLook) folding(dir string) fsutil.CaseFolding {
	dir = filepath.Clean(dir)
	if f, ok := c.seen[dir]; ok {
		return f
	}
	f := c.s.foldingIn(dir)
	if c.seen == nil {
		c.seen = make(map[string]fsutil.CaseFolding)
	}
	c.seen[dir] = f
	return f
}

// sameName reports whether x and y, names that differ in case alone, name one
// entry of the folder parent, for which root a path is: only where the folder
// is established to fold case (an Unknown answer keeps them two) and the
// lookups of both find one file, or neither finds anything and the folder's
// answer decides. One that finds a file where the other finds nothing is an
// answer a folder that folds that name never gives (Claude on round 8: a
// volume may fold ASCII case and not another script's), and a lookup that
// fails otherwise (a permission refused, a transient error) cannot tell
// (Codex on round 7): both keep them two, since taking them for one would
// route a scan to a root the path may not be in. Every lookup here is the
// scanner's (Scanner.lstat), so a test's stand-in volume answers it.
func (c *caseLook) sameName(parent, x, y string) bool {
	if c.folding(parent) != fsutil.CaseInsensitive {
		return false
	}
	fx, errx := c.s.lstat(filepath.Join(parent, x))
	fy, erry := c.s.lstat(filepath.Join(parent, y))
	switch {
	case errx == nil && erry == nil:
		return os.SameFile(fx, fy)
	case errors.Is(errx, fs.ErrNotExist) && errors.Is(erry, fs.ErrNotExist):
		return true
	}
	return false
}

// mayBeOneName reports whether a name that differs from x in case alone may
// name the entry x names in the folder parent, for what a scan spares and a
// walk passes over: unless the folder answers that it is case-sensitive and is
// the folder the rows were written through, which it is where it holds x,
// another spelling of x, or library content. A folder that answers Unknown or
// is not there may fold, and so may one that holds none of these: a mount
// point a volume left bare answers for the folder it covers, not for the
// volume that was mounted on it (Codex on round 6).
func (c *caseLook) mayBeOneName(parent, x string) bool {
	if c.folding(parent) != fsutil.CaseSensitive {
		return true
	}
	if _, err := c.s.lstat(filepath.Join(parent, x)); err == nil {
		return false
	}
	return !holdsSpellingOrContent(parent, x)
}

// listedName is name as the folder dir lists it, for the path a walk reaches
// an entry of dir by: name itself where the listing holds it byte for byte,
// else a listed name the folder takes for it, as which root a path is decides
// (sameName: a folder established to fold case, the lookups of both finding
// one entry; a name in another case alone is a candidate, never the answer),
// else name. A listing that cannot be read leaves name as it is.
func (c *caseLook) listedName(dir, name string) string {
	f, err := fsutil.OpenDir(dir)
	if err != nil {
		return name
	}
	entries, _ := f.ReadDir(-1)
	_ = f.Close()
	for _, e := range entries {
		if e.Name() == name {
			return name
		}
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) && c.sameName(dir, name, e.Name()) {
			return e.Name()
		}
	}
	return name
}

// holdsSpellingOrContent reports whether the folder dir holds a spelling of x
// in another case, or library content (isLibraryEntry), reading its listing
// rootLookBatch entries at a time; false where it cannot be read.
func holdsSpellingOrContent(dir, x string) bool {
	f, err := fsutil.OpenDir(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(rootLookBatch)
		for _, e := range entries {
			if e.Name() != x && strings.EqualFold(e.Name(), x) {
				return true
			}
		}
		if holdsLibraryContent(dir, entries) {
			return true
		}
		if err != nil {
			return false // the listing's end (io.EOF), or a failed read
		}
	}
}

// under reports whether abs is at or under the folder dir, and abs's own
// spelling of the path below it ("." for dir itself), element by element: one
// that differs from dir's in case alone matches where the folder it is looked
// up in, in abs's spelling, names one entry by both, established (sameName),
// or, for what is spared (spare), may (mayBeOneName).
func (c *caseLook) under(dir, abs string, spare bool) (string, bool) {
	d, a := pathElements(dir), pathElements(abs)
	if len(a) < len(d) {
		return "", false
	}
	for i := range d {
		if d[i] == a[i] {
			continue
		}
		if !strings.EqualFold(d[i], a[i]) {
			return "", false
		}
		if i == 0 {
			// A volume, a drive letter or a share, which Windows never tells
			// apart by case.
			continue
		}
		parent := filepath.Join(a[:i]...)
		if spare && !c.mayBeOneName(parent, d[i]) || !spare && !c.sameName(parent, d[i], a[i]) {
			return "", false
		}
	}
	if len(a) == len(d) {
		return ".", true
	}
	return filepath.Join(a[len(d):]...), true
}

// spareFolds is, for each element of tail (a path below the folder dir, in a
// configured root's spelling), whether a walk's spelling of it may differ in
// case alone (mayBeOneName in the folder it is looked up in): each element is
// judged by its own folder, so a volume that folds case mounted inside one
// that does not, or the other way round, is judged as it compares names
// (Argon on the seventh round's brief).
func (c *caseLook) spareFolds(dir, tail string) []bool {
	elems := strings.Split(tail, string(filepath.Separator))
	folds := make([]bool, len(elems))
	parent := dir
	for i, e := range elems {
		folds[i] = c.mayBeOneName(parent, e)
		parent = filepath.Join(parent, e)
	}
	return folds
}

// spelledAs reports whether the elements got are want's, each the same or,
// where folds says it may be spelled otherwise, the same with case folded.
func spelledAs(got, want []string, folds []bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] && (!folds[i] || !strings.EqualFold(got[i], want[i])) {
			return false
		}
	}
	return true
}

// pathElements is a path's elements, cleaned: its volume and leading
// separator first, as one element ("/" or `C:\`), where it has them
// (volumeElements).
func pathElements(p string) []string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	return volumeElements(vol, p[len(vol):], filepath.Separator)
}

// volumeElements is pathElements over a clean path split into its volume name
// vol and the rest of it, with the separator sep: apart from the host's path
// rules, so that a test on any host can hand it Windows' volumes. A volume
// longer than a drive letter's two characters, a share's (\\server\share) or
// one in the \\?\ namespace, names its root with nothing after it, as Go's
// filepath.Rel takes it: filepath.Clean leaves \\server\share bare while every
// path below it begins \\server\share\, and a root configured there without
// its separator held none of them (B651's ninth round, Codex on round 8). C:
// alone, a drive's current folder, which filepath.Clean spells C:., stays C:.
func volumeElements(vol, rest string, sep byte) []string {
	s := string(sep)
	var out []string
	switch {
	case strings.HasPrefix(rest, s):
		out = append(out, vol+s)
		rest = rest[1:]
	case len(vol) > 2 && rest == "":
		out = append(out, vol+s)
	case vol != "":
		out = append(out, vol)
	}
	if rest != "" && rest != "." {
		out = append(out, strings.Split(rest, s)...)
	}
	return out
}

// deepestRootOf returns the configured root abs is at or under, the deepest
// where roots nest, as configured and as filepath.Abs gives it, and abs's own
// spelling of the path below it (caseLook.under, as which root a path is
// compares: never by filepath.Rel, which on Windows folds case in every
// folder, a case-sensitive one included; Codex on round 6); "" for none. abs
// must be absolute.
func (s *Scanner) deepestRootOf(roots []string, abs string) (root, absRoot, tail string) {
	c := s.caseLook()
	depth := -1
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		t, ok := c.under(a, abs, false)
		if !ok {
			continue
		}
		// Every root that holds abs is abs or above it, so of two such roots
		// the one with more elements is the deeper.
		if d := len(pathElements(a)); d > depth {
			root, absRoot, tail, depth = r, a, t, d
		}
	}
	return root, absRoot, tail
}

// rootOf returns the configured root name is at or under, the deepest where
// roots nest (deepestRootOf, the containment ScanSubtree takes its owning root
// by), and whether name is that root itself; "" when it is under none.
func (s *Scanner) rootOf(name string) (root string, itself bool) {
	return s.rootIn(s.Roots(), name)
}

// rootIn is rootOf over roots, for a caller that asks of many paths.
func (s *Scanner) rootIn(roots []string, name string) (root string, itself bool) {
	absName, err := filepath.Abs(name)
	if err != nil {
		return "", false
	}
	root, _, tail := s.deepestRootOf(roots, absName)
	return root, root != "" && tail == "."
}

// nestedRoot is a configured root strictly below another: as configured, as
// filepath.Abs gives it, and its path below the other root, in its own
// spelling.
type nestedRoot struct {
	root, abs, tail string
}

// nestedRootsUnder lists the configured roots that are, or may be, strictly
// below root (caseLook.under, as what is spared compares), which a walk of
// root reaches as folders of its own; nil when there is none, which is every
// library but one with nested roots.
func (s *Scanner) nestedRootsUnder(roots []string, root string) []nestedRoot {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	c := s.caseLook()
	var out []nestedRoot
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if tail, ok := c.under(absRoot, a, true); ok && tail != "." {
			out = append(out, nestedRoot{root: r, abs: a, tail: tail})
		}
	}
	return out
}

// nestedPass is the folders of nested roots a walk passes over.
type nestedPass struct {
	folders []sparedFolder
}

// sparedFolder is a nested root's folder: its path, its elements, and for each
// whether a walk's spelling of it may differ in case alone (spareFolds; the
// walked root's own never does).
type sparedFolder struct {
	path  string
	elems []string
	folds []bool
}

// has reports whether the folder at abs, which a walk reaches, is one of them.
func (p nestedPass) has(abs string) bool {
	if len(p.folders) == 0 {
		return false
	}
	abs = filepath.Clean(abs)
	for _, f := range p.folders {
		if abs == f.path {
			return true
		}
		// Both are clean, so the whole of each folds as each element does: a
		// path this does not match matches no spelling of the folder.
		if strings.EqualFold(abs, f.path) && spelledAs(pathElements(abs), f.elems, f.folds) {
			return true
		}
	}
	return false
}

// protectNestedRoots decides, before a walk of root, which of the roots nested
// in it read as a volume not mounted (Scanner.lookAtRoot, the look the
// watcher's hold and their own scans' guards read), whether or not a walk
// could reach their folders: an unmount above a nested root takes its folder
// with it, and a walk that only spared what it reached spared nothing then
// (B651's sixth round, Codex on round 5). Each one's prefix below root is
// recorded as a subtree the walk could not see, so the deletion pass leaves
// every row under it as it is, its missing count untouched, and so is every
// spelling of that prefix the index holds that may be the same folder, each
// element judged by its own folder (spareFolds): the walks wrote the rows as
// the volume spells its folders, which the configured spelling may not be. It
// returns their folders, for the walk to pass over where it reaches them in
// any such spelling. A nested root that reads as it is is walked, as before.
func (s *Scanner) protectNestedRoots(ctx context.Context, root string, nested []nestedRoot, multiRoot bool, rows []map[string]struct{}, errorSubtrees map[string]struct{}) nestedPass {
	var pass nestedPass
	if len(nested) == 0 {
		return pass
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return pass
	}
	rootElems := len(pathElements(absRoot))
	c := s.caseLook()
	for _, n := range nested {
		if !s.lookAtRoot(ctx, n.root).absent {
			continue
		}
		folder := filepath.Join(absRoot, n.tail)
		prefix := relPath(absRoot, folder, multiRoot)
		errorSubtrees[prefix] = struct{}{}
		folds := c.spareFolds(absRoot, n.tail)
		pass.folders = append(pass.folders, sparedFolder{
			path:  folder,
			elems: pathElements(folder),
			folds: append(make([]bool, rootElems), folds...),
		})
		for _, set := range rows {
			protectSpellings(prefix, folds, set, errorSubtrees)
		}
	}
	return pass
}

// protectSpellings records, as a subtree the walk could not see, every
// spelling of prefix that a path of rows begins with, the prefix's last
// len(folds) elements each the same or, where folds says it may be spelled
// otherwise, the same with case folded, and the elements before them (the
// root's own name) the same.
func protectSpellings(prefix string, folds []bool, rows map[string]struct{}, errorSubtrees map[string]struct{}) {
	if !anyFolds(folds) {
		return
	}
	want := strings.Split(prefix, "/")
	all := append(make([]bool, len(want)-len(folds)), folds...)
	for p := range rows {
		got := strings.SplitN(p, "/", len(want)+1)
		if len(got) < len(want) {
			continue
		}
		if spelledAs(got[:len(want)], want, all) {
			errorSubtrees[strings.Join(got[:len(want)], "/")] = struct{}{}
		}
	}
}

// anyFolds reports whether any element may be spelled otherwise.
func anyFolds(folds []bool) bool {
	for _, f := range folds {
		if f {
			return true
		}
	}
	return false
}
