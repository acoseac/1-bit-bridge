package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A snapshot's owner (backlog B653's second round): the process that owes it
// its verification, from before its manifest is published until that
// verification ends. The owner holds the kernel's lock on ownerLockName in
// the snapshot's folder, which its capture takes right after it makes the
// folder (CaptureSnapshot) and a verification in place of a capture takes
// under the backups folder's lock (Claim), and lets go once the verification
// has ended (FinishCapture) or it gives the snapshot up (Release). A process
// that dies lets it go with it, so the lock tells a capture whose
// verification is still to come, queued or under way, from one whose process
// is gone, which no age can: the prune keeps a snapshot its owner holds
// however old its PENDING (keptByRecovery, ownedNow), and a reader reads a
// PENDING whose owner is gone as INCOMPLETE (interrupted) at once
// (readVerdict). The file itself stays, and only the lock on it means
// anything; a snapshot written before owners were kept has none, and its
// PENDING reads by its age as before.
//
// A process records what it owns in memory too (ownedHere), and asks that
// before any file: where a lock is carried out as a POSIX record lock (a
// Linux NFS mount), a second open of the file in the owning process takes
// the lock again, and closing it lets the owner's own go.
const ownerLockName = ".verification.lock"

// Ownership is what a snapshot's owner lock says (probeOwner).
type Ownership int

const (
	// OwnerNone: the snapshot holds no owner lock, as one written before
	// owners were kept does; its PENDING reads by its age.
	OwnerNone Ownership = iota
	// OwnerLive: a process holds it, and owes the snapshot its verification.
	OwnerLive
	// OwnerGone: the lock is there and nobody holds it, the owner's process
	// gone with the verification it owed.
	OwnerGone
	// OwnerUnknown: the lock could not be asked (a filesystem that gives
	// none, a read it refused): read as a live owner by the prune, and by
	// age by a reader.
	OwnerUnknown
)

// ownedHere is the snapshots this process owns, by their folder's resolved
// path.
var ownedHere sync.Map

// owner is a held owner lock.
type owner struct {
	key string
	f   *os.File
}

// release lets the lock go, and this process's record of it; a nil owner,
// or one let go already, does nothing.
func (o *owner) release() {
	if o == nil || o.key == "" {
		return
	}
	if o.f != nil {
		_ = fsutil.Unlock(o.f)
		_ = o.f.Close()
		o.f = nil
	}
	ownedHere.Delete(o.key)
	o.key = ""
}

// ownerKey is dir as ownedHere keys it.
func ownerKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	if real, err := resolveLinks(abs); err == nil {
		return real
	}
	return abs
}

// takeOwner takes the owner lock of the snapshot in dir, making its file
// when missing (fsutil.OpenLockFile, which gives it dir's owner as root),
// its record in this process first, so that no second open of the file is
// made here. A lock another holds is ErrClaimedElsewhere. A filesystem that
// gives no lock leaves the snapshot owned by this process alone, its file
// let go, which another process's probe reads as OwnerUnknown.
func takeOwner(dir string) (*owner, error) {
	key := ownerKey(dir)
	if _, taken := ownedHere.LoadOrStore(key, true); taken {
		return nil, ErrClaimedElsewhere
	}
	f, err := fsutil.OpenLockFile(dir, ownerLockName)
	if err != nil {
		ownedHere.Delete(key)
		return nil, fmt.Errorf("open the snapshot's owner lock: %w", err)
	}
	switch err := fsutil.TryLock(f); {
	case err == nil:
		return &owner{key: key, f: f}, nil
	case fsutil.KeepsNoLocks(err):
		_ = f.Close()
		return &owner{key: key}, nil
	case errors.Is(err, fsutil.ErrLocked):
		_ = f.Close()
		ownedHere.Delete(key)
		return nil, ErrClaimedElsewhere
	default:
		_ = f.Close()
		ownedHere.Delete(key)
		return nil, fmt.Errorf("lock the snapshot's owner lock: %w", err)
	}
}

// ErrClaimedElsewhere is a claim's answer for a snapshot whose owner lock
// another verification holds, in this process or another.
var ErrClaimedElsewhere = errors.New("another verification owes this snapshot its check")

// probeOwner is what the snapshot in dir's owner lock says, for a reader: a
// shared try (fsutil.TryLockShared), which an owner's exclusive hold refuses
// and another probe's does not, on a file opened for reading and never made.
func probeOwner(dir string) Ownership {
	if _, held := ownedHere.Load(ownerKey(dir)); held {
		return OwnerLive
	}
	f, err := os.Open(filepath.Join(dir, ownerLockName))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return OwnerNone
	case err != nil:
		return OwnerUnknown
	}
	defer f.Close()
	switch err := fsutil.TryLockShared(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
		return OwnerGone
	case errors.Is(err, fsutil.ErrLocked):
		return OwnerLive
	}
	return OwnerUnknown
}

// ownedNow reports whether the snapshot in dir may be owed its verification
// still, asked by the prune and the orphan sweep just before they remove it,
// holding the backups folder's lock, under which no verification claims a
// snapshot: an exclusive try on its owner lock, opened for writing as an
// exclusive lock on a network mount wants and never made, let go and closed
// again before the removal. Held, or not answered (err says why), it may be
// owed (true), and is kept; a snapshot with no owner lock, or one nobody
// holds, is not.
func ownedNow(dir string) (bool, error) {
	if _, held := ownedHere.Load(ownerKey(dir)); held {
		return true, nil
	}
	f, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_RDWR, 0)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return true, err
	}
	defer f.Close()
	switch err := fsutil.TryLock(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
		return false, nil
	case errors.Is(err, fsutil.ErrLocked):
		return true, nil
	default:
		return true, err
	}
}

// Capture is a snapshot kept on disk whose verification its holder owes:
// its owner lock is held until FinishCapture has verified it, or Release
// gives it up.
type Capture struct {
	// Dir is the snapshot's folder.
	Dir   string
	owner *owner
	once  sync.Once
}

// Release lets the snapshot's owner lock go, its verification still owed:
// a reader then reads its PENDING as INCOMPLETE (interrupted), and the prune
// keeps it no longer than any other snapshot whose check did not end. A nil
// Capture, or one released already, does nothing.
func (c *Capture) Release() {
	if c == nil {
		return
	}
	c.once.Do(func() { c.owner.release() })
}

// claimLockWait is how long a claim waits for the backups folder's lock, and
// claimTries how often it asks the owner lock, a reader's shared probe
// answering the first.
const (
	claimLockWait = 30 * time.Second
	claimTries    = 3
)

// Claim takes the ownership of the snapshot in dir, which a verification in
// place of a capture owes its check (the startup pass's, backlog B653's
// second round), under the backups folder's lock, so that no prune removes
// it between the claim and its check: ErrClaimedElsewhere where another
// verification holds it, the folder's lock's own answer (ErrBackupsBusy,
// ErrBackupsUnlocked, ErrCheckStillRunning) where that cannot be had. Every
// step that can wait on a filesystem runs on a goroutine of its own, which
// its caller waits for until it answers, or, once ctx has ended, for
// StopWriteBound more, as a verification's caller does (verifyAndThen); a
// claim that lands after its caller stopped waiting lets its ownership go at
// once. Under the lock its steps are bounded, StopWriteBound from the lock,
// as every holder's are (hold.go), and given up past it, ErrCheckGivenUp, the
// folder marked until they have ended; a folder a hold given up still holds
// is refused at once, asking no filesystem.
func Claim(ctx context.Context, dir, backupsRoot string) (*Capture, error) {
	if check, held := stuckBy(filepath.Dir(dir), backupsRoot); held {
		return nil, stillRunning(check)
	}
	type answer struct {
		c   *Capture
		err error
	}
	done := make(chan answer, 1)
	h := newHold(ctx, filepath.Base(spellingKey(dir)), filepath.Dir(dir), backupsRoot)
	go func() {
		defer h.drop()
		defer h.release()
		c, err := claim(ctx, dir, backupsRoot, h)
		done <- answer{c, err}
	}()
	select {
	case a := <-done:
		return a.c, a.err
	case <-ctx.Done():
	}
	t := time.NewTimer(StopWriteBound)
	defer t.Stop()
	select {
	case a := <-done:
		return a.c, a.err
	case <-t.C:
		h.abandon()
		go func() { (<-done).c.Release() }()
		return nil, fmt.Errorf("the claim was left to end on its own %s after its stop: %w", StopWriteBound, ctx.Err())
	}
}

// claim is Claim's work, on its own goroutine, its hold h, which its caller
// lets go of once it has answered.
func claim(ctx context.Context, dir, backupsRoot string, h *hold) (*Capture, error) {
	snap, root, err := resolveSnapshotIn(dir, backupsRoot)
	if err != nil {
		return nil, err
	}
	key := ownerKey(root)
	unlock, err := lockBackupsWith(ctx, root, claimLockWait, folderToken(key))
	if err != nil {
		return nil, err
	}
	h.held(unlock, root, key, filepath.Base(snap))
	by := h.lockedAt().Add(StopWriteBound)
	type took struct {
		o   *owner
		err error
	}
	for try := 1; ; try++ {
		if h.gaveUp() {
			return nil, h.givenUp(ctx, nil)
		}
		t, ok := stepOf(h, by, stepClaim, func(context.Context) took {
			if try == 1 {
				clearStaleMarker(root)
			}
			o, err := takeOwner(snap)
			return took{o: o, err: err}
		}, func(late took) { late.o.release() })
		if !ok {
			return nil, h.givenUp(ctx, nil)
		}
		switch {
		case t.err == nil && h.gaveUp():
			t.o.release()
			return nil, h.givenUp(ctx, nil)
		case t.err == nil:
			return &Capture{Dir: snap, owner: t.o}, nil
		case !errors.Is(t.err, ErrClaimedElsewhere) || try == claimTries:
			return nil, t.err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
