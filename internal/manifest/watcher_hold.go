package manifest

import (
	"context"
	"errors"
	"maps"
	"os"
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
// root that reads as a volume not mounted (Scanner.lookAtRoot), or as one
// holding nothing but links to directories over tracks the index holds, whose
// rows the scan spares as well (rootOnlyDirLinks, B742), is held: it is
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
//   - every rootCheckEvery too, when the folder at a watched root's path is
//     another than the one its own watch was added on (os.SameFile, against the
//     identity taken just before that watch was added; backlog B777): a volume
//     mounted over the folder the root was watched on, a volume unmounted and
//     mounted again, whose first mount's revoked folders kqueue keeps watched
//     and listed, a linked root's link repointed, none of which fsnotify says
//     anything about. A root that then reads as a volume not mounted is held;
//     an identity that cannot be read is no change, and one that could not be
//     read as the watch was added is taken once the folder reads, the watch
//     added again around the read (takeRootIdentity);
//   - and, while it is held, on a bounded retry (recheckHeld): recheckFirst
//     after a root is held, then twice as long each time, recheckMax at most,
//     each look a stat, one batch of the root's listing and, for a root that
//     holds nothing, one count of its rows.
//
// A held root that reads as it did is watched again, through the directory at
// its path now, and a scan of it is asked for, since nothing saw what changed
// in it meanwhile. Each change is said once: held, at Warn, with what the look
// found (and again when that changes), and watched again, at Info. A look that
// finds what the last one found says nothing. A root the start could not watch
// (it went between the start's look and its walk, or its own Add failed) is
// held where it reads as a volume not mounted (B777); one that is there whose
// own watch fsnotify would not add, at the start or at a check, is tried again
// on a backoff of its own, the hold's timing, with one scan as it gets there
// and none at a try, said once each way (unwatchRoot, retryUnwatched; B777's
// round 2).
//
// A root the index holds nothing of (a fresh install whose volume is not
// mounted yet) reads as it is and is watched; a mount over it puts another
// folder at its path, which the check finds and watches, with a scan (B777).
// kqueue (macOS) keeps the watched folders of an unmounted volume open, so
// there a volume that goes is seen by the debounced dispatch's look, and a
// volume mounted again by its root's identity alone; one case stays open:
// backlog B777, item 3. A container's bind mount made before the host mounts
// the volume never sees the mount (Docker's default propagation, rprivate):
// there the root reads as the bare mount point until the container is
// restarted (ops/deployment-runbook.md).

// The words a root that reads as a volume not mounted is told by: the hold's
// line and its hint, and the console's refusal of a write into such a root
// (RootAwayError, backlog B776), which says what the hold says.
const (
	readsAsNotMounted    = "reads as a volume that is not mounted"
	hintVolumeMounted    = "check that its volume is mounted"
	hintContainerRestart = "in a container, restart it once the volume is mounted, since a bind mount made before the mount does not see it"
)

// Messages the held roots log, named so tests can find them.
const (
	msgWatchRootHeld      = "library root " + readsAsNotMounted + "; the watcher does not watch it until it reads as it did"
	msgWatchRootResumed   = "library root reads as it did again; the watcher watches it"
	msgWatchRootRewatched = "library root's own watch went while the root reads as it did; the watcher watches it again where it is now"
	msgWatchRootReplaced  = "library root is another folder than the one its watch was added on (a volume mounted over it, a remount, a link repointed); the watcher watches it again where it is now"
	// msgWatchRootIdentityTaken: a root whose folder could not be read as its
	// watch was added, read since (B777's round 2).
	msgWatchRootIdentityTaken = "library root's folder reads again; the watcher watches it again where it is now, to tell when another folder takes its place"
	// msgRootWatchNotAdded and msgRootWatchAdded: a root that is there whose own
	// watch fsnotify would not add (the watch limit spent, a permission), and
	// its watch added again (B777's round 2).
	msgRootWatchNotAdded = "library root's own watch could not be added; the watcher tries again, less often each time, and the periodic scan still covers it"
	msgRootWatchAdded    = "library root's own watch could be added again; the watcher watches it"
	heldRootHint         = hintVolumeMounted + ": the scan keeps the tracks indexed under it, and the watcher looks at it again until it reads as it did; " +
		hintContainerRestart
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

// errRootWatchMissing is what a root whose own watch fsnotify does not hold
// after a walk that went on is said with.
var errRootWatchMissing = errors.New("the root's own watch was not added")

// errIdentityUnread is a folder whose identity its stat left to a later read,
// which failed.
var errIdentityUnread = errors.New("the folder's identity could not be read")

// rootIdentity is the identity of the folder at root's path now, through every
// link on the way (os.Stat, as the walk resolves a linked root), or a test's
// stand-in for it (identityForTests). Windows' os.Stat of a plain folder keeps
// its path and reads the folder's file ID only when os.SameFile first asks,
// from whatever folder the path names then, so the comparison of the stat with
// itself reads it here: an identity taken before a walk is of the folder there
// before it. Elsewhere the stat's device and inode are the identity, and the
// comparison reads nothing.
func (wt *Watcher) rootIdentity(root string) (os.FileInfo, error) {
	if h := wt.identityForTests; h != nil {
		return h(root)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, info) {
		return nil, errIdentityUnread
	}
	return info, nil
}

// rootIdentities is each root's identity now (rootIdentity), taken before the
// walks that add their watches: nil for a root whose folder cannot be read.
// All are taken before any walk, so a nested root's is taken before the walk
// of the root it is nested in opens its folder: on kqueue that walk holds the
// folder as an entry of the one above, and fsnotify's Add of the nested root
// keeps that descriptor, on whatever was at the path then (B651's ninth round).
func (wt *Watcher) rootIdentities(roots []string) map[string]os.FileInfo {
	ids := make(map[string]os.FileInfo, len(roots))
	for _, root := range roots {
		if info, err := wt.rootIdentity(root); err == nil {
			ids[root] = info
		}
	}
	return ids
}

// noteRootWatch records root's own watch for checkRootWatches, after a walk
// that was to add it: the path the watch is registered as, and pre, the
// identity the root's folder had just before the walk (backlog B777), where
// the watch is among live, fsnotify's watches, and pre is known. It reports
// false where the folder at the root's path is another now than pre: the
// watch may be on either, and the caller adds it again. Taken once the walk
// is done, the identity would be of the folder there then, which a volume
// mounted while the watch was added makes the mounted one, recorded against a
// watch on the folder it covers, and the two would compare equal for good.
// A root whose own watch fsnotify does not hold is recorded with no identity,
// so checkRootWatches finds the watch missing and tries again.
func (wt *Watcher) noteRootWatch(root string, pre os.FileInfo, live map[string]struct{}) bool {
	rw := wt.rootWatchPath(root)
	wt.rootWatches[root] = rw
	delete(wt.rootIDs, root)
	if _, installed := live[rw]; !installed || pre == nil {
		return true
	}
	wt.rootIDs[root] = pre
	post, err := wt.rootIdentity(root)
	return err != nil || os.SameFile(pre, post)
}

// confirmRootWatches records each root's own watch once the start's walks are
// done, with the identity its folder had before them (ids), and fails is the
// roots whose walk could not start, said already (backlog B777). A root whose
// own watch fsnotify does not hold goes to rootWatchMissing. One whose folder
// is another than it was before the walks (a volume mounted while they ran) is
// looked at again, as the check would: its watch may be on what was there.
func (wt *Watcher) confirmRootWatches(ctx context.Context, roots []string, ids map[string]os.FileInfo, fails map[string]bool) {
	for _, root := range roots {
		live := wt.watchSet()
		if _, installed := live[wt.rootWatchPath(root)]; !installed {
			wt.rootWatchMissing(ctx, root, fails[root])
			continue
		}
		if !wt.noteRootWatch(root, ids[root], live) {
			wt.lookAgain(ctx, root, msgWatchRootReplaced)
		}
	}
}

// rootWatchMissing is a root whose own watch fsnotify does not hold once the
// start's walk was to add it (backlog B777): the walk could not start, the
// root having gone between holdAbsentRoots' look and the walk, or the root's
// own Add failed while the walk went on (the ordinary walker logs an Add's
// failure and goes on, and kqueue's counts the folder held). A root that reads
// as a volume not mounted is held, as holdAbsentRoots holds one, unless the
// watcher has stopped for good (fds.off, as resumeRoot does). Any other is
// recorded and tried again on a backoff of its own (unwatchRoot), said as a
// walk that could not start is, where that was not said already (said): an
// Add can fail on a root that is there (a permission, the watch limit).
func (wt *Watcher) rootWatchMissing(ctx context.Context, root string, said bool) {
	look := wt.scanner.lookAtRoot(ctx, root)
	if ctx.Err() != nil {
		return
	}
	if look.absent {
		if wt.fds == nil || !wt.fds.off {
			wt.holdRoot(root, look)
		}
		return
	}
	wt.noteRootWatch(root, nil, nil)
	msg := msgInitialWatchFailed
	if said {
		msg = ""
	}
	wt.unwatchRoot(ctx, root, msg)
}

// unwatchRoot puts a root that is there, whose own watch fsnotify would not
// add, among the roots tried again on a backoff of their own (unwatched,
// retryUnwatched; B777's round 2), saying so with msg ("" when it was said
// already) and asking once for a scan of it, which reads what changed while
// nothing watched it; a root already there is left as it is, nothing said and
// no scan asked. Not the hold, which lets a root's watches go and drops its
// scans and its events, which a root that is there keeps: with the watch
// limit spent, every check looked at the root again, walked its tree, asked
// for a scan of it and said the limit again, for as long as the limit held,
// which on a NAS is not an outage. The periodic scan (scanIntervalSec, 6 h by
// default, which config cannot turn off) still covers it.
func (wt *Watcher) unwatchRoot(ctx context.Context, root, msg string) {
	if wt.unwatched[root] {
		return
	}
	wt.unwatched[root] = true
	wt.addRetryDelay, wt.addRearm = wt.recheckFirst, true
	if msg != "" {
		watcherLogger.Warn(msg, "root", root, "err", errRootWatchMissing)
	}
	wt.scheduleScan(ctx, root)
}

// retryUnwatched tries each root among unwatched again. Where fsnotify keeps
// no open file per watch (fds is nil: inotify, ReadDirectoryChangesW), the
// root's own folder is asked for alone first, saying nothing of a refusal
// (tryAdd), so a limit still spent costs one Add and no line; where fsnotify
// does (kqueue), the tree is walked as watchAgain walks it, its account kept.
// A root that takes its watch is looked at and watched again where it is now
// (watchAgain, which lets the folder's own watch go first, takes its identity
// around the walk and asks for a scan), said once (msgRootWatchAdded); one that
// reads as a volume not mounted meanwhile goes to the hold. A root no longer
// configured is let go.
func (wt *Watcher) retryUnwatched(ctx context.Context) {
	roots := wt.scanner.Roots()
	for _, root := range slices.Sorted(maps.Keys(wt.unwatched)) {
		if !slices.Contains(roots, root) {
			delete(wt.unwatched, root)
			continue
		}
		if wt.fds == nil && wt.tryAdd(wt.rootWatchPath(root)) != nil {
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
		if err := wt.watchAgain(ctx, root); err != nil {
			delete(wt.unwatched, root)
			if wt.fds == nil || !wt.fds.off {
				wt.holdRoot(root, rootLook{absent: true, reason: rootAbsentUnreachable, err: err})
			}
			continue
		}
		if !wt.unwatched[root] {
			watcherLogger.Info(msgRootWatchAdded, "root", root)
		}
	}
}

// unwatchedRetried notes a try of the unwatched roots that left some: the next
// is due twice as long after it, recheckMax at most.
func (wt *Watcher) unwatchedRetried() {
	if len(wt.unwatched) == 0 {
		return
	}
	wt.addRetryDelay = min(max(2*wt.addRetryDelay, wt.recheckFirst), wt.recheckMax)
	wt.addRearm = true
}

// rearmAddRetry arms the unwatched roots' try when an entry or a try asked for
// it (addRearm), addRetryDelay from now.
func (wt *Watcher) rearmAddRetry(t *time.Timer) {
	if !wt.addRearm {
		return
	}
	wt.addRearm = false
	t.Reset(wt.addRetryDelay)
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
	delete(wt.unwatched, root)
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
	watcherLogger.Warn(msgWatchRootHeld, append(attrs, "hint", heldHint(look))...)
	if !was {
		if hook := wt.afterHoldForTests; hook != nil {
			hook(root)
		}
	}
	return !was
}

// heldHint is the hint the line holdRoot logs gives for what the look found.
// A root holding nothing but links to directories over tracks the index holds
// (rootOnlyDirLinks) is held as the scan spares its rows, and its volume is
// there, so the line gives the scan's own account of it (msgRootOnlyDirLinks)
// where it said to check that the volume is mounted (B742); every other hold
// is a volume that may not be mounted.
func heldHint(look rootLook) string {
	if look.reason == rootOnlyDirLinks {
		return msgRootOnlyDirLinks
	}
	return heldRootHint
}

// releaseRoot lets go of every watch the watcher holds at or under root's own:
// what fsnotify still holds on the volume's folders, by the names they had,
// and on kqueue the account of them too (releaseTree). A watch fsnotify has
// dropped already answers its Remove with an error, which says nothing here.
// So are the watches the walk of a root it is nested in added at its folder in
// another spelling where that may be the same folder, usb for a root
// configured as USB, as what is spared compares (caseLook.under; B651's sixth
// and seventh rounds). The root's alias stays, so an event already queued
// under the name it resolved to is still named back under the root, and
// dropped as a held root's; watchAgain notes it afresh.
func (wt *Watcher) releaseRoot(root string) {
	rw := wt.rootWatchPath(root)
	delete(wt.rootWatches, root)
	delete(wt.rootIDs, root)
	c := wt.scanner.caseLook()
	if wt.fds != nil {
		if !wt.fds.off {
			same := func(q string) bool {
				tail, ok := c.under(rw, q, true)
				return ok && tail == "."
			}
			for _, p := range wt.fds.spellings(rw, same) {
				wt.releaseTree(p)
				wt.fds.dropPending(p)
			}
		}
		return
	}
	for p := range wt.watchSet() {
		if pathAtOrUnder(p, rw) {
			_ = wt.w.Remove(p)
		} else if _, ok := c.under(rw, p, true); ok {
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
// of it: nothing saw what changed in it while it was not watched. What is
// held at or under the root's path goes first (releaseRoot): it was opened on
// what was there then. On kqueue that includes a held root's bare mount
// point, which fsnotify opened as an entry when it listed the folder above,
// and fsnotify's Add of the root finds that descriptor by its path and keeps
// it, on the folder a volume mounted since covers, so nothing made directly
// in the root was seen while its folders below, opened through the path, were
// (B651's ninth round, Codex on round 8). fsnotify forgets having seen the
// path with it, so the next change in the folder above sends a Create for the
// root, which asks for a scan of that folder. The identity of the folder at the
// root's path is taken just before the walk and recorded with the root's own
// watch (noteRootWatch), so a later change of it is found too; where it changed
// while the walk ran, the watch is added once more (backlog B777). A root
// whose own watch fsnotify would not add after all (the watch limit spent, a
// permission) is tried again on a backoff of its own (unwatchRoot, which asks
// for the scan the first time alone, and B777's round 2), and one that takes
// its watch leaves it. It answers the error of a root whose tree could not be
// added after all (it went again between the look and the walk, or, on
// kqueue, took the watcher past its budget, which stops the watcher for good).
func (wt *Watcher) watchAgain(ctx context.Context, root string) error {
	if wt.fds != nil && wt.fds.off {
		return errWatchBudget
	}
	var live map[string]struct{}
	for attempt := 1; ; attempt++ {
		wt.releaseRoot(root)
		wt.aliases = slices.DeleteFunc(wt.aliases, func(a rootAlias) bool { return a.configured == root })
		pre := wt.rootIdentities([]string{root})[root]
		if err := wt.addTree(root, true); err != nil {
			return err
		}
		live = wt.watchSet()
		if wt.noteRootWatch(root, pre, live) || attempt == 2 {
			break
		}
	}
	if _, installed := live[wt.rootWatchPath(root)]; !installed {
		wt.unwatchRoot(ctx, root, msgRootWatchNotAdded)
		return nil
	}
	delete(wt.unwatched, root)
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
	// A root whose own watch could not be added was said so (unwatchRoot).
	if !wt.unwatched[root] {
		watcherLogger.Info(msgWatchRootResumed, "root", root)
	}
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
// watches goes, and it is watched again where it is now, with a scan. A root
// whose Create's look waits for the flush is left to it: fsnotify may still be
// opening the folder at its path, which is why that look waits. A root whose
// own watch fsnotify holds is compared with the folder at its path now
// (checkRootIdentity, backlog B777).
func (wt *Watcher) checkRootWatches(ctx context.Context) {
	if hook := wt.afterRootCheckForTests; hook != nil {
		defer hook()
	}
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
		// A root whose own watch could not be added is its retry's
		// (retryUnwatched), on a backoff of its own, never a check's.
		if wt.fds.lookWaits(root) || wt.unwatched[root] {
			continue
		}
		if !held[wt.rootWatches[root]] {
			wt.lookAgain(ctx, root, msgWatchRootRewatched)
			continue
		}
		wt.checkRootIdentity(ctx, root)
	}
}

// checkRootIdentity looks again at a watched root whose own watch fsnotify
// holds, where the folder at its path is another than the one that watch was
// added on (rootIDs, os.SameFile; backlog B777): a volume mounted over the
// folder the root was watched on, a volume unmounted and mounted again (kqueue
// keeps watching, and listing, its first mount's revoked folders; inotify drops
// an unmounted volume's watches, which the check finds missing), a linked
// root's link repointed. None of these is anything fsnotify says. lookAgain
// holds a root that then reads as a volume not mounted, as holdAbsentRoots
// holds one, so a volume away is looked at by the hold's retry and never
// watched again at every check, and watches one that reads as it did again
// where it is now, recording that folder's identity. An identity that cannot be
// read is no change: the root is held where it reads as a volume not mounted,
// and otherwise left as it is until the next check. A root recorded with no
// identity (its folder could not be read as its watch was added) takes one
// once its folder reads (takeRootIdentity).
func (wt *Watcher) checkRootIdentity(ctx context.Context, root string) {
	want, known := wt.rootIDs[root]
	if !known {
		wt.takeRootIdentity(ctx, root)
		return
	}
	now, err := wt.rootIdentity(root)
	if err != nil {
		if look := wt.scanner.lookAtRoot(ctx, root); look.absent && ctx.Err() == nil {
			wt.holdRoot(root, look)
		}
		return
	}
	if !os.SameFile(want, now) {
		wt.lookAgain(ctx, root, msgWatchRootReplaced)
	}
}

// takeRootIdentity is the check of a watched root recorded with no identity,
// its folder unreadable as its own watch was added (B777's round 2): every
// check returned there, for good, though the reads answered again. Once the
// folder reads, the root is looked at again and its watch added again around
// the read (lookAgain, watchAgain), never the identity recorded against the
// watch it has, which may be on a folder another has covered since (a volume
// mounted, a link repointed while the reads failed): the identity recorded is
// of the folder the new watch is on, with a scan of it. A folder that still
// does not read is left to the next check, which reads it again and nothing
// more.
func (wt *Watcher) takeRootIdentity(ctx context.Context, root string) {
	if _, err := wt.rootIdentity(root); err != nil {
		return
	}
	wt.lookAgain(ctx, root, msgWatchRootIdentityTaken)
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
	if err := wt.watchAgain(ctx, root); err != nil {
		if wt.fds == nil || !wt.fds.off {
			wt.holdRoot(root, rootLook{absent: true, reason: rootAbsentUnreachable, err: err})
		}
		return
	}
	// A root whose own watch could not be added was said so (unwatchRoot).
	if !wt.unwatched[root] {
		watcherLogger.Info(msg, "root", root)
	}
}

// lookAgainAfterCreate is the look a Create of root asked for while root's own
// watch was gone (handleEvent), taken on kqueue once fsnotify has watched the
// folder now at root's path itself (flushPendingAdds), and only while the
// handler would still take it: not for a root held since (its events are
// dropped, and its retry looks at it), nor for one whose own watch is
// fsnotify's again (something since watched it again).
func (wt *Watcher) lookAgainAfterCreate(ctx context.Context, root string) {
	if _, held := wt.held[root]; held {
		return
	}
	if rw, watched := wt.rootWatches[root]; watched {
		if _, installed := wt.watchSet()[rw]; installed {
			return
		}
	}
	wt.lookAgain(ctx, root, msgWatchRootRewatched)
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
