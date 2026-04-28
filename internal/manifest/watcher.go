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
type Watcher struct {
	scanner  *Scanner
	debounce time.Duration
	w        *fsnotify.Watcher

	mu      sync.Mutex
	pending map[string]*time.Timer
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
		pending:  make(map[string]*time.Timer),
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
			wt.cancelAllPending()
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
			if isWatchLimitError(addErr) {
				watcherLogger.Error("watch limit reached — periodic scan covers the gap; raise fs.inotify.max_user_watches to fix",
					"path", path, "err", addErr,
					"hint", "echo fs.inotify.max_user_watches=524288 | sudo tee -a /etc/sysctl.d/99-bridge.conf && sudo sysctl -p")
				limitHit = true
				return nil
			}
			watcherLogger.Warn("watch add", "path", path, "err", addErr)
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
func (wt *Watcher) scheduleScan(ctx context.Context, dir string) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	if existing, ok := wt.pending[dir]; ok {
		existing.Reset(wt.debounce)
		return
	}
	wt.pending[dir] = time.AfterFunc(wt.debounce, func() {
		wt.mu.Lock()
		delete(wt.pending, dir)
		wt.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		watcherLogger.Info("subtree scan", "dir", dir)
		if _, err := wt.scanner.ScanSubtree(ctx, dir); err != nil && ctx.Err() == nil {
			watcherLogger.Error("subtree scan", "dir", dir, "err", err)
		}
	})
}

// cancelAllPending stops every armed debounce timer. Called from
// Run()'s ctx-cancel branch so a shutting-down server doesn't
// dispatch a flurry of scans against a context the scanner is
// about to refuse.
func (wt *Watcher) cancelAllPending() {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	for k, t := range wt.pending {
		t.Stop()
		delete(wt.pending, k)
	}
}

// dirStat is a tiny helper that returns FileInfo for a path
// without opening the file. Wraps os.Stat so it's mockable from
// tests if needed.
func dirStat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

// isWatchLimitError matches the platform-specific "too many
// watches" error fsnotify surfaces. On Linux this is ENOSPC; on
// other platforms the watch budget is effectively unlimited so
// false is the right default. We rely on the error string match
// rather than syscall.ENOSPC so the helper compiles cleanly on
// Windows / macOS where ENOSPC isn't relevant to the watcher
// budget. The match is conservative — only the canonical strings
// fsnotify documents — so it doesn't fire on unrelated disk-full
// errors.
func isWatchLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{
		"no space left on device",
		"too many open files",
		"watch limit reached",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
