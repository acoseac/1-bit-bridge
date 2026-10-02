package manifest

import (
	"errors"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"
)

// kqueue (macOS, and the BSDs, where fsnotify watches through it) makes every
// watch an open file: asked to watch a folder, fsnotify opens the folder and
// then every entry in it, files included (watchDirectoryFiles), one
// descriptor each. Measured through this watcher: 132 descriptors for 12
// folders holding 120 files. They come out of the open-file limit every
// socket, SQLite file, pipe and scan of the bridge comes out of, so a library
// larger than that limit left nothing for the rest, and every later open and
// accept failed, the periodic scan's included (backlog B212). inotify and
// ReadDirectoryChangesW hold no descriptor per watched file, so the account
// below is kept only where kqueueBackend says fsnotify uses kqueue.
//
// The account does four things. It budgets the watches (WatchFDLimits.Budget):
// a library that needs more than the budget is not watched at all, and one
// that grows past it while watched has every watch released, with one
// warning either way; the periodic scan picks up changes. It releases what
// fsnotify keeps open after a folder is renamed or moved away: fsnotify drops
// the folder's own watch and keeps a descriptor on every entry in it, for
// good, under paths that no longer exist (measured: every rename of an album
// folder kept one per file). And it keeps the watcher from opening a new
// folder twice: fsnotify sends a new folder's Create and then watches the
// folder itself, on its own goroutine, while the Create's handler asks for
// the same folder, and when the two race, fsnotify keeps one descriptor and
// loses track of the other, which nothing closes, Close included (measured:
// 9 or 10 of every 10 new folders). So a new folder is watched once fsnotify
// has sent a later event, which it sends only after watching the folder
// itself, or after a quiet spell (deferredAddWait). And it reconciles, for
// the folder fsnotify drops without sending anything (reconcileWatches).

// watchShareOfProcess and watchShareOfSystem divide the open-file limits into
// the share the watcher's watches may take: half of what this process may
// hold open, and a quarter of the system's open-file table, whichever is
// less. The
// rest of the bridge needs a few hundred at most (41 when idle, measured:
// SQLite's files, the listeners, the logs, the poller), bounded by its
// pools, so half of the macOS default (61,440, as Go raises it at start)
// leaves it two orders of magnitude to spare. Not the 80% Linux grants
// inotify: inotify watches are a budget of their own, while every kqueue
// watch is a slot in the table everything else opens into, and a spike there
// (a full scan on every worker, a burst of clients, the render pipes) must
// not find it full. The system cap matters only when kern.maxfilesperproc is
// raised to kern.maxfiles, where half of the process's limit would be half
// of every process's table; with the macOS defaults the two give the same
// 30,720.
const (
	watchShareOfProcess = 2
	watchShareOfSystem  = 4
)

// deferredAddWait is how long a new folder waits for fsnotify to send a later
// event before the watcher asks for it anyway (see flushPendingAdds). fsnotify
// watches the folder itself in microseconds after sending its Create, so a
// quarter second leaves it ample time; it is kept under the shortest scan
// debounce (a second), so the folder is watched before the scan its Create
// asked for walks it, and nothing that lands in it after that scan goes
// unseen.
const deferredAddWait = 250 * time.Millisecond

// reconcileEvery is how often the watcher compares the folders it listed
// with the ones fsnotify still watches (reconcileWatches).
const reconcileEvery = 30 * time.Second

// Messages and the hint the kqueue account logs, named so tests can find
// them.
const (
	msgWatcherStaysOff   = "library watcher stays off: it would hold an open file for every folder and file it watches, more than its share of the open-file limit; the periodic scan picks up changes"
	msgWatcherStopped    = "library watcher stopped: the library grew past its share of the open-file limit (it holds an open file for every folder and file it watches); it released its watches, and the periodic scan picks up changes"
	msgWatcherOutOfFiles = "library watcher stopped: this process ran out of open files while the watcher added watches; it released them, and the periodic scan picks up changes"
	msgWatcherNoLimit    = "library watcher stays off: the open-file limit its watches are budgeted against could not be read; the periodic scan picks up changes"
	watchFDHint          = "the watcher may hold half of this process's open-file limit and a quarter of the system's; to watch a larger library, raise kern.maxfilesperproc and kern.maxfiles (sysctl, as root) and restart the bridge"
)

// errWatchBudget is what planning a tree answers when its watches would take
// the watcher past its budget.
var errWatchBudget = errors.New("the library watcher's share of the open-file limit is spent")

// ErrNoWatchFDBudget is what WatchFileLimits answers where fsnotify does not
// hold a file per watch.
var ErrNoWatchFDBudget = errors.New("the library watcher holds no open file per watch on this platform")

// WatchFDLimits are the open-file limits a kqueue watcher's budget is taken
// from.
type WatchFDLimits struct {
	// Process is the most files this process may hold open: the soft
	// RLIMIT_NOFILE (which Go raises at start to the hard limit less one, on
	// macOS to kern.maxfilesperproc at most), and never above
	// kern.maxfilesperproc where that can be read.
	Process int
	// System is the system's open-file table, kern.maxfiles, which every
	// process shares; 0 where it cannot be read.
	System int
}

// Budget is the most open files the library watcher's watches may take:
// Process divided by watchShareOfProcess, and System divided by
// watchShareOfSystem at most.
func (l WatchFDLimits) Budget() int {
	b := l.Process / watchShareOfProcess
	if l.System > 0 && l.System/watchShareOfSystem < b {
		b = l.System / watchShareOfSystem
	}
	return b
}

// SystemCapped reports whether Budget is the system's cap rather than the
// process's.
func (l WatchFDLimits) SystemCapped() bool {
	return l.System > 0 && l.System/watchShareOfSystem < l.Process/watchShareOfProcess
}

// clampFileLimit turns a raw rlimit or sysctl value into an int a budget can
// be taken from: RLIM_INFINITY and anything past what an int32 holds read as
// math.MaxInt32, which no process reaches.
func clampFileLimit(v uint64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(v)
}

// WatchSetCount is what the library watcher would hold open for a set of
// roots on a kqueue platform: a watch on every folder it lists, and one on
// every entry in such a folder (a file, a link, a folder it skips).
type WatchSetCount struct {
	Folders int
	Entries int
	// Partial says the count stopped at the stopAt it was given, so the
	// library holds at least this many.
	Partial bool
}

// OpenFiles is the number of open files the count stands for.
func (c WatchSetCount) OpenFiles() int { return c.Folders + c.Entries }

// CountWatchSet counts what the library watcher would hold open for roots on
// a kqueue platform, by the walk the watcher plans its watches with
// (walkWatchTree), so the two cannot disagree: bridge doctor's pre-flight,
// and Run's own decision whether the library fits its budget. It stops once
// the count reaches stopAt (when stopAt > 0), and answers a root it cannot
// walk with its error.
func CountWatchSet(roots []string, stopAt int) (WatchSetCount, error) {
	var c WatchSetCount
	for _, root := range roots {
		_, err := walkWatchTree(root, true, func(_ string, d fs.DirEntry, watch bool) error {
			if !kqueueHoldsEntry(d.Type()) {
				return nil
			}
			if watch {
				c.Folders++
			} else {
				c.Entries++
			}
			if stopAt > 0 && c.OpenFiles() >= stopAt {
				c.Partial = true
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return c, err
		}
		if c.Partial {
			break
		}
	}
	return c, nil
}

// kqueueHoldsEntry reports whether fsnotify's kqueue backend opens an entry of
// this type when it lists a folder: it skips sockets and named pipes, and
// opens everything else (a link through to its target).
func kqueueHoldsEntry(t fs.FileMode) bool {
	return t&(fs.ModeSocket|fs.ModeNamedPipe) == 0
}

// watchFDs is a kqueue watcher's account of the open files its watches hold,
// kept on Run's goroutine alone: the paths fsnotify holds a watch on, by the
// folder it listed them in, so a renamed folder's whole subtree can be
// released, and their number, which the budget is checked against.
type watchFDs struct {
	limits   WatchFDLimits
	limitErr error
	budget   int
	// kids maps every folder the watcher listed (asked fsnotify to watch,
	// which opened it and every entry in it) to the paths fsnotify holds in
	// it.
	kids map[string]map[string]struct{}
	// tops holds the paths fsnotify holds whose folder the watcher did not
	// list: the roots, and a folder whose own watch failed after its
	// parent's listing had opened it.
	tops map[string]struct{}
	n    int
	// pending holds the new folders that wait for their watch
	// (flushPendingAdds).
	pending map[string]struct{}
	// off says the watcher has released its watches for good.
	off bool
}

func newWatchFDs(limits WatchFDLimits, limitErr error) *watchFDs {
	return &watchFDs{
		limits:   limits,
		limitErr: limitErr,
		budget:   limits.Budget(),
		kids:     make(map[string]map[string]struct{}),
		tops:     make(map[string]struct{}),
		pending:  make(map[string]struct{}),
	}
}

// held reports whether the account holds p.
func (f *watchFDs) held(p string) bool {
	if _, ok := f.tops[p]; ok {
		return true
	}
	_, ok := f.kids[filepath.Dir(p)][p]
	return ok
}

// hold records that fsnotify holds a watch on p.
func (f *watchFDs) hold(p string) {
	if f.held(p) {
		return
	}
	if k := f.kids[filepath.Dir(p)]; k != nil {
		k[p] = struct{}{}
	} else {
		f.tops[p] = struct{}{}
	}
	f.n++
}

// list records that fsnotify listed dir: what it holds in it is recorded
// against it from now on.
func (f *watchFDs) list(dir string) {
	if f.kids[dir] == nil {
		f.kids[dir] = make(map[string]struct{})
	}
}

// listed reports whether fsnotify listed dir.
func (f *watchFDs) listed(dir string) bool { return f.kids[dir] != nil }

// release forgets p and everything recorded below it, and returns those paths,
// p first, for the watcher to remove from fsnotify: after a rename or a
// removal fsnotify has dropped p's own watch and none of the ones below it.
func (f *watchFDs) release(p string) []string {
	out := []string{p}
	if _, ok := f.tops[p]; ok {
		delete(f.tops, p)
		f.n--
	} else if k := f.kids[filepath.Dir(p)]; k != nil {
		if _, ok := k[p]; ok {
			delete(k, p)
			f.n--
		}
	}
	var drop func(dir string)
	drop = func(dir string) {
		k, ok := f.kids[dir]
		if !ok {
			return
		}
		delete(f.kids, dir)
		for c := range k {
			f.n--
			out = append(out, c)
			drop(c)
		}
	}
	drop(p)
	return out
}

// dropPending forgets the new folders at or below p that wait for their watch.
func (f *watchFDs) dropPending(p string) {
	for d := range f.pending {
		if pathAtOrUnder(d, p) {
			delete(f.pending, d)
		}
	}
}

// pendingInOrder is the waiting folders, a parent ahead of what is in it.
func (f *watchFDs) pendingInOrder() []string {
	return slices.Sorted(maps.Keys(f.pending))
}

// forget empties the account, once every watch is released.
func (f *watchFDs) forget() {
	clear(f.kids)
	clear(f.tops)
	clear(f.pending)
	f.n = 0
}

// budgetArgs follows a warning's own counts with the budget, the limits it
// was taken from, and the hint.
func (f *watchFDs) budgetArgs(counts ...any) []any {
	args := append(counts, "budget", f.budget, "open_file_limit", f.limits.Process)
	if f.limits.System > 0 {
		args = append(args, "system_open_file_limit", f.limits.System)
	}
	return append(args, "hint", watchFDHint)
}

// wait records a new folder whose watch waits for fsnotify to have watched
// it itself (flushPendingAdds).
func (f *watchFDs) wait(dir string) {
	if !f.off {
		f.pending[filepath.Clean(dir)] = struct{}{}
	}
}

// plannedWatch is a path asking for a tree's watches makes fsnotify hold:
// a folder the watcher asks it to watch (watch), or an entry fsnotify opens
// in the folder ahead of it.
type plannedWatch struct {
	path  string
	watch bool
}

// watchRootsWithinBudget is Run's start where fsnotify holds an open file
// per watched entry: it counts what the library would hold open, by the walk
// bridge doctor counts with (CountWatchSet), stays off with one warning when
// that is more than the budget, and adds every root's tree otherwise. It
// reports whether the watcher is on. The count runs to the end, so the
// warning names what the library needs: it holds no path, and it runs once,
// on the watcher's goroutine. A root that cannot be walked is left to
// addTree, which meets the same failure and logs it, as on every platform.
func (wt *Watcher) watchRootsWithinBudget(roots []string) bool {
	f := wt.fds
	if f.limitErr != nil {
		watcherLogger.Warn(msgWatcherNoLimit, "err", f.limitErr, "hint", watchFDHint)
		return false
	}
	var total WatchSetCount
	for _, root := range roots {
		c, err := CountWatchSet([]string{root}, 0)
		if err != nil {
			continue
		}
		total.Folders += c.Folders
		total.Entries += c.Entries
	}
	if total.OpenFiles() > f.budget {
		watcherLogger.Warn(msgWatcherStaysOff, f.budgetArgs("open_files_needed", total.OpenFiles(),
			"folders", total.Folders, "entries", total.Entries)...)
		return false
	}
	for _, root := range roots {
		if err := wt.addTree(root, true); err != nil && !f.off {
			watcherLogger.Warn("initial watch add failed (partial coverage; periodic scan still runs)",
				"root", root, "err", err)
		}
		if f.off {
			return false
		}
	}
	return true
}

// addTreeWithinBudget is addTree where fsnotify holds an open file per watched
// entry. It plans the tree's watches (planWatches) and asks for them only
// when they fit what is left of the budget; past it, or when this process
// runs out of open files on the way, it releases every watch and the watcher
// stays off (stopWatching), which its error says.
func (wt *Watcher) addTreeWithinBudget(root string, isConfiguredRoot bool) error {
	f := wt.fds
	if f.off {
		return errWatchBudget
	}
	plan, rootWatch, err := wt.planWatches(root, isConfiguredRoot)
	wt.noteAlias(root, rootWatch)
	if errors.Is(err, errWatchBudget) {
		wt.stopWatching(msgWatcherStopped, f.budgetArgs("open_files_needed_more_than", f.budget)...)
		return err
	}
	if err != nil {
		return err
	}
	for _, p := range plan {
		if !p.watch {
			// fsnotify opened it when it listed its folder, and only then.
			if f.listed(filepath.Dir(p.path)) {
				f.hold(p.path)
			}
			continue
		}
		if f.listed(p.path) {
			continue
		}
		res, addErr := wt.addWatch(p.path)
		if res == watchOutOfFiles {
			wt.stopWatching(msgWatcherOutOfFiles, f.budgetArgs("path", p.path, "err", addErr, "open_files_held", f.n)...)
			return addErr
		}
		// Held either way: a folder fsnotify could not list may still be
		// open as an entry of the folder that holds it, and counting one it
		// does not hold costs only budget.
		f.hold(p.path)
		if res == watchAdded {
			f.list(p.path)
		}
	}
	return nil
}

// planWatches walks a tree as addTree would (walkWatchTree) and lists what
// asking for its watches makes fsnotify hold, in walk order, each folder
// ahead of the entries in it. The walk stops with errWatchBudget once the
// paths the account does not hold yet pass what is left of the budget.
func (wt *Watcher) planWatches(root string, isConfiguredRoot bool) (plan []plannedWatch, rootWatch string, err error) {
	f := wt.fds
	room, need := f.budget-f.n, 0
	rootWatch, err = walkWatchTree(root, isConfiguredRoot, func(path string, d fs.DirEntry, watch bool) error {
		if !kqueueHoldsEntry(d.Type()) {
			return nil
		}
		path = filepath.Clean(path)
		if !f.held(path) {
			need++
			if need > room {
				return errWatchBudget
			}
		}
		plan = append(plan, plannedWatch{path: path, watch: watch})
		return nil
	})
	return plan, rootWatch, err
}

// flushPendingAdds watches the new folders that wait for it. Run calls it as
// soon as fsnotify has sent another event (or an error), which it sends only
// after it has watched, itself, every folder it sent a Create for before: a
// folder asked for while fsnotify watches it was opened twice, and fsnotify
// kept track of one of the two descriptors only (measured: 9 or 10 of every
// 10 new folders, each a descriptor nothing closes, Close included). With no
// event to come, Run calls it after deferredAddWait. A folder that has gone,
// or cannot be read, is left to the scan its Create asked for.
func (wt *Watcher) flushPendingAdds() {
	f := wt.fds
	if f == nil || f.off || len(f.pending) == 0 {
		return
	}
	dirs := f.pendingInOrder()
	clear(f.pending)
	for _, d := range dirs {
		_ = wt.addTree(d, false)
		if f.off {
			return
		}
	}
}

// accountEvent keeps the account in step with what fsnotify holds once it
// has sent ev. A rename or a removal: fsnotify has dropped the path's own
// watch and, where the path is a folder, kept every watch below it, on paths
// that are gone, and the watcher removes them (measured: renaming an album
// folder kept a descriptor open for every file in it, for good). A Create:
// fsnotify watches the new path itself right after sending it, and the
// watcher stops, releasing every watch, when that takes it past its budget.
func (wt *Watcher) accountEvent(ev fsnotify.Event) {
	f := wt.fds
	if f.off {
		return
	}
	name := filepath.Clean(ev.Name)
	if ev.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
		wt.releaseTree(name)
		f.dropPending(name)
	}
	if ev.Op&fsnotify.Create != 0 {
		info, err := os.Lstat(name)
		if err != nil || !kqueueHoldsEntry(info.Mode().Type()) {
			return
		}
		f.hold(name)
		if f.n > f.budget {
			wt.stopWatching(msgWatcherStopped, f.budgetArgs("open_files_needed", f.n)...)
		}
	}
}

// releaseTree forgets p and everything recorded below it, and removes their
// watches from fsnotify. fsnotify answers a path it no longer watches with an
// error, which says nothing here.
func (wt *Watcher) releaseTree(p string) {
	for _, q := range wt.fds.release(p) {
		_ = wt.w.Remove(q)
	}
}

// reconcileWatches releases what is recorded under every folder the watcher
// listed that fsnotify no longer watches. fsnotify drops a folder's watch,
// and keeps every watch in it, without sending anything when the folder is
// renamed or moved away after an entry of it changed and before fsnotify
// read that change (measured: an album folder that gained a file and was
// then renamed while nothing read fsnotify's events reached the reader as no
// event at all), so no event says to release them. Run calls it every
// reconcileEvery.
func (wt *Watcher) reconcileWatches() {
	f := wt.fds
	if f == nil || f.off {
		return
	}
	if len(f.kids) > 0 {
		live := make(map[string]struct{}, len(f.kids))
		for _, p := range wt.w.WatchList() {
			live[filepath.Clean(p)] = struct{}{}
		}
		for d := range f.kids {
			if _, ok := live[d]; !ok {
				wt.releaseTree(d)
			}
		}
	}
	if hook := wt.afterReconcileForTests; hook != nil {
		hook()
	}
}

// stopWatching releases every watch and keeps the watcher off for good,
// saying why once: fsnotify's Close closes every descriptor it keeps track
// of. Run then waits for its context alone, so a scan an event already asked
// for still runs.
func (wt *Watcher) stopWatching(msg string, args ...any) {
	f := wt.fds
	_ = wt.w.Close()
	f.off = true
	f.forget()
	watcherLogger.Warn(msg, args...)
}
