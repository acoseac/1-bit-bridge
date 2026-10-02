package manifest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var watcherLogger = logging.Component("watcher")

// pendingScan is one armed debounce entry. Wrapping the timer in a
// pointer struct gives each AfterFunc callback a STABLE IDENTITY to
// compare against wt.pending[dir]. time.Timer.Reset/Stop cannot cancel a
// callback that has already been dispatched, so a timer that fired but
// whose callback hasn't yet re-acquired wt.mu must NOT delete a fresh
// entry a concurrent scheduleScan installed in the meantime. Without the
// identity check the stale callback evicts the live entry, and under a
// sustained event storm the map loses track of the current timer per dir
// → unbounded timer creation + overlapping ScanSubtree dispatches.
type pendingScan struct {
	timer *time.Timer
}

// Watcher is the optional fsnotify-based instant-update layer.
// Off by default in config (LibraryWatchConfig.Enabled). When on,
// it adds a recursive watch over every configured library root and
// triggers a debounced ScanSubtree against the affected directory
// when files are created / written / renamed under it.
//
// The periodic full scan (Scanner.RunPeriodic) remains the safety
// net regardless: missed events (kernel limit hit, watcher crash,
// rapid rename storm) get reconciled on the next tick. The watcher
// is a "good UX in the common case" layer, not a correctness path.
// That holds only while the watcher leaves the rest of the bridge the
// files it needs: on kqueue (macOS) every watched folder AND every file
// in it is an open file, so the watcher there keeps to a budget and
// releases what fsnotify would otherwise keep open (watcher_fds.go).
//
// Concurrency: Run() owns one goroutine for the fsnotify event
// loop and spawns one fire-and-forget goroutine per debounced
// dispatch. The debounce map is mutex-protected; scan invocations
// serialise via Scanner's own s.mu.
type Watcher struct {
	scanner  *Scanner
	debounce time.Duration
	w        *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]*pendingScan
	// closing is set (under mu) at shutdown so a debounce timer that fires
	// during teardown skips its scan instead of racing the store close.
	// scanWG tracks the in-flight AfterFunc scan dispatches (each is its own
	// goroutine) so Run() can wait for them before returning — otherwise an
	// in-flight ScanSubtree mid UpsertTrackBatch could run while the caller's
	// deferred Store.Close() executes (B8, the SQLite-corruption class).
	closing bool
	scanWG  sync.WaitGroup

	// afterDispatchHookForTests fires at the tail of a debounced dispatch
	// — after ScanSubtree has returned (so its writes have landed) but
	// BEFORE the deferred scanWG.Done(), which is the only window in which
	// a dispatch is provably still counted as in-flight. That window is
	// what makes the shutdown drain testable at all: without it a test can
	// only sleep and hope, and a sleep that is slightly too LONG lets the
	// scan finish, drops scanWG to zero, and passes without exercising the
	// wait at all.
	//
	// Per-INSTANCE, not a package-level var — the same call the transcode
	// Pool's jobTimeout seam makes, and for the same reason, learned here
	// the hard way: as a package var this raced under -race, because a
	// dispatch goroutine left over from an EARLIER watcher test reads it
	// with no happens-before edge to the next test's assignment. Set before
	// Run starts (goroutine creation orders the write); nil in production,
	// one nil check per debounced directory change.
	afterDispatchHookForTests func()

	// afterInitialWalkHookForTests fires once Run's initial walk has
	// registered every watch it is going to, before the event loop starts:
	// a file created before then is one no watch sees, and nothing but the
	// periodic scan picks it up. A test that drops a file waits on it
	// (startWatcher), where it slept a guess at how long the walk takes,
	// and a starved host walked past the guess (backlog B104). Per-instance
	// and set before Run starts, for afterDispatchHookForTests' reason; nil
	// in production.
	afterInitialWalkHookForTests func()

	// aliases are the configured roots that are links, each watched at the
	// directory it resolves to (watchWalkStart), paired with the root as
	// configured so an event is named back under it (configuredName).
	// Written by Run's initial walk and read by handleEvent, both on Run's
	// goroutine, so no lock.
	aliases []rootAlias

	// fds is the account of the open files the watches hold where fsnotify
	// holds one per watched folder and per entry in it (kqueue,
	// watcher_fds.go), read and written on Run's goroutine alone; nil
	// elsewhere.
	fds *watchFDs
	// deferredAddWait and reconcileEvery are the package constants of those
	// names, per instance so a test can shorten them before Run starts.
	deferredAddWait time.Duration
	reconcileEvery  time.Duration

	// dropEventForTests, when it answers true, has Run act as though
	// fsnotify had never sent the event: a test stands in that way for an
	// event fsnotify swallows (a folder renamed while its listing changed
	// drops the folder's watch and sends nothing). afterReconcileForTests
	// fires after each reconcile pass. Both per instance and set before Run
	// starts, for afterDispatchHookForTests' reason; nil in production.
	dropEventForTests      func(fsnotify.Event) bool
	afterReconcileForTests func()
}

// rootAlias pairs the directory a linked configured root resolves to with
// the root as configured.
type rootAlias struct {
	resolved   string
	configured string
}

// NewWatcher constructs a Watcher against the scanner's currently
// configured roots. `debounce` is the per-directory event coalesce
// window — picks the configured value or the default if zero.
//
// Returns an error if fsnotify can't be initialised at all (e.g.
// older kernel without inotify support; in practice every supported
// platform satisfies this). Per-root Add failures during Run() are
// non-fatal and surface as Warn logs.
func NewWatcher(scanner *Scanner, debounce time.Duration) (*Watcher, error) {
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	wt := &Watcher{
		scanner:         scanner,
		debounce:        debounce,
		w:               w,
		pending:         make(map[string]*pendingScan),
		deferredAddWait: deferredAddWait,
		reconcileEvery:  reconcileEvery,
	}
	if kqueueBackend {
		wt.fds = newWatchFDs(WatchFileLimits())
	}
	return wt, nil
}

// Run starts the watch loop. Walks every configured root, adds a
// watch on every directory under each root (one Add per directory on
// every platform: the watcher asks fsnotify for no recursive watch),
// then loops on events until ctx is cancelled. On macOS fsnotify
// watches through kqueue, not FSEvents, and kqueue makes every
// watched directory and every entry in one an open file of this
// process, so there the watcher first counts what the library would
// hold open and stays off, with one warning, when that is more than
// its budget (watchRootsWithinBudget, watcher_fds.go).
//
// Linux watch-limit handling: any fsnotify Add() that fails with
// ENOSPC ("too many watches") logs a single Error with the
// directory count walked so far and a hint to raise
// `fs.inotify.max_user_watches`, then continues with a partial
// watch set. The doctor check pre-flags this case so operators
// see it before runtime; this fallback exists for the case where
// the limit is hit AFTER the operator passed doctor (e.g. another
// app on the host claimed a chunk of inotify watches between
// `bridge doctor` and `bridge serve`).
//
// On Create events for directories, the watcher recursively adds
// watches for the new subtree so newly-mkdir'd folders inside an
// already-watched root get picked up automatically.
func (wt *Watcher) Run(ctx context.Context) error {
	// Stop armed debounce timers on EVERY exit path — ctx cancel AND the
	// fsnotify Events/Errors channels closing (an internal fatal error) —
	// not just the ctx-cancel branch. Without this, the `!ok` returns
	// below would leave armed timers running that fire ScanSubtree after
	// Run has already returned. cancelAllPending is idempotent, so the
	// defer is safe on every path.
	defer func() {
		wt.cancelAllPending()     // stop un-fired timers
		wt.waitForInflightScans() // wait for already-fired ScanSubtree dispatches (B8)
	}()
	defer wt.w.Close()

	roots := wt.scanner.Roots()
	if wt.fds != nil {
		if !wt.watchRootsWithinBudget(roots) {
			// Logged: the watcher stays off, holding nothing, and the
			// periodic scan picks up changes. No scan is pending yet.
			return nil
		}
	} else {
		for _, root := range roots {
			if err := wt.addTree(root, true); err != nil {
				watcherLogger.Warn("initial watch add failed (partial coverage; periodic scan still runs)",
					"root", root, "err", err)
			}
		}
	}
	if hook := wt.afterInitialWalkHookForTests; hook != nil {
		hook()
	}

	events, errs := wt.w.Events, wt.w.Errors
	// quiet fires when new folders have waited deferredAddWait for their
	// watch, and reconcile on the account's schedule; both stay nil where
	// fsnotify holds no file per watch, so the loop there is what it was.
	var quiet, reconcile <-chan time.Time
	if wt.fds != nil {
		t := time.NewTicker(wt.reconcileEvery)
		defer t.Stop()
		reconcile = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			// fsnotify sends each event only after it has watched every
			// new folder it sent a Create for before, so the folders
			// waiting for their watch can have it now.
			wt.flushPendingAdds()
			quiet = nil
			if drop := wt.dropEventForTests; drop != nil && drop(ev) {
				break
			}
			wt.handleEvent(ctx, ev)
		case err, ok := <-errs:
			if !ok {
				return nil
			}
			wt.flushPendingAdds()
			quiet = nil
			watcherLogger.Warn("fsnotify error", "err", err)
		case <-quiet:
			quiet = nil
			wt.flushPendingAdds()
		case <-reconcile:
			wt.reconcileWatches()
		}
		if f := wt.fds; f != nil {
			if f.off {
				// Every watch is released. Run stays until ctx is done,
				// so a scan an event already asked for still runs, and
				// still finishes before the caller closes the store.
				events, errs, quiet, reconcile = nil, nil, nil, nil
				continue
			}
			if len(f.pending) > 0 && quiet == nil {
				quiet = time.After(wt.deferredAddWait)
			}
		}
	}
}

// addTree adds a watch on every directory of the tree at root, as
// walkWatchTree walks it. Stops walking on the first ENOSPC ("watch
// limit reached") to avoid spamming the log once per directory — the
// operator gets one clear signal that the kernel limit needs raising.
// Where fsnotify holds an open file per watched entry (kqueue) the tree
// is planned and checked against the watcher's budget first
// (addTreeWithinBudget, watcher_fds.go).
func (wt *Watcher) addTree(root string, isConfiguredRoot bool) error {
	if wt.fds != nil {
		return wt.addTreeWithinBudget(root, isConfiguredRoot)
	}
	limitHit := false
	rootWatch, err := walkWatchTree(root, isConfiguredRoot, func(path string, _ fs.DirEntry, watch bool) error {
		if !watch {
			return nil
		}
		if limitHit {
			// Every subsequent Add would fail the same way, so there
			// is nothing left to do — SkipAll stops the walk instead
			// of stat-ing the rest of the tree for no benefit.
			// (SkipDir would be wrong: it only prunes descendants and
			// the walk would continue through every sibling.) The
			// periodic full scan covers the gap.
			return filepath.SkipAll
		}
		res, _ := wt.addWatch(path)
		limitHit = res == watchLimitReached
		return nil
	})
	wt.noteAlias(root, rootWatch)
	return err
}

// noteAlias records a configured root that is watched where it resolves to,
// so an event under it is named back under the root (configuredName).
func (wt *Watcher) noteAlias(root, rootWatch string) {
	if rootWatch != "" && rootWatch != root {
		wt.aliases = append(wt.aliases, rootAlias{resolved: rootWatch, configured: root})
	}
}

// walkWatchTree walks the tree the watcher watches from root and hands visit
// every path it reaches: watch is true for a directory the watcher asks
// fsnotify to watch, and false for everything else the walk reaches in such a
// directory (a file, a link, a folder ShouldSkipDir names, which it does not
// enter). It is the one walk: addTree, the kqueue budget's plan
// (planWatches) and CountWatchSet (bridge doctor's pre-flight) all go through
// it, so what the watcher watches and what it counts cannot disagree. visit
// may answer filepath.SkipAll to stop the walk, or an error to end it with
// that error. It returns the path the root's own watch is registered as.
//
// `isConfiguredRoot` says whether `root` is an operator-configured
// library root (Run's startup pass) or a directory the watcher just
// saw appear at runtime (handleEvent's Create branch). It gates the
// dot-directory carve-out, and the distinction is load-bearing: the
// exemption exists so an operator who configures
// `/mnt/storage/.music` as a library root gets it watched, and it must
// apply to THAT decision only. Keyed on `path != root` alone it also
// exempted every runtime-created directory from itself, so a
// freshly-appeared `.Trashes` / `.stversions` got a watch, a later
// event under it dispatched ScanSubtree INSIDE it (whose own walker
// exempts the directory it was pointed at), and its files were indexed
// as `.Trashes/501/Album/track.flac`. The full Scan never sees those
// paths — ShouldSkipDir prunes them as descendants — so they accrued
// missing_count and were reaped three scans later, then reappeared:
// deleted albums cycling in and out of /v1/manifest.
//
// **Root-level walk failure surfaces** (CodeRabbit Major post-merge
// on PR #83): a permission/missing/IO error AT the root path
// itself produces an err callback with `path == root`. Pre-fix,
// the per-callsite `return nil` swallowed it and the caller
// reported success with zero watches registered — an entire
// library could lose instant-update coverage with no signal.
// Now we surface it as an error, and the caller in `Run()` logs
// "initial watch add failed (partial coverage)" so the operator
// at least knows.
//
// A configured root that is itself a link to a directory (or on Windows a
// junction) is walked THROUGH, as the scanner walks it: walked as the link,
// the root was one entry that is not a directory, so not a single watch was
// registered and addTree still returned nil — the library had no instant
// updates and nothing said so. Where the link resolves to a directory
// (filepath.EvalSymlinks), the tree is watched THERE, and every event under
// it is named back under the configured root (configuredName), so the scan
// it asks for is one ScanSubtree finds under its configured root. That is
// what makes a link to a link work on macOS: fsnotify's kqueue backend
// follows ONE level of a link it is asked to watch, so a root that is a
// chain had its own watch registered as a watch on a FILE, saw no Create
// event, and named each change after the root itself, which the watcher
// took for a change in the root's parent (see watchWalkStart). A Windows
// junction, which EvalSymlinks leaves as it is, is walked through its
// configured spelling (fsutil.WalkableRoot) and watched as the configured
// path, which ReadDirectoryChangesW follows. Only a configured root is
// followed: a directory that appears at runtime is walked as the scanner
// walks it, and the scanner walks no link below a root.
func walkWatchTree(root string, isConfiguredRoot bool, visit func(path string, d fs.DirEntry, watch bool) error) (string, error) {
	walkFrom, rootWatch, err := watchWalkStart(root, isConfiguredRoot)
	if err != nil {
		// The root cannot be seen through: the caller logs it as a
		// failed initial watch, which is the "root-level walk failure
		// surfaces" rule above.
		return "", err
	}
	return rootWatch, filepath.WalkDir(walkFrom, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == walkFrom {
				// Failure to even open the root — surface so the
				// caller can log a clear warning. Returning the
				// error stops the walk, which is what we want
				// (no point trying subdirs of an unreachable
				// root).
				return err
			}
			// Permission flaps on individual subdirs are non-fatal;
			// the periodic full scan would surface them anyway.
			return nil
		}
		if !d.IsDir() {
			return visit(path, d, false)
		}
		// `isConfiguredRoot && path == walkFrom` is the ONLY exemption.
		// Without it a configured root whose basename starts with a dot
		// (`/mnt/storage/.music`) registers ZERO watches and addTree
		// returns nil, so the caller's "initial watch add failed
		// (partial coverage)" warning never fires either: the library
		// silently loses instant-update coverage with no operator
		// signal at all. WalkDir hands the callback the string it was
		// handed verbatim, so string identity is exact against
		// walkFrom (never root: a linked root's walkFrom carries the
		// separator WalkableRoot appended). A runtime-discovered
		// directory gets no exemption — see the docblock.
		if (!isConfiguredRoot || path != walkFrom) && ShouldSkipDir(d.Name()) {
			// Not entered, but fsnotify's kqueue backend opens it as an
			// entry of the folder that holds it.
			if err := visit(path, d, false); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		watchPath := path
		if path == walkFrom {
			watchPath = rootWatch
		}
		return visit(watchPath, d, true)
	})
}

// watchWalkStart is where the watcher's walk starts (walkWatchTree), and
// the path the root's own watch is registered as. A directory that
// appeared at runtime, and a configured root that is a directory, are
// both as they are. A configured root that is a link to a directory is
// watched at the directory the link
// resolves to, when filepath.EvalSymlinks answers one: registered as the
// link itself, fsnotify's kqueue backend (macOS, the BSDs) follows one level
// of it, so a link to a link got a watch that sees the root as a file. What
// EvalSymlinks does not resolve to a plain directory (a Windows junction,
// which it leaves as it is since Go 1.23) is walked through its configured
// spelling (fsutil.WalkableRoot) and watched as the configured path, as
// before, which inotify and ReadDirectoryChangesW follow at every level.
func watchWalkStart(root string, isConfiguredRoot bool) (walkFrom, rootWatch string, err error) {
	if !isConfiguredRoot {
		return root, root, nil
	}
	walkFrom, err = fsutil.WalkableRoot(root)
	if err != nil {
		return "", "", err
	}
	if walkFrom == root {
		return root, root, nil
	}
	if resolved, evalErr := filepath.EvalSymlinks(root); evalErr == nil && resolved != root {
		if info, lstatErr := os.Lstat(resolved); lstatErr == nil && info.IsDir() {
			return resolved, resolved, nil
		}
	}
	return walkFrom, root, nil
}

// configuredName is an event's path in the spelling of the configured root it
// is under: a path under a linked root's resolved directory is renamed onto
// the root as configured (the longest resolved directory that holds it, should
// two nest), and every other path is returned as it is.
func (wt *Watcher) configuredName(name string) string {
	var match *rootAlias
	for i := range wt.aliases {
		a := &wt.aliases[i]
		if !pathAtOrUnder(name, a.resolved) {
			continue
		}
		if match == nil || len(a.resolved) > len(match.resolved) {
			match = a
		}
	}
	if match == nil {
		return name
	}
	return filepath.Join(match.configured, strings.TrimPrefix(name[len(match.resolved):], string(filepath.Separator)))
}

// pathAtOrUnder reports whether p is dir or a path below it, by string.
func pathAtOrUnder(p, dir string) bool {
	if p == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(p, dir)
}

// watchAddResult is how asking fsnotify to watch one directory went.
type watchAddResult int

const (
	// watchAdded: fsnotify watches the directory.
	watchAdded watchAddResult = iota
	// watchAddFailed: this directory could not be watched (logged), and the
	// walk goes on.
	watchAddFailed
	// watchLimitReached: a limit every later Add would meet too (logged), and
	// the walk stops.
	watchLimitReached
	// watchOutOfFiles: on kqueue, this process ran out of open files, which
	// its watches are; the caller releases every watch and says so
	// (stopWatching).
	watchOutOfFiles
)

// addWatch registers one directory's watch, and says how it went, with the
// error that answered a failure.
func (wt *Watcher) addWatch(path string) (watchAddResult, error) {
	addErr := wt.w.Add(path)
	switch {
	case addErr == nil:
		return watchAdded, nil
	case wt.fds != nil && isOpenFileLimitError(addErr):
		// kqueue: what ran out is the table every other open of this
		// process goes into, so keeping the watches it has would leave
		// the periodic scan nothing to open files with either.
		return watchOutOfFiles, addErr
	case isWatchLimitError(addErr):
		watcherLogger.Error("watch limit reached — periodic scan covers the gap; raise fs.inotify.max_user_watches to fix",
			"path", path, "err", addErr,
			"hint", "echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p")
		return watchLimitReached, addErr
	case isOpenFileLimitError(addErr):
		// fd-exhaustion (EMFILE) is a DIFFERENT limit from the
		// watch budget — pointing the operator at
		// max_user_watches here would send them down the wrong
		// path. Same degrade-to-periodic fallback, different hint.
		// A backstop: no Add outside kqueue opens a file per watch
		// (inotify_add_watch reports no EMFILE, and Windows words its
		// handle exhaustion otherwise), and kqueue takes the case above.
		watcherLogger.Error("open-file limit reached — periodic scan covers the gap; raise the open-files limit to fix",
			"path", path, "err", addErr,
			"hint", "raise the process open-files limit (ulimit -n, or LimitNOFILE= in the systemd unit) or the system-wide fs.file-max")
		return watchLimitReached, addErr
	default:
		watcherLogger.Warn("watch add", "path", path, "err", addErr)
		return watchAddFailed, addErr
	}
}

// handleEvent debounces and dispatches one fsnotify event. We
// trigger ScanSubtree against the parent directory of the
// affected file (the file itself isn't a watchable target on most
// platforms; the watch is on the dir).
//
// On directory Create we also add a watch over the new subtree so
// drops into freshly-mkdir'd folders inside already-watched
// libraries get seen. On kqueue the event is accounted for first
// (accountEvent), and the new folder waits for its watch
// (flushPendingAdds).
func (wt *Watcher) handleEvent(ctx context.Context, ev fsnotify.Event) {
	if wt.fds != nil {
		wt.accountEvent(ev)
	}
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return
	}
	if ev.Op&fsnotify.Create != 0 {
		if info, err := dirStat(ev.Name); err == nil && info.IsDir() {
			// New subtree under a watched root — start watching it.
			// Failures are non-fatal (logged inside addTree).
			// isConfiguredRoot=false: this directory just appeared, so
			// it gets no dot-name carve-out — a `.Trashes` that shows
			// up at runtime must be pruned exactly like one discovered
			// by the startup walk. Watched as fsnotify named it, which
			// under a linked root is the resolved spelling its parent's
			// watch uses (kqueue already holds an entry watch under
			// that name, and another spelling would be a second one).
			if wt.fds != nil {
				// kqueue: fsnotify watches the folder itself right after
				// sending this event, on its own goroutine, and an Add of
				// it now races that and loses a descriptor for good.
				wt.fds.wait(ev.Name)
			} else {
				_ = wt.addTree(ev.Name, false)
			}
		}
	}
	// The scan is asked for in the configured spelling, which is the one
	// ScanSubtree finds under a configured root.
	dir := filepath.Dir(wt.configuredName(ev.Name))
	wt.scheduleScan(ctx, dir)
}

// scheduleScan resets / arms a debounce timer for `dir`. Multiple
// events under the same dir within the debounce window collapse
// into a single ScanSubtree invocation. The timer fire path
// re-checks `ctx.Err()` so a watcher shutdown doesn't dispatch a
// stale scan after Run() returns.
//
// Reschedule vs. re-arm is decided by Stop(): if the existing timer is
// stopped before it fired, we reuse the same entry with a fresh window;
// if Stop() reports the timer already fired (its callback is in flight,
// blocked on wt.mu which we hold), we install a FRESH entry instead. The
// in-flight stale callback then finds `wt.pending[dir] != its own ps` and
// no-ops, so it can neither evict nor double-dispatch the new entry.
func (wt *Watcher) scheduleScan(ctx context.Context, dir string) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	if existing, ok := wt.pending[dir]; ok {
		if existing.timer.Stop() {
			existing.timer.Reset(wt.debounce)
			return
		}
		// Stop() == false: the timer already fired; its callback is
		// blocked on wt.mu. Fall through to a fresh entry — the stale
		// callback's identity check keeps it from touching this one.
	}
	ps := &pendingScan{}
	ps.timer = time.AfterFunc(wt.debounce, func() {
		wt.mu.Lock()
		if wt.pending[dir] == ps {
			delete(wt.pending, dir)
		}
		if wt.closing {
			// Shutting down: skip the scan and do NOT Add to scanWG —
			// waitForInflightScans (which set closing under the same mu) is
			// already waiting, and an Add here could race its Wait.
			wt.mu.Unlock()
			return
		}
		// Register this dispatch under mu, before releasing: it pairs with
		// waitForInflightScans's closing=true+Wait so a fired-during-shutdown
		// callback is either counted here (Add before closing → waited for) or
		// observes closing and no-ops — never lost, never leaked.
		wt.scanWG.Add(1)
		wt.mu.Unlock()
		defer wt.scanWG.Done()
		if ctx.Err() != nil {
			return
		}
		watcherLogger.Info("subtree scan", "dir", dir)
		if _, err := wt.scanner.ScanSubtree(ctx, dir); err != nil && ctx.Err() == nil {
			watcherLogger.Error("subtree scan", "dir", dir, "err", err)
		}
		// Deliberately here rather than in a defer: this must run BEFORE
		// the deferred scanWG.Done() above, and defers unwind LIFO.
		if hook := wt.afterDispatchHookForTests; hook != nil {
			hook()
		}
	})
	wt.pending[dir] = ps
}

// cancelAllPending stops every armed debounce timer. Called via a
// defer in Run() so every exit path — ctx cancel AND the fsnotify
// Events/Errors channels closing — stops armed timers before Run
// returns; otherwise a timer could fire ScanSubtree against a context
// the scanner is about to refuse (or after Run has already returned).
// Idempotent: a second call over an already-drained map is a no-op.
func (wt *Watcher) cancelAllPending() {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	for k, ps := range wt.pending {
		ps.timer.Stop()
		delete(wt.pending, k)
	}
}

// waitForInflightScans marks the watcher closing (so no debounce timer that
// fires from here on starts a scan) and blocks until every already-dispatched
// ScanSubtree has returned. Called from Run()'s defer AFTER cancelAllPending,
// so Run doesn't return while a scan is mid UpsertTrackBatch — the caller can
// then safely close the store once Run returns (B8). The closing flag and the
// per-dispatch scanWG.Add both live under wt.mu, so a fired-during-shutdown
// callback is deterministically either counted (waited for) or skipped.
func (wt *Watcher) waitForInflightScans() {
	wt.mu.Lock()
	wt.closing = true
	wt.mu.Unlock()
	wt.scanWG.Wait()
}

// dirStat is a tiny helper that returns FileInfo for a path
// without opening the file. Wraps os.Stat so it's mockable from
// tests if needed.
func dirStat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

// isWatchLimitError matches the inotify WATCH-budget exhaustion errors
// fsnotify surfaces — ENOSPC ("no space left on device", the
// fs.inotify.max_user_watches ceiling) and the documented "watch limit
// reached". Elsewhere no Add reports either: Windows has no such budget,
// and kqueue's budget is this process's open files, which is
// isOpenFileLimitError's and the watcher's own account (watcher_fds.go).
// We rely on the error string
// match rather than syscall.ENOSPC so the helper compiles cleanly on
// Windows / macOS where ENOSPC isn't relevant to the watcher budget.
// The match is conservative — only the canonical strings — so it
// doesn't fire on unrelated disk-full errors. fd-exhaustion (EMFILE,
// "too many open files") is deliberately NOT matched here — it's a
// different limit with a different remedy; see isOpenFileLimitError.
func isWatchLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{
		"no space left on device",
		"watch limit reached",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// isOpenFileLimitError matches fd-exhaustion (EMFILE / ENFILE, surfaced
// as "too many open files"). An Add meets it on kqueue (macOS), where
// every watched folder and every entry in one is an open file of this
// process, once the process's or the system's open-file limit is
// reached; there the watcher releases every watch (addWatch,
// stopWatching), since keeping them would leave the periodic scan, and
// everything else the bridge opens, no file to open. inotify_add_watch
// opens no file and reports no EMFILE. Distinct from isWatchLimitError:
// the remedy is raising the open-files limit, NOT
// fs.inotify.max_user_watches — pointing an fd-exhausted host at
// max_user_watches would send the operator down the wrong path.
func isOpenFileLimitError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "too many open files")
}
