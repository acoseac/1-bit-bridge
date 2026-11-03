package updater

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// dirBarrier is the marker's directory barrier in its three steps: the data
// dir opened, that descriptor fsynced, and closed (syncMarkerDir).
type dirBarrier struct {
	open  func(name string) (*os.File, error)
	sync  func(d *os.File) error
	close func(d *os.File) error
}

// markerBarrier is the barrier every write of the marker runs once the data
// dir's entry for it has changed, a rename or an unlink (syncMarkerDir). It
// is nil where the platform has no directory barrier (Windows, where
// fsutil.SyncsParentDir is false: fsutil.SyncParentDir's compile-time skip,
// since FlushFileBuffers refuses a directory handle). A test seam,
// process-global: production code MUST NOT mutate it; in-package tests
// override it with a t.Cleanup restore and never call t.Parallel(), and
// tests in other packages go through SetMarkerBarrierForTest.
var markerBarrier = platformMarkerBarrier()

// platformMarkerBarrier is the data dir's own open, fsync and close where the
// platform has a directory barrier, and nil where it has none.
func platformMarkerBarrier() *dirBarrier {
	if !fsutil.SyncsParentDir {
		return nil
	}
	return &dirBarrier{open: os.Open, sync: syncDirDescriptor, close: (*os.File).Close}
}

// syncDirDescriptor fsyncs the directory d: (*os.File).Sync, and on darwin,
// where Sync asks for F_FULLFSYNC, the ordinary fsync it falls back to
// (syncWithFallback, plainFsync). Not swapBinary's fsyncDir, which is
// best-effort and opens the directory itself.
func syncDirDescriptor(d *os.File) error {
	return syncWithFallback(d.Sync, plainFsync(d))
}

// syncWithFallback runs full, the directory's fsync as (*os.File).Sync makes
// it, and where that answers EINVAL or ENOTTY and plain is not nil, plain,
// the ordinary fsync, whose answer is then the barrier's. On darwin Sync asks
// for F_FULLFSYNC and itself falls back to fsync on ENOTSUP alone (Go's
// #64215); a full sync a filesystem does not support can answer EINVAL or
// ENOTTY too, and an ordinary fsync it may still do is asked before
// anything is taken for a filesystem that cannot fsync a directory. plain is
// nil off darwin, where Sync is the ordinary fsync already.
func syncWithFallback(full, plain func() error) error {
	err := full()
	if plain != nil && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY)) {
		return plain()
	}
	return err
}

// unsupportedDirSyncMessage is the line said once a process where the data
// dir's filesystem answers a directory's fsync that it cannot do one.
const unsupportedDirSyncMessage = "the data dir's filesystem cannot fsync a directory, so a change to the install marker " +
	"may not outlive a power loss there (said once a process)"

// unsupportedDirSyncSaid is whether unsupportedDirSyncMessage has been said
// in this process. A test resets it with a t.Cleanup restore.
var unsupportedDirSyncSaid atomic.Bool

// cannotSyncADirectory names the errno of a directory's fsync that says the
// filesystem cannot do one, "" for any other answer: EINVAL, ENOTSUP and
// EOPNOTSUPP (one errno on Linux, two on darwin and the BSDs). Linux's NFS
// and CIFS/SMB clients treat a directory's fsync as a success that does
// nothing, and its FUSE turns a filesystem's unimplemented FSYNCDIR into
// one, so those answer nil there and never come here; this is for the rest,
// a FUSE daemon or a network filesystem on another kernel that answers the
// fsync itself. Never ENOSYS, EBADF or anything else: those are failures.
func cannotSyncADirectory(err error) string {
	switch {
	case errors.Is(err, syscall.EINVAL):
		return "EINVAL"
	case errors.Is(err, syscall.ENOTSUP):
		return "ENOTSUP"
	case errors.Is(err, syscall.EOPNOTSUPP):
		return "EOPNOTSUPP"
	}
	return ""
}

// sayCannotSyncADirectory says once a process, naming no path and no value
// but the errno, that the data dir's filesystem cannot fsync a directory, so
// that the guarantee given up is not given up in silence.
func sayCannotSyncADirectory(errno string) {
	if unsupportedDirSyncSaid.Swap(true) {
		return
	}
	logger.Warn(unsupportedDirSyncMessage, "errno", errno)
}

// BarrierError is the marker's directory barrier failing: Step is the step
// that failed, "open", "sync" or "close", and Err its error, which errors.Is
// and errors.As reach. Its text names the step and Err's own words without
// the path an *fs.PathError names (fsutil.ErrorWithoutPath): the caller says
// which directory it was.
type BarrierError struct {
	Step string
	Err  error
}

func (e *BarrierError) Error() string {
	why := fsutil.ErrorWithoutPath(e.Err)
	switch e.Step {
	case "open":
		return fmt.Sprintf("the data dir could not be opened for its fsync (%s)", why)
	case "close":
		return fmt.Sprintf("the data dir's descriptor would not close after its fsync (%s)", why)
	}
	return fmt.Sprintf("the data dir would not fsync (%s)", why)
}

func (e *BarrierError) Unwrap() error { return e.Err }

// syncMarkerDir is the marker's directory barrier: dataDir opened, fsynced
// and closed, once a write of the marker has renamed a new marker over the
// old or removed it (SaveState, ClearStateDurably), and before a disarm
// trusts what such a write left (disarmHeld), so that the change outlives a
// power loss, which both rollbacks rest on. A fsync that answers that the
// filesystem cannot fsync a directory (cannotSyncADirectory) counts as done,
// said once a process: the marker's writes there are atomic and not
// durable, and no install is refused for it. Any other failure of the fsync,
// and any failure of the open or the close, is its *BarrierError. A no-op
// where the platform has no barrier (markerBarrier nil: Windows).
func syncMarkerDir(dataDir string) error {
	b := markerBarrier
	if b == nil {
		return nil
	}
	d, err := b.open(dataDir)
	if err != nil {
		return &BarrierError{Step: "open", Err: err}
	}
	syncErr := b.sync(d)
	closeErr := b.close(d)
	if syncErr != nil {
		errno := cannotSyncADirectory(syncErr)
		if errno == "" {
			return &BarrierError{Step: "sync", Err: syncErr}
		}
		sayCannotSyncADirectory(errno)
	}
	if closeErr != nil {
		return &BarrierError{Step: "close", Err: closeErr}
	}
	return nil
}

// SetMarkerBarrierForTest stands a test's barrier in for the marker's, for a
// test in another package: at each of its steps, "open", "sync" and "close",
// fail is asked, and its error is that step's failure; nil goes on with the
// step, the data dir's own open and close and no fsync, so that it answers
// alike on every platform, Windows included. A nil fail is a barrier that
// always answers. It returns what puts the barrier before it back.
// Process-global: a test that uses it MUST NOT call t.Parallel(), and
// restores it in a t.Cleanup.
func SetMarkerBarrierForTest(fail func(step string) error) (restore func()) {
	prev := markerBarrier
	ask := func(step string) error {
		if fail == nil {
			return nil
		}
		return fail(step)
	}
	markerBarrier = &dirBarrier{
		open: func(name string) (*os.File, error) {
			if err := ask("open"); err != nil {
				return nil, err
			}
			return os.Open(name)
		},
		sync: func(*os.File) error { return ask("sync") },
		close: func(d *os.File) error {
			err := d.Close()
			if failed := ask("close"); failed != nil {
				return failed
			}
			return err
		},
	}
	return func() { markerBarrier = prev }
}
