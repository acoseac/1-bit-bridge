package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A hold given up (backlog B653's third and fourth rounds). Every holder of a
// backups folder's lock runs each step it takes under the lock bounded
// (hold.go): a verification (finish.go), the write of INCOMPLETE for a
// verification its caller stopped before it began (abandonPending) and a
// claim (owner.go). A step that overruns its bound is given up: its holder
// answers its caller, runs no check and no prune, writes nothing but the
// INCOMPLETE of a verification it gave up, and keeps all it holds (the lock,
// and a verification's slot and the snapshot's ownership) until every step it
// gave up has ended, so nothing protected runs beside a step that may still
// have the bundle's files open, in this process or another. What a step given
// up answers late is dropped.
//
// A hold given up is marked, with no filesystem asked, under the folder's
// resolved key and every spelling it was reached by, and the mark stands
// until every hold is let go; it is raised only by an overrun, never by a hold
// that keeps within its bound. Behind it, a verification, a claim or a prune
// answers ErrCheckStillRunning at once, naming the snapshot: in this process
// by the mark, which each asks before any filesystem for a spelling the hold
// was reached by (stuckBy), and in another by the marker the hold writes
// beside the folder's lock (stuckMarkerName), best effort, which counts only
// while that lock is held. On a mount that answers nothing that marker cannot
// be written or read, and another process's wait ends ErrBackupsBusy. The
// doctor says so (StuckCheckHere, StuckCheckOf). A shutdown does not wait for
// it: the process's end ends the step and lets the kernel's locks go, a
// marker left behind removed by the next holder of the lock. What stays open
// here is backlog B653's.

// ErrCheckStillRunning is the answer of a verification, a claim and a prune
// behind a hold given up that still holds the backups folder: until its step
// ends, or until the process that runs it does.
var ErrCheckStillRunning = errors.New("a verification or prune of these snapshots that did not end still holds them, until it ends or its process does")

// StuckCheck is a hold given up that still holds a backups folder: its
// snapshot's folder name, "" for a prune's (Pruning), the step it did not
// end, in words (Step), and when it was given up.
type StuckCheck struct {
	Snapshot string    `json:"snapshot"`
	Step     string    `json:"step,omitempty"`
	Since    time.Time `json:"since"`
}

// Pruning reports whether the hold given up is a prune's (PruneContext),
// which names no snapshot.
func (c StuckCheck) Pruning() bool { return c.Snapshot == "" }

// stillRunning is ErrCheckStillRunning for the hold given up that check is,
// naming its snapshot, or the prune it is, and its step.
func stillRunning(check StuckCheck) error {
	step := ""
	if check.Step != "" {
		step = " in " + check.Step
	}
	what := "snapshot " + check.Snapshot
	if check.Pruning() {
		what = "a prune"
	}
	return fmt.Errorf("%w (%s, given up%s at %s)", ErrCheckStillRunning, what, step,
		check.Since.Format(time.RFC3339))
}

// stuckMark is a hold given up as this process keeps it: what it says, the
// keys it stands under, and its marker, whose write and removal take turns
// under mu, so that no write lands once its holder has removed it.
type stuckMark struct {
	check   StuckCheck
	keys    []string
	marker  string
	ref     string
	mu      sync.Mutex
	removed bool
}

// stuck is this process's holds given up, by every key they stand under: the
// folder's resolved key and the spellings it was reached by (spellingKey).
// A key keeps every mark that stands under it, since two holds of one folder
// can be given up at once (a verification's worker, which holds the slot, and
// the write of INCOMPLETE it starts, which holds the lock), and a mark's end
// takes its own alone; the lock is never held across a filesystem's call.
var (
	stuckMu sync.Mutex
	stuck   = map[string][]*stuckMark{}
)

// stuckMarkerName is the marker a hold given up leaves in its backups folder
// for another process to read.
const stuckMarkerName = ".check-still-running"

// notice is closed, and replaced, at each mark, so a wait for the slot that
// began before a hold was given up hears of it (takeSlot).
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

// markStuck marks a hold given up: check under every one of keys, asking no
// filesystem, the notice closed; and, where root names the folder as
// resolved, its marker written there on a goroutine of its own (the folder
// may not answer). It answers the mark, which its holder removes the marker
// of before it lets the lock go (removeMarker) and drops once every hold is
// let go (drop).
func markStuck(keys []string, root string, check StuckCheck) *stuckMark {
	mark := &stuckMark{check: check}
	for _, k := range keys {
		if !slices.Contains(mark.keys, k) {
			mark.keys = append(mark.keys, k)
		}
	}
	stuckMu.Lock()
	for _, k := range mark.keys {
		stuck[k] = append(stuck[k], mark)
	}
	stuckMu.Unlock()
	noticeMu.Lock()
	close(notice)
	notice = make(chan struct{})
	noticeMu.Unlock()
	if root != "" {
		mark.marker, mark.ref = filepath.Join(root, stuckMarkerName), root
		go mark.writeMarker()
	}
	return mark
}

// writeMarker writes the mark into its marker, with the owner of its backups
// folder, unless its holder has removed it already.
func (mark *stuckMark) writeMarker() {
	data, err := json.Marshal(mark.check)
	if err != nil {
		return
	}
	mark.mu.Lock()
	defer mark.mu.Unlock()
	if mark.removed {
		return
	}
	f, err := os.OpenFile(mark.marker, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_ = fsutil.KeepOwner(f, mark.ref)
	_, _ = f.Write(data)
	_ = f.Close()
}

// removeMarker removes the mark's marker, once its write has ended, and keeps
// any write after it from making one; a nil mark, or one with none, does
// nothing.
func (mark *stuckMark) removeMarker() {
	if mark == nil || mark.marker == "" {
		return
	}
	mark.mu.Lock()
	defer mark.mu.Unlock()
	mark.removed = true
	_ = os.Remove(mark.marker)
}

// drop takes the mark out of this process's record, under each key it stands
// under, leaving every other mark there; a nil mark does nothing.
func (mark *stuckMark) drop() {
	if mark == nil {
		return
	}
	stuckMu.Lock()
	defer stuckMu.Unlock()
	for _, k := range mark.keys {
		marks := slices.DeleteFunc(stuck[k], func(m *stuckMark) bool { return m == mark })
		if len(marks) == 0 {
			delete(stuck, k)
		} else {
			stuck[k] = marks
		}
	}
}

// stuckAt is a mark that stands under key, if any.
func stuckAt(key string) (StuckCheck, bool) {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	if marks := stuck[key]; len(marks) > 0 {
		return marks[0].check, true
	}
	return StuckCheck{}, false
}

// workDir is the working directory as the process started, which a relative
// spelling is keyed against (spellingKey); "" where it could not be had.
var workDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}()

// spellingKey is p as the record of holds given up keys a spelling: made
// absolute against the working directory the process started in (workDir),
// never asked again (os.Getwd stats the directory), and clean, no link
// resolved, so no filesystem is asked of anything (backlog B653's fifth
// round).
func spellingKey(p string) string {
	if !filepath.IsAbs(p) && workDir != "" {
		p = filepath.Join(workDir, p)
	}
	return filepath.Clean(p)
}

// stuckBy is the hold given up that holds the folder one of paths names, by
// the spelling it is given in, asking no filesystem.
func stuckBy(paths ...string) (StuckCheck, bool) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if check, held := stuckAt(spellingKey(p)); held {
			return check, true
		}
	}
	return StuckCheck{}, false
}

// stuckAnywhere is one of this process's holds given up, if any.
func stuckAnywhere() (StuckCheck, bool) {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	for _, marks := range stuck {
		if len(marks) > 0 {
			return marks[0].check, true
		}
	}
	return StuckCheck{}, false
}

// clearStaleMarker removes a marker left in the backups folder at root, which
// its holder's first step runs: held here, the lock holds no hold given up,
// so a marker there is a process's that has ended.
func clearStaleMarker(root string) {
	_ = os.Remove(filepath.Join(root, stuckMarkerName))
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

// StuckCheckHere is this process's hold given up that still holds the
// backups folder at backupsRoot, by the spelling it is given in: it asks no
// filesystem, so the doctor asks it first, whatever the folder answers.
func StuckCheckHere(backupsRoot string) (StuckCheck, bool) {
	return stuckBy(backupsRoot)
}

// StuckCheckOf is the hold given up that still holds the backups folder at
// backupsRoot, if one does: this process's own (StuckCheckHere, then by the
// folder as resolved), or another's whose marker is there while the folder's
// lock is held (a marker beside a lock nobody holds is a process's that has
// ended). Past this process's own by the spelling given, it asks the folder,
// which may not answer: the doctor bounds it. It makes nothing, and lets go
// at once of the lock it tries.
func StuckCheckOf(backupsRoot string) (StuckCheck, bool) {
	if check, held := stuckBy(backupsRoot); held {
		return check, true
	}
	if check, held := stuckAt(ownerKey(backupsRoot)); held {
		return check, true
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
