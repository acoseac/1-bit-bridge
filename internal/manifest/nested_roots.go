package manifest

import (
	"context"
	"path/filepath"
	"strings"
)

// Library roots may nest: neither the config's validation nor the console's
// or `bridge library add`'s root add refuses a root configured inside another
// (B651's fifth round, CodeRabbit on #1397). A path under a nested root is the
// nested root's, the deepest root that holds it, whatever order the roots are
// listed in: the watcher's events and the looks its dispatches make
// (configuredRootOf), and a subtree scan's owning root (ScanSubtree), all go
// through deepestRootOf. Before, each took the first root listed that held the
// path, so with the outer root listed first a nested root's events, held or
// not, went to the outer one, and so did its subtree scans, indexing its
// files under the outer root's path. A walk of the outer root still passes
// through a nested root, as it always has, unless the nested root reads as a
// volume that is not mounted (spareNestedRoot).

// deepestRootOf returns the configured root abs is at or under, the deepest
// where roots nest, as configured and as filepath.Abs gives it, by
// filepath.Rel (the containment ScanSubtree has always used); "" for none.
// abs must be absolute.
func deepestRootOf(roots []string, abs string) (root, absRoot string) {
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(a, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Every root that holds abs is abs or above it, so of two such roots
		// the longer is the deeper.
		if len(a) > len(absRoot) {
			root, absRoot = r, a
		}
	}
	return root, absRoot
}

// nestedRootsUnder is the configured roots strictly below root, keyed by the
// path filepath.Abs gives each, which a walk of root reaches as folders of its
// own; nil when there is none, which is every library but one with nested
// roots.
func nestedRootsUnder(roots []string, root string) map[string]string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	var out map[string]string
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil || a == absRoot {
			continue
		}
		rel, err := filepath.Rel(absRoot, a)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[a] = r
	}
	return out
}

// spareNestedRoot reports whether a walk of a root that reaches the folder
// abs, a nested root (nested, from nestedRootsUnder), must pass over it: when
// the nested root reads as a volume that is not mounted (Scanner.lookAtRoot,
// the look its own scan's guards and the watcher's hold read). Its bare
// mount point is no evidence that the files the outer root indexed under it
// are gone, so the walk records it as a subtree it could not see, which the
// deletion pass spares, and does not walk into it (Argon on the brief). A
// nested root that reads as it is is walked, as before.
func (s *Scanner) spareNestedRoot(ctx context.Context, nested map[string]string, abs string) bool {
	if len(nested) == 0 {
		return false
	}
	root, ok := nested[filepath.Clean(abs)]
	return ok && s.lookAtRoot(ctx, root).absent
}
