//go:build !(darwin || dragonfly || freebsd || netbsd || openbsd)

package manifest

// kqueueBackend says fsnotify does not watch through kqueue here: inotify and
// ReadDirectoryChangesW hold no open file per watched file, so the watcher
// keeps no account of them (watcher_fds.go).
const kqueueBackend = false

// WatchFileLimits answers ErrNoWatchFDBudget: no budget applies here.
func WatchFileLimits() (WatchFDLimits, error) {
	return WatchFDLimits{}, ErrNoWatchFDBudget
}
