//go:build linux

package doctor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
func checkInotifyLimit(d Deps) Check {
	if !d.LibraryWatchEnabled {
		return ok("inotify-watch-limit", "library watcher disabled — check skipped")
	}
	if len(d.LibraryRoots) == 0 {
		return ok("inotify-watch-limit", "no library roots configured yet")
	}
	limit, err := readInotifyLimit()
	if err != nil {
		return warn("inotify-watch-limit",
			fmt.Sprintf("could not read /proc/sys/fs/inotify/max_user_watches: %v", err),
			"falling back to runtime detection — watcher will log a clear error if the budget is exceeded.")
	}
	dirs, err := countDirs(d.LibraryRoots)
	if err != nil {
		return warn("inotify-watch-limit",
			fmt.Sprintf("could not enumerate library directories: %v", err),
			"check failed; runtime detection still applies.")
	}
	threshold := int(float64(limit) * 0.8)
	if dirs > threshold {
		return warn("inotify-watch-limit",
			fmt.Sprintf("%d directories vs limit %d (>80%%) — watcher may exhaust kernel budget", dirs, limit),
			"raise the limit: `echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p`")
	}
	return ok("inotify-watch-limit",
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
// register. Errors are non-fatal at the per-dir level — a perm
// flap shouldn't kill the check.
func countDirs(roots []string) (int, error) {
	total := 0
	for _, root := range roots {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
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
			return nil
		}); err != nil {
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
