package admin

import (
	"errors"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxSubtreeScans caps how many discrete ScanSubtree calls one commit triggers
// before a single full Scan is cheaper.
//
// It is a guess, held at 8 for v1 rather than justified by a number nobody has
// measured. What makes it tunable later is the restamp duration the scanner now
// logs on every subtree scan: ScanSubtree's tail runs restampDuplicates, and
// that pass is WHOLE-LIBRARY, not subtree-scoped — so N subtree scans cost N
// whole-library restamps, which is the actual reason a cap exists at all.
const maxSubtreeScans = 8

// planScanDirs turns the library-relative directories a commit touched into the
// set of subtree scans to run, or reports that a full scan is cheaper.
//
// It deliberately does NOT scan the common ancestor. Files committed to
// "A/Album" and "Z/Album" have the library root as their LCA, so an
// ancestor-based trigger silently degrades to a full scan on exactly the
// sessions where a targeted one matters most.
//
// The collapse has a FLOOR at depth 1. Without it, an upload spanning nine
// top-level artist folders collapses all nine to the root in one iteration and
// escalates to a full scan — when nine discrete subtree scans were the whole
// point, and the collapse threw away the information needed to do them. The
// floor also makes the fallback meaningful: it fires on genuine breadth (more
// distinct top-level folders than the cap), not on an artefact of the loop.
func planScanDirs(dirs []string, cap int) (scan []string, fullScan bool) {
	if cap < 1 {
		cap = 1
	}
	seen := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		d = strings.TrimPrefix(path.Clean(strings.TrimSpace(d)), "/")
		if d == "" || d == "." {
			// A file committed at the root level: its subtree IS the root,
			// so there is nothing to narrow.
			return nil, true
		}
		seen[d] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, false
	}

	cur := pruneDescendants(scanDirKeys(seen))
	for len(cur) > cap {
		next := make(map[string]struct{}, len(cur))
		movedAny := false
		for _, d := range cur {
			parent := path.Dir(d)
			if parent == "." || parent == "/" {
				// Already a direct child of the root: the floor.
				next[d] = struct{}{}
				continue
			}
			movedAny = true
			next[parent] = struct{}{}
		}
		if !movedAny {
			// Everything has bottomed out at depth 1; collapsing further
			// would mean the root.
			break
		}
		// Re-prune, not merely re-dedupe: collapsing can CREATE an ancestor
		// relationship that did not exist before. "A/B/C" and "A/X" become
		// "A/B" and "A", and "A/B" is now a descendant of "A".
		cur = pruneDescendants(scanDirKeys(next))
	}
	if len(cur) > cap {
		return nil, true
	}
	return cur, false
}

func scanDirKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// pruneDescendants drops any path covered by another in the set. Sorting first
// puts an ancestor before every descendant it covers ("A/B" < "A/B/C"), and the
// explicit separator in the prefix test keeps "A/B" from swallowing "A/B-x".
func pruneDescendants(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for _, d := range in {
		covered := false
		for _, kept := range out {
			if d == kept || strings.HasPrefix(d, kept+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, d)
		}
	}
	return out
}

// spawnBackgroundSubtreeScan scans the given library-relative directories under
// root.
//
// It mirrors spawnBackgroundScan's WaitGroup contract exactly: admin shutdown
// waits on s.bgScans (capped at a 5s grace), so a process exit during a
// mid-write scan cannot corrupt SQLite. Never replace this with a raw
// `go func()` — that is the regression apiScan already had once.
func (s *Server) spawnBackgroundSubtreeScan(label, root string, relDirs []string) {
	if s.deps.Scanner == nil || len(relDirs) == 0 {
		return
	}
	ctx := s.scanCtx()
	s.bgScans.Add(1)
	go func() {
		defer s.bgScans.Done()
		for _, rel := range relDirs {
			if ctx.Err() != nil {
				return
			}
			abs := filepath.Join(root, filepath.FromSlash(rel))
			if _, err := s.deps.Scanner.ScanSubtree(ctx, abs); err != nil && !errors.Is(err, ctx.Err()) {
				logger.Error("background subtree scan failed", "label", label, "dir", abs, "err", err)
			}
		}
	}()
}
