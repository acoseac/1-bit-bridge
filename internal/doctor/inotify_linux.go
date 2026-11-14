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

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// errCountCapReached is the internal sentinel countDirs uses to stop the
// walk once it has counted enough directories to answer the caller's
// only question ("more than the threshold?").
var errCountCapReached = errors.New("countDirs: cap reached")

// checkNameInotifyWatchLimit is the slug for the Linux-only inotify
// budget check. Six call sites in this file across the ok/warn paths.
const checkNameInotifyWatchLimit = "inotify-watch-limit"

// checkWatchLimit, on Linux, warns when fs.inotify.max_user_watches looks
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
//
// Each root is counted as a work of the runner's RootProbes, on a goroutine
// of its own, and waited for until the bound from when its count began
// (backlog B650): a walk that meets a folder on a mount that does not answer
// waits in the kernel with no end, below a root whose own probe answers too.
// A count that does not finish is never read as a low one: the check says
// which root's count it did not have, and that it does not know whether the
// watcher fits, unless the roots it has already pass the threshold.
func checkWatchLimit(ctx context.Context, d Deps) Check {
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
	threshold := inotifyThreshold(limit)
	// Cap the walk at threshold+1: the check only needs to know whether the
	// directory count EXCEEDS the threshold, so there's no point
	// enumerating every directory on a multi-TB NAS just to over-count.
	t := d.probeRun().tallyWatches(ctx, d.LibraryRoots, threshold+1,
		func(c watchCount) int { return c.folders }, unansweredCount("the folders"))
	if t.err != nil {
		return warn(checkNameInotifyWatchLimit,
			fmt.Sprintf("could not enumerate library directories: %v", t.err),
			"check failed; runtime detection still applies.")
	}
	if t.reached {
		summary := fmt.Sprintf("more than %d directories (>80%% of limit %d) — watcher may exhaust kernel budget", threshold, limit)
		hint := "raise the limit: `echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p`"
		if len(t.unanswered) > 0 {
			summary += "; " + strings.Join(t.unanswered, "; ")
			hint += "; " + stalledCountAdvice
		}
		return warn(checkNameInotifyWatchLimit, summary, hint)
	}
	if len(t.unanswered) > 0 {
		return warn(checkNameInotifyWatchLimit,
			fmt.Sprintf("not checked: %s, so whether the watcher fits its share of limit %d is not known", strings.Join(t.unanswered, "; "), limit),
			stalledCountAdvice)
	}
	return ok(checkNameInotifyWatchLimit,
		fmt.Sprintf("%d directories vs limit %d", t.total.folders, limit))
}

// inotifyThreshold is the directory count past which checkWatchLimit warns:
// 80 % of the kernel's limit, which every other watching app on the box
// shares.
func inotifyThreshold(limit int) int { return int(float64(limit) * 0.8) }

// beginWatchCounts begins the count of each root that checkWatchLimit waits
// for, where it waits for one (the watcher on, roots configured, the limit
// read), so Run's works on the roots run together.
func beginWatchCounts(d Deps) {
	if !d.LibraryWatchEnabled || len(d.LibraryRoots) == 0 {
		return
	}
	limit, err := readInotifyLimit()
	if err != nil {
		return
	}
	run := d.probeRun()
	for _, root := range d.LibraryRoots {
		run.beginCount(root, inotifyThreshold(limit)+1)
	}
}

// countRootWatches is the count of one root's directories, countDirs over it
// alone: a work of the runner's RootProbes.
func countRootWatches(root string, stopAt int) (watchCount, error) {
	n, err := countDirs([]string{root}, stopAt)
	return watchCount{folders: n}, err
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
// skipping what manifest.ShouldSkipDir names, the one rule the watcher
// and the scanner skip by, so the count matches what the watcher will
// actually register. Per-subdir errors are non-fatal (a permission flap
// shouldn't kill the check), but a root-level failure (the root
// itself can't be opened) propagates so the doctor check warns
// rather than reporting a false OK with zero count (CodeRabbit
// Major post-merge on PR #83).
// countDirs walks the roots counting directories. When stopAt > 0 it
// stops early once the running total reaches stopAt (the caller only
// needs a "> threshold?" verdict), bounding the walk's cost on huge
// libraries.
//
// A root that is itself a link to a directory is walked through, as the
// watcher walks it (fsutil.WalkableRoot): walked as the link it counted
// nothing, while the watcher registers a watch per directory behind it.
func countDirs(roots []string, stopAt int) (int, error) {
	total := 0
	for _, root := range roots {
		walkFrom, err := fsutil.WalkableRoot(root)
		if err != nil {
			return total, err
		}
		err = fsutil.WalkConfigured(walkFrom, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == walkFrom {
					// Root unreadable — bubble up so checkWatchLimit's
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
			if path != walkFrom && manifest.ShouldSkipDir(name) {
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
