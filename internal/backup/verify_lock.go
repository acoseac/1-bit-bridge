package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// LockFileName is the lock every verification, bridge backup --verify and
// the pruner take, in every process, in the folder that holds the snapshots
// (backlog B653): a verification holds it from before it reads the
// manifest it checks until its verdict is written, so it is that record's
// one writer, and the pruner holds it across its listing and its deletions,
// so no snapshot is deleted from under a check. The lock is the kernel's on
// the open file (fsutil.TryLock), which a crash drops; the file itself
// stays, and only the lock on it means anything. A listing of the backups
// folder passes over it: it is no snapshot.
const LockFileName = ".backups.lock"

// ErrBackupsBusy is a verification's answer when another verification or the
// pruner held the backups folder's lock for all of its wait.
var ErrBackupsBusy = errors.New("another verification or prune of these snapshots holds their lock")

// lockPoll is how often a wait for the lock tries it.
const lockPoll = 50 * time.Millisecond

// AutoLockWait is how long a verification the bridge makes (the scheduled
// one, the console's, the one bridge backup makes of its snapshot) waits for
// another process's verification or prune: as long as that one may run
// (VerifyDeadline), so a fresh snapshot's check waits its turn rather than
// leave it PENDING. VerifyCmdLockWait is bridge backup --verify's, someone
// at a terminal.
const (
	AutoLockWait      = VerifyDeadline
	VerifyCmdLockWait = 15 * time.Second
)

// tryLockBackups is the try a wait makes: fsutil.TryLock, and a test's
// stand-in for a try no lock is granted to.
var tryLockBackups = fsutil.TryLock

// noLocksOnce says once per process that no lock was granted on the backups
// folder's lock file.
var noLocksOnce sync.Once

// lockBackups takes the lock in dir, the folder that holds the snapshots,
// waiting while another open holds it, for at most wait (0: one try), and
// answers the release. ErrBackupsBusy when the wait ends with the lock held,
// ctx's error when ctx ends first. Where the try's answer is one
// fsutil.KeepsNoLocks reads (a filesystem or a platform that gave no lock)
// it goes on without one, said once, in words that claim nothing of the
// filesystem, since ENOLCK does not show that it keeps none: the pruner and
// a verification then run as they did before the lock, beside each other.
func lockBackups(ctx context.Context, dir string, wait time.Duration) (release func(), err error) {
	f, err := fsutil.OpenLockFile(dir, LockFileName)
	if err != nil {
		return nil, fmt.Errorf("open the snapshots' lock: %w", err)
	}
	giveUp := time.Now().Add(wait)
	for {
		err := tryLockBackups(f)
		switch {
		case err == nil:
			return func() {
				_ = fsutil.Unlock(f)
				_ = f.Close()
			}, nil
		case fsutil.KeepsNoLocks(err):
			noLocksOnce.Do(func() {
				logging.Component("backup").Warn("no lock was granted on the backups folder: another process may still hold one, and verifications and the prune run without exclusive use of the snapshots",
					"err", fsutil.ErrorWithoutPath(err))
			})
			return func() { _ = f.Close() }, nil
		case !errors.Is(err, fsutil.ErrLocked):
			_ = f.Close()
			return nil, fmt.Errorf("lock the snapshots: %w", err)
		}
		if !time.Now().Before(giveUp) {
			_ = f.Close()
			return nil, ErrBackupsBusy
		}
		t := time.NewTimer(min(lockPoll, time.Until(giveUp)))
		select {
		case <-ctx.Done():
			t.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// verifySlot is this process's one verification at a time, taken before the
// backups folder's lock: a verification that finds it taken waits its turn,
// in the order the waits began, for as long as its context lasts.
var verifySlot = make(chan struct{}, 1)

// takeSlot waits for the slot until ctx ends, and answers its release.
func takeSlot(ctx context.Context) (func(), error) {
	select {
	case verifySlot <- struct{}{}:
		return func() { <-verifySlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
