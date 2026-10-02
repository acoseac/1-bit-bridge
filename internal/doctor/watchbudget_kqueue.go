//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package doctor

import (
	"context"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// checkNameWatcherFDBudget is the slug of the kqueue watch-budget check.
const checkNameWatcherFDBudget = "watcher-fd-budget"

// watchFileLimits is manifest.WatchFileLimits; a test stands in for it.
var watchFileLimits = manifest.WatchFileLimits

// checkWatchLimit, on macOS (and wherever fsnotify watches through kqueue),
// warns when the library watcher is on and the library needs more open files
// than the watcher may hold. kqueue makes every watched folder and every
// entry in one an open file of the bridge, and the watcher stays off, with
// one warning, when the library needs more than its budget (half of the
// process's open-file limit, a quarter of the system's at most;
// manifest.WatchFDLimits.Budget), and stops, releasing every watch, when the
// library grows past it. The count and the budget are the watcher's own
// (manifest.CountWatchSet), so this pre-flight and the watcher cannot
// disagree about the library. The limit is this process's, which bridge
// serve shares unless its launchd job or its shell sets another.
//
// Skipped silently when the watcher is disabled or no root is configured
// yet, as on Linux.
func checkWatchLimit(_ context.Context, d Deps) Check {
	if !d.LibraryWatchEnabled {
		return ok(checkNameWatcherFDBudget, "library watcher disabled — check skipped")
	}
	if len(d.LibraryRoots) == 0 {
		return ok(checkNameWatcherFDBudget, "no library roots configured yet")
	}
	limits, err := watchFileLimits()
	if err != nil {
		return warn(checkNameWatcherFDBudget,
			fmt.Sprintf("could not read the open-file limit: %v", err),
			"the library watcher stays off when it cannot read it; the periodic scan picks up changes.")
	}
	budget := limits.Budget()
	count, err := manifest.CountWatchSet(d.LibraryRoots, budget+1)
	if err != nil {
		return warn(checkNameWatcherFDBudget,
			fmt.Sprintf("could not enumerate the library's folders: %v", err),
			"check failed; the watcher counts the library again when bridge serve starts.")
	}
	share := describeWatchShare(limits)
	if count.OpenFiles() > budget {
		return warn(checkNameWatcherFDBudget,
			fmt.Sprintf("the library would take the watcher past its share of %d open files (%s; one per folder and per file it watches): the watcher will stay off, and the periodic scan picks up changes", budget, share),
			watchBudgetHint)
	}
	summary := fmt.Sprintf("%d open files (%d folders and %d entries in them) of the watcher's %d (%s)",
		count.OpenFiles(), count.Folders, count.Entries, budget, share)
	if count.OpenFiles()*5 > budget*4 {
		return warn(checkNameWatcherFDBudget,
			summary+": over 80% of it, and the watcher stops, releasing every watch, once the library grows past it",
			watchBudgetHint)
	}
	return ok(checkNameWatcherFDBudget, summary)
}

// watchBudgetHint is what the kqueue check advises when the library is near
// or past the watcher's budget.
const watchBudgetHint = "to watch a larger library, raise kern.maxfilesperproc and kern.maxfiles (sysctl, as root) and restart the bridge; or leave the watcher off: the periodic scan picks up changes either way."

// describeWatchShare says which limit the budget is a share of.
func describeWatchShare(l manifest.WatchFDLimits) string {
	if l.SystemCapped() {
		return fmt.Sprintf("a quarter of the system's open-file table of %d; this process's limit is %d", l.System, l.Process)
	}
	return fmt.Sprintf("half of this process's open-file limit of %d", l.Process)
}
