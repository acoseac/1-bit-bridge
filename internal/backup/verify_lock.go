package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// LockFileName is the lock every verification, bridge backup --verify, a
// claim and the pruner take, in every process, in the folder that holds the
// snapshots (backlog B653), named by where it resolves (resolveSnapshotIn,
// resolveDir), so that every path to one folder reaches one lock: a
// verification holds it from before it reads the manifest it checks until
// its verdict is written, so it is that record's one writer, and the pruner
// holds it across its orphan sweep, its listing and its deletions, so no
// snapshot is deleted from under a check. The lock is the kernel's on the
// open file (fsutil.TryLock), which a crash drops; the file itself stays, and
// only the lock on it means anything. A listing of the backups folder passes
// over it: it is no snapshot. Where no lock is granted, the protected work is
// refused (ErrBackupsUnlocked), never done without it.
const LockFileName = ".backups.lock"

// ErrBackupsBusy is a verification's answer when another verification or the
// pruner held the backups folder's lock for all of its wait.
var ErrBackupsBusy = errors.New("another verification or prune of these snapshots holds their lock")

// ErrBackupsUnlocked is the answer of a verification, a claim and a prune
// where the try's answer is one fsutil.KeepsNoLocks reads, a filesystem or a
// platform that gave no lock (ENOLCK, which Linux also answers on a
// filesystem that keeps locks and while another open holds one, among
// them): nothing is verified, recorded or removed then (backlog B653's
// second round), since nothing would keep another process's prune or check
// from it.
var ErrBackupsUnlocked = errors.New("no lock was granted on the backups folder, so nothing was verified or pruned")

// lockPoll is how often a wait for the lock tries it, and markerPoll how
// often a wait for another process's hold asks whether it is a check that
// did not end (stuckMarker).
const (
	lockPoll   = 50 * time.Millisecond
	markerPoll = time.Second
)

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

// openBackupsLock opens the folder's lock file: fsutil.OpenLockFile, and a
// test's stand-in for an open that hangs (SetOpenBackupsLockForTest).
var openBackupsLock = fsutil.OpenLockFile

// onLockWait, nil in production, runs once a wait for the lock has found it
// held by another open (SetOnLockWaitForTest).
var onLockWait func()

// onLockRelease, nil in production, runs as a holder lets the lock go,
// before its unlock, given the folder (SetOnLockReleaseForTest).
var onLockRelease func(dir string)

// folderTokens is this process's one holder of each backups folder's lock,
// by its resolved path: taken before the lock file is opened, and given
// back once it is closed, so that two holders in one process never open the
// file side by side. Where a lock is carried out as a POSIX record lock (a
// Linux NFS mount) a second open's lock is granted to the same process, and
// its close lets the first holder's lock go.
var folderTokens sync.Map

// folderToken is the token of the folder at key: a channel of one.
func folderToken(key string) chan struct{} {
	ch, _ := folderTokens.LoadOrStore(key, make(chan struct{}, 1))
	return ch.(chan struct{})
}

// lockBackups takes the lock in dir, the folder that holds the snapshots,
// waiting while another holder has it, in this process or another, for at
// most wait (0: one try), and answers the release. ErrBackupsBusy when the
// wait ends with the lock held, ErrBackupsUnlocked where no lock is granted,
// ctx's error when ctx ends first: a lock file opened, or a lock taken, once
// ctx has ended is closed at once, so a step its caller abandoned keeps no
// lock it took late.
func lockBackups(ctx context.Context, dir string, wait time.Duration) (release func(), err error) {
	return lockBackupsWith(ctx, dir, wait, folderToken(ownerKey(dir)))
}

// lockBackupsWith is lockBackups taking token, this process's holder of the
// folder, first; a nil token takes the kernel's lock alone, as another
// process does (VerifySnapshotAsAnotherProcessForTest).
func lockBackupsWith(ctx context.Context, dir string, wait time.Duration, token chan struct{}) (release func(), err error) {
	giveUp := time.Now().Add(wait)
	waited := false
	sawWait := func() {
		if !waited {
			waited = true
			if onLockWait != nil {
				onLockWait()
			}
		}
	}
	// pause waits a poll, or what is left of the wait; false once it is over.
	pause := func() (bool, error) {
		if !time.Now().Before(giveUp) {
			return false, nil
		}
		t := time.NewTimer(min(lockPoll, time.Until(giveUp)))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-t.C:
			return true, nil
		}
	}
	// The token first: another holder in this process is a lock held, and one
	// given up (stuck.go) is not waited for, asked of no filesystem.
	takeToken := func() error {
		if token == nil {
			return nil
		}
		for {
			select {
			case token <- struct{}{}:
				return nil
			default:
			}
			if check, held := stuckBy(dir); held {
				return stillRunning(check)
			}
			sawWait()
			ok, err := pause()
			if err != nil {
				return err
			}
			if !ok {
				return ErrBackupsBusy
			}
		}
	}
	if err := takeToken(); err != nil {
		return nil, err
	}
	giveToken := func() {
		if token != nil {
			<-token
		}
	}
	f, err := openBackupsLock(dir, LockFileName)
	if err != nil {
		giveToken()
		return nil, fmt.Errorf("open the snapshots' lock: %w", err)
	}
	closeAll := func() {
		_ = f.Close()
		giveToken()
	}
	if err := ctx.Err(); err != nil {
		closeAll()
		return nil, err
	}
	var askedMarker time.Time
	for {
		err := tryLockBackups(f)
		switch {
		case err == nil:
			if err := ctx.Err(); err != nil {
				_ = fsutil.Unlock(f)
				closeAll()
				return nil, err
			}
			// A marker left beside the lock its holder's first step removes,
			// bounded (clearStaleMarker). The release's unlock and close can
			// wait on the folder's filesystem: a holder runs it bounded
			// (hold.release).
			return func() {
				if onLockRelease != nil {
					onLockRelease(dir)
				}
				_ = fsutil.Unlock(f)
				closeAll()
			}, nil
		case fsutil.KeepsNoLocks(err):
			closeAll()
			return nil, fmt.Errorf("%w (%w)", ErrBackupsUnlocked, err)
		case !errors.Is(err, fsutil.ErrLocked):
			closeAll()
			return nil, fmt.Errorf("lock the snapshots: %w", err)
		}
		// Held by another process: one given up, then or since, is not
		// waited for; its marker is asked once a second.
		if time.Since(askedMarker) >= markerPoll {
			askedMarker = time.Now()
			if check, ok := stuckMarker(dir); ok {
				closeAll()
				return nil, stillRunning(check)
			}
		}
		sawWait()
		ok, perr := pause()
		if perr != nil {
			closeAll()
			return nil, perr
		}
		if !ok {
			closeAll()
			return nil, ErrBackupsBusy
		}
	}
}

// ProbeBackupsLock asks whether a lock is granted on the backups folder at
// all, for the doctor and for the ticker's pass: ErrBackupsUnlocked where the
// try's answer says none is (fsutil.KeepsNoLocks), and nil otherwise, a lock
// another holder has included, and where its lock file is not there yet or
// cannot be opened. It makes nothing, opens nothing this process holds (a
// holder's own lock, the token), and lets go at once of what it takes.
func ProbeBackupsLock(backupsRoot string) error {
	token := folderToken(ownerKey(backupsRoot))
	select {
	case token <- struct{}{}:
	default:
		return nil
	}
	defer func() { <-token }()
	f, err := openProbe(filepath.Join(backupsRoot, LockFileName))
	if err != nil {
		return nil
	}
	defer f.Close()
	switch err := tryLockBackups(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
	case fsutil.KeepsNoLocks(err):
		return fmt.Errorf("%w (%s)", ErrBackupsUnlocked, fsutil.ErrorWithoutPath(err))
	}
	return nil
}

// openProbe opens a lock file there already for a probe's try, making none:
// os.OpenFile for writing, as an exclusive lock on a network mount wants, and
// a test's stand-in for an open that hangs (SetOpenProbeForTest).
var openProbe = func(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }

// verifySlot is this process's one verification at a time, taken before the
// backups folder's lock: a verification that finds it taken waits its turn,
// in the order the waits began, for as long as its context lasts.
var verifySlot = make(chan struct{}, 1)

// slotHolder is the hold of the verification that has the slot, if any.
var slotHolder atomic.Pointer[hold]

// takeSlot takes the slot for the verification whose hold is h, waiting for
// it while another verification holds it until ctx ends, and answers its
// release; a slot held by a verification given up (stuck.go), from before the
// wait or from during it, answers ErrCheckStillRunning at once, naming its
// snapshot, and asks no filesystem.
func takeSlot(ctx context.Context, h *hold) (func(), error) {
	release := func() {
		slotHolder.CompareAndSwap(h, nil)
		<-verifySlot
	}
	select {
	case verifySlot <- struct{}{}:
		slotHolder.Store(h)
		return release, nil
	default:
	}
	if onSlotWait != nil {
		onSlotWait()
	}
	for {
		notice := stuckNotice()
		if holder := slotHolder.Load(); holder != nil {
			if check, marked := holder.marked(); marked {
				return nil, stillRunning(check)
			}
		}
		select {
		case verifySlot <- struct{}{}:
			slotHolder.Store(h)
			return release, nil
		case <-notice:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
