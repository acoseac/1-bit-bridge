package manifest

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"time"
)

// The roots the watcher holds (backlog B651). A NAS that starts the bridge
// before its volumes are mounted shows a library root as a path that is not
// there, or as the bare mount point holding nothing. The scan spares every row
// under such a root (walkRoot, emptyRootMustBeSpared), and the watcher must not
// act on one either: a watch on the bare mount point is on a directory the
// mount then hides, so nothing the volume holds is ever seen, and a root that
// could not be walked at the start was never watched at all. So a configured
// root that reads as a volume not mounted (Scanner.lookAtRoot) is held: it is
// not watched, not even by the walk of a root it is nested in (heldBelow), its
// events are dropped and no scan is dispatched for it, until a look finds it
// as it was. A path is the deepest configured root's that holds it
// (Scanner.rootOf), so a held root nested in another keeps its own events
// and scans, compared as its volume compares names: where it folds case, a
// root configured as USB holds the folder the walks list as usb. It is looked
// at:
//
//   - at the start, before any watch is added (holdAbsentRoots);
//   - when a debounced scan of a folder under it is due, before the scan is
//     dispatched (absentRootOf): an unmount takes the volume's content from
//     under the watches, and a watch left on the bare mount point sends the
//     event that asks for that scan;
//   - when an event names the root itself, removed or moved away (lookAgain;
//     Windows says so of a watched directory that went);
//   - every rootCheckEvery, when a watched root's own watch is one fsnotify no
//     longer holds (checkRootWatches), on every platform: inotify drops every
//     watch on a volume as it is unmounted and sends nothing (IN_UNMOUNT, which
//     fsnotify swallows), and kqueue drops a folder's watch and sends nothing
//     when the folder is renamed after a change in it and before fsnotify read
//     the change (reconcileWatches), a root's own folder as much as any;
//   - and, while it is held, on a bounded retry (recheckHeld): recheckFirst
//     after a root is held, then twice as long each time, recheckMax at most,
//     each look a stat, one batch of the root's listing and, for a root that
//     holds nothing, one count of its rows.
//
// A held root that reads as it did is watched again, through the directory at
// its path now, and a scan of it is asked for, since nothing saw what changed
// in it meanwhile. Each change is said once: held, at Warn, with what the look
// found (and again when that changes), and watched again, at Info. A look that
// finds what the last one found says nothing.
//
// A root the index holds nothing of (a fresh install whose volume is not
// mounted yet) reads as it is and is watched; a mount over it then hides that
// watch until the bridge restarts, and the periodic scan still indexes the
// volume. kqueue (macOS) keeps the watched folders of an unmounted volume
// open, so there a volume that goes is seen by the debounced dispatch's look
// alone. A container's bind mount made before the host mounts the volume
// never sees the mount (Docker's default propagation, rprivate): there the
// root reads as the bare mount point until the container is restarted
// (ops/deployment-runbook.md).

// Messages the held roots log, named so tests can find them.
const (
	msgWatchRootHeld      = "library root reads as a volume that is not mounted; the watcher does not watch it until it reads as it did"
	msgWatchRootResumed   = "library root reads as it did again; the watcher watches it"
	msgWatchRootRewatched = "library root's own watch went while the root reads as it did; the watcher watches it again where it is now"
	heldRootHint          = "check that its volume is mounted: the scan keeps the tracks indexed under it, and the watcher looks at it again until it reads as it did; " +
		"in a container, restart it once the volume is mounted, since a bind mount made before the mount does not see it"
)

const (
	// heldRecheckFirst is how long after a root is held the watcher looks at
	// it again; each look that finds it still absent doubles the wait, up to
	// heldRecheckMax.
	heldRecheckFirst = 5 * time.Second
	heldRecheckMax   = 5 * time.Minute
	// rootWatchCheckEvery is how often the watcher asks fsnotify whether each
	// watched root's own watch is still there (checkRootWatches).
	rootWatchCheckEvery = 30 * time.Second
)

// holdAbsentRoots looks at each configured root before the watcher adds any
// watch, holds those that read as a volume not mounted, and returns the
// others, for the watcher to watch.
func (wt *Watcher) holdAbsentRoots(ctx context.Context, roots []string) []string {
	var watch []string
	for _, root := range roots {
		if look := wt.scanner.lookAtRoot(ctx, root); look.absent {
			wt.holdRoot(root, look)
			continue
		}
		watch = append(watch, root)
	}
	return watch
}

// noteRootWatches records, for each of roots whose own watch fsnotify now
// holds, the path that watch is registered as, for checkRootWatches. A root
// whose own watch could not be added is not recorded: no check could tell a
// watch that went from one that was never there.
func (wt *Watcher) noteRootWatches(roots []string) {
	if len(roots) == 0 {
		return
	}
	live := wt.watchSet()
	for _, root := range roots {
		rw := wt.rootWatchPath(root)
		if _, ok := live[rw]; ok {
			wt.rootWatches[root] = rw
		}
	}
}

// rootWatchPath is the path a configured root's own watch is registered as:
// the directory a linked root resolves to (its alias), or the root.
func (wt *Watcher) rootWatchPath(root string) string {
	for _, a := range wt.aliases {
		if a.configured == root {
			return filepath.Clean(a.resolved)
		}
	}
	return filepath.Clean(root)
}

// watchSet is the paths fsnotify watches now, cleaned.
func (wt *Watcher) watchSet() map[string]struct{} {
	list := wt.w.WatchList()
	set := make(map[string]struct{}, len(list))
	for _, p := range list {
		set[filepath.Clean(p)] = struct{}{}
	}
	return set
}

// holdRoot holds root on what look found. A root that was watched lets its
// watches go (releaseRoot) and the scans armed under it (dropScansUnder), and
// the next look at the held roots is due recheckFirst from now. The hold is
// said once, and again when what the look found changes; a look that finds
// what the last one found says nothing. It reports whether root was newly
// held.
func (wt *Watcher) holdRoot(root string, look rootLook) bool {
	prev, was := wt.held[root]
	wt.held[root] = look
	if was && prev.reason == look.reason {
		return false
	}
	if !was {
		wt.releaseRoot(root)
		wt.dropScansUnder(root)
		wt.recheckDelay, wt.rearm = wt.recheckFirst, true
	}
	attrs := []any{"root", root, "reads", look.reason}
	if look.rows > 0 {
		attrs = append(attrs, "rows_in_db", look.rows)
	}
	if look.err != nil {
		attrs = append(attrs, "err", look.err)
	}
	watcherLogger.Warn(msgWatchRootHeld, append(attrs, "hint", heldRootHint)...)
	if !was {
		if hook := wt.afterHoldForTests; hook != nil {
			hook(root)
		}
	}
	return !was
}

// releaseRoot lets go of every watch the watcher holds at or under root's own:
// what fsnotify still holds on the volume's folders, by the names they had,
// and on kqueue the account of them too (releaseTree). A watch fsnotify has
// dropped already answers its Remove with an error, which says nothing here.
// Where the volume folds case, so are the watches the walk of a root it is
// nested in added at its folder in the volume's spelling, usb for a root
// configured as USB (B651's sixth round). The root's alias stays, so an event
// already queued under the name it resolved to is still named back under the
// root, and dropped as a held root's; watchAgain notes it afresh.
func (wt *Watcher) releaseRoot(root string) {
	rw := wt.rootWatchPath(root)
	delete(wt.rootWatches, root)
	fold := wt.scanner.foldsCase(rw)
	if wt.fds != nil {
		if !wt.fds.off {
			for _, p := range wt.fds.spellings(rw, fold) {
				wt.releaseTree(p)
				wt.fds.dropPending(p)
			}
		}
		return
	}
	for p := range wt.watchSet() {
		at := pathAtOrUnder(p, rw)
		if !at && fold {
			_, at = foldedTail(rw, p)
		}
		if at {
			_ = wt.w.Remove(p)
		}
	}
}

// dropScansUnder stops the debounced scans armed for folders under root, the
// root the dispatch would look at (absentRootOf), once the watcher holds it:
// each would look at the root again as it fell due, for nothing, and ask for
// the hold again, and the scan watchAgain asks for once the root reads as it
// did covers every folder under it. A scan whose timer has fired already finds
// its entry gone, and its look still runs.
func (wt *Watcher) dropScansUnder(root string) {
	roots := wt.scanner.Roots()
	wt.mu.Lock()
	defer wt.mu.Unlock()
	for dir, ps := range wt.pending {
		if r, _ := wt.scanner.rootIn(roots, dir); r == root {
			ps.timer.Stop()
			delete(wt.pending, dir)
		}
	}
}

// watchAgain watches root where it is now, held or not, and asks for a scan
// of it: nothing saw what changed in it while it was not watched. It answers
// the error of a root whose tree could not be added after all (it went again
// between the look and the walk, or, on kqueue, took the watcher past its
// budget, which stops the watcher for good).
func (wt *Watcher) watchAgain(ctx context.Context, root string) error {
	if wt.fds != nil && wt.fds.off {
		return errWatchBudget
	}
	wt.aliases = slices.DeleteFunc(wt.aliases, func(a rootAlias) bool { return a.configured == root })
	if err := wt.addTree(root, true); err != nil {
		return err
	}
	wt.noteRootWatches([]string{root})
	wt.scheduleScan(ctx, root)
	return nil
}

// resumeRoot watches a held root again (watchAgain), says so, and stops
// holding it. A root that cannot be walked after all stays held, as a root
// that cannot be seen.
func (wt *Watcher) resumeRoot(ctx context.Context, root string) {
	if err := wt.watchAgain(ctx, root); err != nil {
		if wt.fds == nil || !wt.fds.off {
			wt.holdRoot(root, rootLook{absent: true, reason: rootAbsentUnreachable, err: err})
		}
		return
	}
	delete(wt.held, root)
	watcherLogger.Info(msgWatchRootResumed, "root", root)
	if hook := wt.afterResumeForTests; hook != nil {
		hook(root)
	}
}

// recheckHeld looks at each held root again and watches again each that reads
// as it did. A root no longer configured is let go: the watcher watches the
// roots it started with, and the scanner refuses a scan of one it no longer
// has.
func (wt *Watcher) recheckHeld(ctx context.Context) {
	roots := wt.scanner.Roots()
	for _, root := range slices.Sorted(maps.Keys(wt.held)) {
		if !slices.Contains(roots, root) {
			delete(wt.held, root)
			continue
		}
		look := wt.scanner.lookAtRoot(ctx, root)
		if ctx.Err() != nil {
			return
		}
		if look.absent {
			wt.holdRoot(root, look)
			continue
		}
		wt.resumeRoot(ctx, root)
	}
}

// checkRootWatches looks at each watched root whose own watch fsnotify no
// longer holds though nothing said so: inotify drops every watch on a volume
// as it is unmounted and sends no event (IN_UNMOUNT, which fsnotify
// swallows), and kqueue drops the watch of a root renamed after a change in
// it and before fsnotify read the change, sending nothing either, while its
// reconcile lets go only what is left under it (reconcileWatches). A root
// that reads as a volume not mounted is held. One that reads as it did was
// replaced under its watch (unmounted and mounted again between two checks,
// or moved away and another folder put at its path): what is left of its
// watches goes, and it is watched again where it is now, with a scan.
func (wt *Watcher) checkRootWatches(ctx context.Context) {
	if len(wt.rootWatches) == 0 {
		return
	}
	// Only the roots' own watches are asked after, so the list is read
	// against them, and nothing the size of the library is kept.
	held := make(map[string]bool, len(wt.rootWatches))
	for _, rw := range wt.rootWatches {
		held[rw] = false
	}
	for _, p := range wt.w.WatchList() {
		if _, ok := held[filepath.Clean(p)]; ok {
			held[filepath.Clean(p)] = true
		}
	}
	for _, root := range slices.Sorted(maps.Keys(wt.rootWatches)) {
		if held[wt.rootWatches[root]] {
			continue
		}
		wt.lookAgain(ctx, root, msgWatchRootRewatched)
	}
}

// lookAgain looks at a watched root whose own watch can no longer be trusted:
// its watch went (checkRootWatches), or an event named the root itself,
// removed or moved away (handleEvent). One that reads as a volume not mounted
// is held; one that reads as it did is watched again where it is now (its
// old watch being on what went), said with msg.
func (wt *Watcher) lookAgain(ctx context.Context, root, msg string) {
	look := wt.scanner.lookAtRoot(ctx, root)
	if ctx.Err() != nil {
		return
	}
	if look.absent {
		wt.holdRoot(root, look)
		return
	}
	wt.releaseRoot(root)
	if err := wt.watchAgain(ctx, root); err != nil {
		if wt.fds == nil || !wt.fds.off {
			wt.holdRoot(root, rootLook{absent: true, reason: rootAbsentUnreachable, err: err})
		}
		return
	}
	watcherLogger.Info(msg, "root", root)
}

// absentRootOf looks at the configured root dir is at or under, when a
// debounced scan of dir is due, and answers it with what the look found when
// it reads as a volume not mounted. A dir under no root is left to
// ScanSubtree, which refuses it. Called on the dispatch's own goroutine.
func (wt *Watcher) absentRootOf(ctx context.Context, dir string) (string, rootLook, bool) {
	root, _ := wt.scanner.rootOf(dir)
	if root == "" {
		return "", rootLook{}, false
	}
	look := wt.scanner.lookAtRoot(ctx, root)
	if !look.absent || ctx.Err() != nil {
		return "", rootLook{}, false
	}
	return root, look, true
}

// askHold hands Run's goroutine a root a dispatch found absent, to hold
// (takeHoldAsks), and wakes it. It never blocks: Run's shutdown waits for
// every dispatch to return.
func (wt *Watcher) askHold(root string, look rootLook) {
	wt.mu.Lock()
	if wt.holdAsks == nil {
		wt.holdAsks = make(map[string]rootLook)
	}
	wt.holdAsks[root] = look
	wt.mu.Unlock()
	select {
	case wt.holdWake <- struct{}{}:
	default:
	}
}

// takeHoldAsks holds the roots the dispatches asked for, on Run's goroutine.
// A root that came back since its dispatch looked is held all the same, and
// the retry recheckFirst later watches it again.
func (wt *Watcher) takeHoldAsks() {
	wt.mu.Lock()
	asks := wt.holdAsks
	wt.holdAsks = nil
	wt.mu.Unlock()
	for _, root := range slices.Sorted(maps.Keys(asks)) {
		wt.holdRoot(root, asks[root])
	}
}

// heldRetried notes a retry of the held roots that left some held: the next is
// due twice as long after it, recheckMax at most.
func (wt *Watcher) heldRetried() {
	if len(wt.held) == 0 {
		return
	}
	wt.recheckDelay = min(max(2*wt.recheckDelay, wt.recheckFirst), wt.recheckMax)
	wt.rearm = true
}

// rearmRecheck arms the held roots' retry when a hold or a retry asked for it
// (rearm), recheckDelay from now.
func (wt *Watcher) rearmRecheck(t *time.Timer) {
	if !wt.rearm {
		return
	}
	wt.rearm = false
	t.Reset(wt.recheckDelay)
}
