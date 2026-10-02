//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package doctor

import "context"

// checkWatchLimit is a no-op where fsnotify watches through neither inotify
// nor kqueue: Windows' ReadDirectoryChangesW holds one handle per watched
// folder, against a per-process handle limit no library reaches, so there is
// no budget to check. Linux grades its inotify budget (inotify_linux.go) and
// macOS the open files its kqueue watches take (watchbudget_kqueue.go). We
// still emit an OK row so the JSON / human report has a consistent shape
// across platforms.
func checkWatchLimit(_ context.Context, _ Deps) Check {
	return ok("inotify-watch-limit", "not applicable on this platform")
}
