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

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var watcherLogger = logging.Component("watcher")

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
//
// Concurrency: Run() owns one goroutine for the fsnotify event
// loop and spawns one fire-and-forget goroutine per debounced
// dispatch. The debounce map is mutex-protected; scan invocations
// serialise via Scanner's own s.mu.
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
	return &Watcher{
		scanner:  scanner,
		debounce: debounce,
		w:        w,
		pending:  make(map[string]*pendingScan),
	}, nil
}

// Run starts the watch loop. Walks every configured root, adds a
// watch on every directory under each root (fsnotify is non-
// recursive on Linux/Windows; macOS coalesces fsevents at the
// kernel but the explicit per-dir watch keeps cross-platform
// behaviour uniform), then loops on events until ctx is cancelled.
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
	for _, root := range roots {
		if err := wt.addTree(root); err != nil {
			watcherLogger.Warn("initial watch add failed (partial coverage; periodic scan still runs)",
				"root", root, "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-wt.w.Events:
			if !ok {
				return nil
			}
			wt.handleEvent(ctx, ev)
		case err, ok := <-wt.w.Errors:
			if !ok {
				return nil
			}
			watcherLogger.Warn("fsnotify error", "err", err)
		}
	}
}

// addTree adds a recursive watch over root. Stops walking on the
// first ENOSPC ("watch limit reached") to avoid spamming the log
// once per directory — the operator gets one clear signal that
// the kernel limit needs raising.
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
func (wt *Watcher) addTree(root string) error {
	limitHit := false
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
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
			return nil
		}
		if shouldSkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if limitHit {
			// Already reported once per root — keep walking but
			// don't add more watches we know would fail.
			return nil
		}
		if addErr := wt.w.Add(path); addErr != nil {
			switch {
			case isWatchLimitError(addErr):
				watcherLogger.Error("watch limit reached — periodic scan covers the gap; raise fs.inotify.max_user_watches to fix",
					"path", path, "err", addErr,
					"hint", "echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p")
				limitHit = true
				return nil
			case isOpenFileLimitError(addErr):
				// fd-exhaustion (EMFILE) is a DIFFERENT limit from the
				// watch budget — pointing the operator at
				// max_user_watches here would send them down the wrong
				// path. Same degrade-to-periodic fallback, different hint.
				watcherLogger.Error("open-file limit reached — periodic scan covers the gap; raise the open-files limit to fix",
					"path", path, "err", addErr,
					"hint", "raise the process open-files limit (ulimit -n, or LimitNOFILE= in the systemd unit) or the system-wide fs.file-max")
				limitHit = true
				return nil
			default:
				watcherLogger.Warn("watch add", "path", path, "err", addErr)
			}
		}
		return nil
	})
}

// handleEvent debounces and dispatches one fsnotify event. We
// trigger ScanSubtree against the parent directory of the
// affected file (the file itself isn't a watchable target on most
// platforms; the watch is on the dir).
//
// On directory Create we also add a watch over the new subtree so
// drops into freshly-mkdir'd folders inside already-watched
// libraries get seen.
func (wt *Watcher) handleEvent(ctx context.Context, ev fsnotify.Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return
	}
	if ev.Op&fsnotify.Create != 0 {
		if info, err := dirStat(ev.Name); err == nil && info.IsDir() {
			// New subtree under a watched root — start watching it.
			// Failures are non-fatal (logged inside addTree).
			_ = wt.addTree(ev.Name)
		}
	}
	dir := filepath.Dir(ev.Name)
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
// reached". On non-Linux platforms the watch budget is effectively
// unlimited so false is the right default. We rely on the error string
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
// as "too many open files") which inotify raises when the process- or
// system-wide open-file limit is hit while arming a watch. Distinct
// from isWatchLimitError: the remedy is raising the open-files limit
// (ulimit -n / systemd LimitNOFILE / fs.file-max), NOT
// fs.inotify.max_user_watches. Same degrade-to-periodic fallback, but a
// different operator hint — pointing an fd-exhausted host at
// max_user_watches would send the operator down the wrong path.
func isOpenFileLimitError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "too many open files")
}
