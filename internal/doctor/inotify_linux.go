//go:build linux

package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// errCountCapReached is the internal sentinel countDirs uses to stop the
// walk once it has counted enough directories to answer the caller's
// only question ("more than the threshold?").
var errCountCapReached = errors.New("countDirs: cap reached")

// checkNameInotifyWatchLimit is the slug for the Linux-only inotify
// budget check. Six call sites in this file across the ok/warn paths.
const checkNameInotifyWatchLimit = "inotify-watch-limit"

// checkInotifyLimit warns when fs.inotify.max_user_watches looks
// too low for the configured library roots and the operator has
// LibraryWatch enabled. We count directories under each root and
// compare against 80 % of the kernel limit — the budget is shared
// with every other watching app on the box, so a 100 % match
// would be a near-certain runtime failure.
//
// Skipped silently when:
//   - LibraryWatch is disabled (the watcher won't try to register
//     watches anyway)
//   - LibraryRoots is empty (first-run, no config yet)
//   - We can't read /proc/sys/fs/inotify/max_user_watches (rare on
//     a real Linux box; unit tests in container with /proc mounted
//     ro might trip this — non-fatal)
func checkInotifyLimit(_ context.Context, d Deps) Check {
	if !d.LibraryWatchEnabled {
		return ok(checkNameInotifyWatchLimit, "library watcher disabled — check skipped")
	}
	if len(d.LibraryRoots) == 0 {
		return ok(checkNameInotifyWatchLimit, "no library roots configured yet")
	}
	limit, err := readInotifyLimit()
	if err != nil {
		return warn(checkNameInotifyWatchLimit,
			fmt.Sprintf("could not read /proc/sys/fs/inotify/max_user_watches: %v", err),
			"falling back to runtime detection — watcher will log a clear error if the budget is exceeded.")
	}
	threshold := int(float64(limit) * 0.8)
	// Cap the walk at threshold+1: the check only needs to know whether the
	// directory count EXCEEDS the threshold, so there's no point
	// enumerating every directory on a multi-TB NAS just to over-count.
	dirs, err := countDirs(d.LibraryRoots, threshold+1)
	if err != nil {
		return warn(checkNameInotifyWatchLimit,
			fmt.Sprintf("could not enumerate library directories: %v", err),
			"check failed; runtime detection still applies.")
	}
	if dirs > threshold {
		return warn(checkNameInotifyWatchLimit,
			fmt.Sprintf("more than %d directories (>80%% of limit %d) — watcher may exhaust kernel budget", threshold, limit),
			"raise the limit: `echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p`")
	}
	return ok(checkNameInotifyWatchLimit,
		fmt.Sprintf("%d directories vs limit %d", dirs, limit))
}

// readInotifyLimit reads the integer at
// /proc/sys/fs/inotify/max_user_watches.
func readInotifyLimit() (int, error) {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", string(raw), err)
	}
	return n, nil
}

// countDirs walks every configured root and counts directories,
// honouring filepath.SkipDir on dotfiles and well-known noise
// folders so the count matches what the watcher will actually
// register. Per-subdir errors are non-fatal (a permission flap
// shouldn't kill the check), but a root-level failure (the root
// itself can't be opened) propagates so the doctor check warns
// rather than reporting a false OK with zero count (CodeRabbit
// Major post-merge on PR #83).
// countDirs walks the roots counting directories. When stopAt > 0 it
// stops early once the running total reaches stopAt (the caller only
// needs a "> threshold?" verdict), bounding the walk's cost on huge
// libraries.
func countDirs(roots []string, stopAt int) (int, error) {
	total := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					// Root unreadable — bubble up so checkInotifyLimit's
					// caller produces a Warn instead of a misleading OK.
					return err
				}
				// Sub-tree perm/IO flap — keep counting siblings.
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			name := d.Name()
			if path != root && shouldSkipNoiseDir(name) {
				return filepath.SkipDir
			}
			total++
			if stopAt > 0 && total >= stopAt {
				return errCountCapReached
			}
			return nil
		})
		if errors.Is(err, errCountCapReached) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// shouldSkipNoiseDir mirrors the manifest scanner's
// `shouldSkipDir` for the well-known FS-noise folders the watcher
// won't register. Kept local to avoid an internal package cycle —
// this list rarely changes.
func shouldSkipNoiseDir(name string) bool {
	switch name {
	case ".Trash", ".Trashes", ".Spotlight-V100", ".fseventsd", ".DocumentRevisions-V100",
		"$RECYCLE.BIN", "System Volume Information",
		".git", ".hg", ".svn":
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	return false
}
