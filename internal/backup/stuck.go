package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A check that did not end (backlog B653's third round). A verification
// gives its check up once the check's context has ended (its caller's stop,
// or its deadline) and the check has not returned within half of
// StopWriteBound after: a read its context cannot reach, a hung mount. It
// then answers its caller and records INCOMPLETE, and its worker keeps all it
// holds, this process's verification slot, the backups folder's lock and the
// snapshot's ownership, until the check's goroutine has ended, so no other
// verification or prune runs beside a check that still has the bundle's files
// open, in this process or another. Its late result is dropped: an abandoned
// check records nothing but the INCOMPLETE it was given up with, and its
// verification runs no prune after it.
//
// While it runs it is marked (stuck), and a verification, a claim or a prune
// that would wait behind it answers ErrCheckStillRunning at once: in this
// process by the mark, in another by the marker it writes beside the folder's
// lock (stuckMarkerName), best effort, which counts only while that lock is
// held. The doctor says so (StuckCheckOf). A shutdown does not wait for it:
// the verification has answered its caller already, serve goes on to its end,
// and the process's end ends the check and lets the kernel's locks go, the
// marker left behind for the next holder of the lock to remove. What stays
// open here is backlog B653's.

// ErrCheckStillRunning is the answer of a verification, a claim and a prune
// that would wait behind a check that did not end and is still running: it
// holds the backups folder until it ends, or until the process that runs it
// does.
var ErrCheckStillRunning = errors.New("a check of these snapshots that did not end is still running, and holds them until it ends or its process does")

// StuckCheck is a check that did not end and still holds a backups folder:
// its snapshot's folder name, and when its verification gave it up.
type StuckCheck struct {
	Snapshot string    `json:"snapshot"`
	Since    time.Time `json:"since"`
}

// stuck is this process's checks that did not end, by their backups folder's
// resolved path (ownerKey). There is at most one: the slot is one.
var stuck sync.Map

// stuckMarkerName is the marker a check that did not end leaves in its
// backups folder for another process to read.
const stuckMarkerName = ".check-still-running"

// notice is closed, and replaced, at each mark, so a wait for the slot that
// began before a check was given up hears of it (takeSlot).
var (
	noticeMu sync.Mutex
	notice   = make(chan struct{})
)

// stuckNotice is the channel the next mark closes.
func stuckNotice() <-chan struct{} {
	noticeMu.Lock()
	defer noticeMu.Unlock()
	return notice
}

// markStuck marks the check of the snapshot named in the backups folder at
// root as one that did not end, and writes its marker, on a goroutine of its
// own (the folder may not answer), and answers the mark's clearing, which its
// worker runs once the check has ended and before it lets the lock go.
func markStuck(root, snapshot string) (clear func()) {
	key := ownerKey(root)
	mark := StuckCheck{Snapshot: snapshot, Since: time.Now().UTC()}
	stuck.Store(key, mark)
	noticeMu.Lock()
	close(notice)
	notice = make(chan struct{})
	noticeMu.Unlock()
	marker := filepath.Join(root, stuckMarkerName)
	go writeStuckMarker(key, marker, root, mark)
	return func() {
		stuck.Delete(key)
		_ = os.Remove(marker)
	}
}

// writeStuckMarker writes mark into the marker at path, with the owner of the
// backups folder at root, unless the mark has been cleared since.
func writeStuckMarker(key, path, root string, mark StuckCheck) {
	if _, still := stuck.Load(key); !still {
		return
	}
	data, err := json.Marshal(mark)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_ = fsutil.KeepOwner(f, root)
	_, _ = f.Write(data)
	_ = f.Close()
	// Cleared meanwhile: the marker is the cleared mark's.
	if _, still := stuck.Load(key); !still {
		_ = os.Remove(path)
	}
}

// stuckHere reports whether this process runs a check that did not end, and
// with root (non-empty) whether it holds that backups folder.
func stuckHere(root string) bool {
	if root != "" {
		_, held := stuck.Load(ownerKey(root))
		return held
	}
	found := false
	stuck.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}

// stuckMarker reads the marker in the backups folder at root, if any: a file,
// never a named pipe (fsutil.ReadAsFile), of a size a marker has.
func stuckMarker(root string) (StuckCheck, bool) {
	data, err := fsutil.ReadAsFile(filepath.Join(root, stuckMarkerName))
	if err != nil || len(data) > 4096 {
		return StuckCheck{}, false
	}
	var mark StuckCheck
	if err := json.Unmarshal(data, &mark); err != nil {
		return StuckCheck{}, false
	}
	return mark, true
}

// StuckCheckOf is the check that did not end and still holds the backups
// folder at backupsRoot, if one does: this process's own, or another's whose
// marker is there while the folder's lock is held (a marker beside a lock
// nobody holds is a process's that has ended). For the doctor; it makes
// nothing, and lets go at once of the lock it tries.
func StuckCheckOf(backupsRoot string) (StuckCheck, bool) {
	if v, held := stuck.Load(ownerKey(backupsRoot)); held {
		return v.(StuckCheck), true
	}
	mark, ok := stuckMarker(backupsRoot)
	if !ok {
		return StuckCheck{}, false
	}
	return mark, lockHeldElsewhere(backupsRoot)
}

// lockHeldElsewhere reports whether another open holds the backups folder's
// lock now: a try on its lock file, made nowhere, opened for writing as an
// exclusive lock on a network mount wants, let go at once. This process's
// own holder is no other (folderTokens).
func lockHeldElsewhere(backupsRoot string) bool {
	token := folderToken(ownerKey(backupsRoot))
	select {
	case token <- struct{}{}:
	default:
		return false
	}
	defer func() { <-token }()
	f, err := openProbe(filepath.Join(backupsRoot, LockFileName))
	if err != nil {
		return false
	}
	defer f.Close()
	switch err := tryLockBackups(f); {
	case err == nil:
		_ = fsutil.Unlock(f)
		return false
	case errors.Is(err, fsutil.ErrLocked):
		return true
	}
	return false
}
