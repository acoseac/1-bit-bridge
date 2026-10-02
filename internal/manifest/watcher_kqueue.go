//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package manifest

import "golang.org/x/sys/unix"

// kqueueBackend says fsnotify watches through kqueue here (the build tag is
// fsnotify's own backend_kqueue.go's), which holds an open file per watched
// folder and per entry in one; see watcher_fds.go.
const kqueueBackend = true

// WatchFileLimits reads the open-file limits a kqueue watcher's budget is
// taken from: this process's soft RLIMIT_NOFILE, as Go left it after raising
// it at start, capped by kern.maxfilesperproc, and the system's table,
// kern.maxfiles. A sysctl that cannot be read (a BSD without the name) caps
// nothing; a limit that cannot be read at all is an error.
func WatchFileLimits() (WatchFDLimits, error) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil {
		return WatchFDLimits{}, err
	}
	// Cur is a uint64 on macOS and an int64 on FreeBSD.
	l := WatchFDLimits{Process: clampFileLimit(uint64(max(rl.Cur, 0)))}
	if n, err := unix.SysctlUint32("kern.maxfilesperproc"); err == nil && n > 0 && int(n) < l.Process {
		l.Process = int(n)
	}
	if n, err := unix.SysctlUint32("kern.maxfiles"); err == nil && n > 0 {
		l.System = clampFileLimit(uint64(n))
	}
	return l, nil
}
