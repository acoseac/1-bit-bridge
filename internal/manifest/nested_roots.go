package manifest

import (
	"context"
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
// (ScanSubtree), all go through deepestRootOf. Before, each took the first root listed that held the path,
// so with the outer root listed first a nested root's events, held or not,
// went to the outer one, and so did its subtree scans, indexing its files
// under the outer root's path. A walk of the outer root still passes through
// a nested root, as it always has, unless the nested root reads as a volume
// that is not mounted (protectNestedRoots).
//
// Every comparison of a path with a root folds case where the volume does
// (underRoot, B651's sixth round, Codex on round 5): on Windows' volumes and
// macOS's default one a root configured as C:\music\USB is the folder the
// volume spells usb, and the walks, which spell a folder as the volume lists
// it, would miss it by its configured spelling. Whether the volume folds case
// is the volume's own answer (foldsCase), and never taken as case-sensitive
// for a root that is not there: its nearest ancestor that is answers for it.

// foldsCase reports whether the volume holding path, or the nearest ancestor
// of path that exists, folds case (fsutil.FoldsCase): probed on the volume, so
// a path that is not there is judged by the folder it would be in, and the
// platform's default answers only where nothing there can be probed.
func (s *Scanner) foldsCase(path string) bool {
	if h := s.foldsCaseHook; h != nil {
		return h(path)
	}
	return fsutil.FoldsCase(path)
}

// underRoot reports whether abs is at or under the folder dir, and abs's own
// spelling of the path below it ("." for dir itself): by filepath.Rel, or,
// where the two differ in case alone and the volume folds case (foldsCase of
// abs, asked only then), element by element with case folded.
func (s *Scanner) underRoot(dir, abs string) (tail string, ok bool) {
	if rel, err := filepath.Rel(dir, abs); err == nil && !climbsOut(rel) {
		return rel, true
	}
	tail, ok = foldedTail(dir, abs)
	if !ok || !s.foldsCase(abs) {
		return "", false
	}
	return tail, true
}

// climbsOut reports whether a filepath.Rel answer leaves the folder it was
// asked from.
func climbsOut(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// foldedTail reports whether abs is at or under dir with case folded, element
// by element, and abs's elements below dir, joined ("." for dir itself).
func foldedTail(dir, abs string) (string, bool) {
	d, a := pathElements(dir), pathElements(abs)
	if len(a) < len(d) {
		return "", false
	}
	for i := range d {
		if !strings.EqualFold(d[i], a[i]) {
			return "", false
		}
	}
	if len(a) == len(d) {
		return ".", true
	}
	return filepath.Join(a[len(d):]...), true
}

// pathElements is a path's elements, cleaned: its volume and leading
// separator first, as one element ("/" or `C:\`), where it has them.
func pathElements(p string) []string {
	p = filepath.Clean(p)
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	var out []string
	switch {
	case strings.HasPrefix(rest, string(filepath.Separator)):
		out = append(out, vol+string(filepath.Separator))
		rest = rest[1:]
	case vol != "":
		out = append(out, vol)
	}
	if rest != "" && rest != "." {
		out = append(out, strings.Split(rest, string(filepath.Separator))...)
	}
	return out
}

// deepestRootOf returns the configured root abs is at or under, the deepest
// where roots nest, as configured and as filepath.Abs gives it, and abs's own
// spelling of the path below it (underRoot, so folded where the volume folds
// case); "" for none. abs must be absolute.
func (s *Scanner) deepestRootOf(roots []string, abs string) (root, absRoot, tail string) {
	depth := -1
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		t, ok := s.underRoot(a, abs)
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

// nestedRootsUnder lists the configured roots strictly below root, which a
// walk of root reaches as folders of its own, compared as underRoot compares;
// nil when there is none, which is every library but one with nested roots.
func (s *Scanner) nestedRootsUnder(roots []string, root string) []nestedRoot {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var out []nestedRoot
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		if tail, ok := s.underRoot(absRoot, a); ok && tail != "." {
			out = append(out, nestedRoot{root: r, abs: a, tail: tail})
		}
	}
	return out
}

// nestedPass is the nested roots a walk passes over, by the folder each is
// below the walked root, matched as the volume compares names.
type nestedPass struct {
	folders []string
	fold    bool
}

// has reports whether the folder at abs, which a walk reaches, is one of them.
// Both are clean, so folding the whole of each folds each element.
func (p nestedPass) has(abs string) bool {
	abs = filepath.Clean(abs)
	for _, f := range p.folders {
		if abs == f || p.fold && strings.EqualFold(abs, f) {
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
// every row under it as it is, its missing count untouched; where the volume
// folds case (foldsCase of root, a folder that is there), so is every spelling
// of that prefix the index holds (rows: the walks wrote them as the volume
// spells its folders, which the configured spelling may not be). It returns
// them, for the walk to pass over where it reaches them. A nested root that
// reads as it is is walked, as before.
func (s *Scanner) protectNestedRoots(ctx context.Context, root string, nested []nestedRoot, multiRoot bool, rows []map[string]struct{}, errorSubtrees map[string]struct{}) nestedPass {
	var pass nestedPass
	if len(nested) == 0 {
		return pass
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return pass
	}
	var prefixes []string
	for _, n := range nested {
		if !s.lookAtRoot(ctx, n.root).absent {
			continue
		}
		folder := filepath.Join(absRoot, n.tail)
		prefix := relPath(absRoot, folder, multiRoot)
		errorSubtrees[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
		pass.folders = append(pass.folders, folder)
	}
	if len(prefixes) == 0 {
		return pass
	}
	pass.fold = s.foldsCase(absRoot)
	if pass.fold {
		for _, prefix := range prefixes {
			for _, set := range rows {
				protectFoldedPrefix(prefix, set, errorSubtrees)
			}
		}
	}
	return pass
}

// protectFoldedPrefix records, as a subtree the walk could not see, every
// spelling of prefix, with case folded element by element, that a path of
// rows begins with.
func protectFoldedPrefix(prefix string, rows map[string]struct{}, errorSubtrees map[string]struct{}) {
	want := strings.Split(prefix, "/")
	for p := range rows {
		got := strings.SplitN(p, "/", len(want)+1)
		if len(got) < len(want) {
			continue
		}
		same := true
		for i := range want {
			if !strings.EqualFold(got[i], want[i]) {
				same = false
				break
			}
		}
		if same {
			errorSubtrees[strings.Join(got[:len(want)], "/")] = struct{}{}
		}
	}
}
