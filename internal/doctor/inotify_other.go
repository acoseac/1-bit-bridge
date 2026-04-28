//go:build !linux

package doctor

// checkInotifyLimit is a no-op on non-Linux platforms — macOS
// (FSEvents) and Windows (ReadDirectoryChangesW) don't share the
// kernel-budget failure mode that Linux's inotify exhibits, so
// there's no equivalent surface to check. We still emit an OK
// row so the JSON / human report has a consistent shape across
// platforms and operators don't notice a missing line on a
// linux-only doc reference.
func checkInotifyLimit(_ Deps) Check {
	return ok("inotify-watch-limit", "not applicable on this platform")
}
