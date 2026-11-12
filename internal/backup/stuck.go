package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A hold given up (backlog B653's third to fifth rounds). Every holder of a
// backups folder's lock runs each step it takes under the lock bounded
// (hold.go): a verification (finish.go), the write of INCOMPLETE for a
// verification its caller stopped before it began (abandonPending), a claim
// (owner.go) and a prune (PruneContext); and what it lets go of at its end,
// the snapshot's ownership and the lock, is bounded too. A step that overruns
// its bound is given up: its holder answers its caller, runs no check and no
// prune, writes nothing but the INCOMPLETE of a verification it gave up, a
// write checking its give-up before each rename attempt (an attempt that
// passed the check may finish afterward), and keeps all it holds (the
// lock, and a verification's slot and the snapshot's ownership) until every
// step it gave up has ended, so nothing protected runs beside a step that may
// still have the bundle's files open, in this process or another. What a step
// given up answers late is dropped.
//
// A hold given up is marked, with no filesystem asked, under the folder's
// resolved key and every spelling it was reached by, and the mark stands
// until every hold is let go; it is raised only by an overrun, never by a hold
// that keeps within its bound, nor by a wait that overran nothing. Behind it,
// a verification, a claim, a prune or a restore of a snapshot there
// (holdSource, backlog B826) answers ErrCheckStillRunning at once, naming the
// snapshot, or the prune: in this process
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
// behind a hold given up that still holds the backups folder, and of a
// restore of a snapshot in that folder (holdSource): until its step ends, or
// until the process that runs it does. The hold given up is a verification's,
// a claim's, a prune's, since backlog B826's second round a restore's
// (StuckCheck.Restore), and since backlog B871 a probe's of the folder's lock
// whose release did not end, in this process (StuckCheck.Probe), which
// StillRunningError names.
var ErrCheckStillRunning = errors.New("a verification, a prune, a restore or a probe of these snapshots that did not end still holds them, until it ends or its process does")

// StuckCheck is a hold given up that still holds a backups folder: its
// snapshot's folder name, "" for a prune's (Pruning), the step it did not
// end, in words (Step), when it was given up, and whether it is a restore's
// (Restore, Restoring), whose snapshot is the one it restores. A marker
// written before restores were marked holds no restore, and reads as a
// verification's or a prune's, as it always did; an older bridge reading a
// restore's marker passes over the field and reads it as its snapshot's
// verification, the step's words saying what it was doing.
type StuckCheck struct {
	Snapshot string    `json:"snapshot"`
	Step     string    `json:"step,omitempty"`
	Since    time.Time `json:"since"`
	Restore  bool      `json:"restore,omitempty"`
	// Probe, set, is a probe of the folder's lock whose release did not end
	// within its bound (letProbeGo), named as its prober names it (Prober):
	// the doctor's or the scheduled snapshot's, never a restore's, whose
	// probe is that restore's own (Restore). Such a hold marks its own
	// process alone, so it is never written to a marker (backlog B871).
	Probe string `json:"-"`
}

// Pruning reports whether the hold given up is a prune's (PruneContext),
// which names no snapshot.
func (c StuckCheck) Pruning() bool { return c.Snapshot == "" && !c.Restore && c.Probe == "" }

// Restoring reports whether the hold given up is a restore's (holdSource):
// a step of a bridge restore left in a process that is still running, which
// the end of that process or of the storage's stall ends, never a restart of
// a serving bridge.
func (c StuckCheck) Restoring() bool { return c.Restore }

// Probing reports whether the hold given up is a probe of the folder's lock
// that did not let it go within its bound, the doctor's or the scheduled
// snapshot's (Prober).
func (c StuckCheck) Probing() bool { return c.Probe != "" }

// What names the hold given up: a restore of its snapshot, a probe of the
// snapshots' lock, a prune, or a verification of its snapshot.
func (c StuckCheck) What() string {
	switch {
	case c.Restoring():
		return "a restore of snapshot " + c.Snapshot
	case c.Probing():
		return c.Probe
	case c.Pruning():
		return "a prune"
	}
	return "snapshot " + c.Snapshot
}

// Prober is who probes a backups folder's lock: StuckCheckOf asks whether
// another open holds it, ProbeBackupsLock whether a lock is granted there at
// all. A probe whose release of the lock file it tried does not end within
// releaseBound marks the folder in this process alone, in its prober's name
// (letProbeGo): the doctor's (ProbeByDoctor), the scheduled snapshot's
// (ProbeByTicker), or bridge restore's look at its snapshot before its
// prompt (CheckRestoreSource), which names that restore. No other prober's
// mark names a restore (backlog B871).
type Prober struct {
	// what is the probe as its mark names it (StuckCheck.Probe), "" for a
	// restore's.
	what string
	// snapshot and restore: a restore's look at its snapshot, its mark the
	// restore's, by the snapshot's folder name.
	snapshot string
	restore  bool
	// spellings are more spellings of the folder, which its mark stands
	// under too (spellingKey's).
	spellings []string
}

// The probers of the doctor's backups lines and of the scheduled snapshot's
// pass.
var (
	ProbeByDoctor = Prober{what: "the doctor's probe of the snapshots' lock"}
	ProbeByTicker = Prober{what: "the scheduled snapshot's probe of the snapshots' lock"}
)

// mark is the hold given up a probe whose release overran its bound is
// marked as, at step: its prober's, a restore's for a restore's look.
func (p Prober) mark(step string) StuckCheck {
	now := time.Now().UTC()
	if p.restore {
		return StuckCheck{Snapshot: p.snapshot, Step: step, Since: now, Restore: true}
	}
	what := p.what
	if what == "" {
		what = "a probe of the snapshots' lock"
	}
	return StuckCheck{Probe: what, Step: step, Since: now}
}

// keys are the keys a probe of the folder at backupsRoot, whose resolved key
// is key, marks it under: the spelling it was given, its resolved key, and
// its prober's spellings.
func (p Prober) keys(backupsRoot, key string) []string {
	return append([]string{spellingKey(backupsRoot), key}, p.spellings...)
}

// StillRunningError is ErrCheckStillRunning for the hold given up that Check
// is, which errors.Is reads as ErrCheckStillRunning: its words name what it is
// (a verification's snapshot, a prune, a restore of its snapshot) and its step,
// and a caller that advises its operator asks Check what ends it
// (errors.As).
type StillRunningError struct {
	Check StuckCheck
}

func (e *StillRunningError) Error() string {
	step := ""
	if e.Check.Step != "" {
		step = " in " + e.Check.Step
	}
	return fmt.Sprintf("%s (%s, given up%s at %s)", ErrCheckStillRunning.Error(), e.Check.What(), step,
		e.Check.Since.Format(time.RFC3339))
}

// Is reads the error as ErrCheckStillRunning.
func (e *StillRunningError) Is(target error) bool { return target == ErrCheckStillRunning }

// stillRunning is ErrCheckStillRunning for the hold given up that check is,
// naming its snapshot, the prune it is, or the restore it is, and its step.
func stillRunning(check StuckCheck) error {
	return &StillRunningError{Check: check}
}

// HeldByRestore reports whether err is ErrCheckStillRunning for a restore's
// hold given up (StillRunningError): a step of a bridge restore left in a
// process that is still running, or waiting on storage that stopped
// answering, which ends with that process or with the stall, never with a
// restart of a serving bridge.
func HeldByRestore(err error) bool {
	var held *StillRunningError
	return errors.As(err, &held) && held.Check.Restoring()
}

// HeldByProbe reports whether err is ErrCheckStillRunning for a probe of the
// snapshots' lock whose release did not end, the doctor's or the scheduled
// snapshot's (StuckCheck.Probing), which this process made: it ends when the
// storage answers or this process does.
func HeldByProbe(err error) bool {
	var held *StillRunningError
	return errors.As(err, &held) && held.Check.Probing()
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
	// after, when not nil, closes once a removal of a marker left beside the
	// lock (hold.clearStale) has ended: the marker is written only then, so
	// that removal, begun before the hold was given up, never removes it.
	after <-chan struct{}
	// beforeCreate is a per-mark test seam after removal, before exclusive creation.
	beforeCreate func()
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
	return markStuckAfter(keys, root, check, nil)
}

// markStuckAfter is markStuck whose marker is written once after closes (nil:
// at once): the end of a removal of a marker its holder's process left, under
// way as the hold was given up (hold.clearStale), which would otherwise remove
// this one. The mark itself stands at once, its marker's write waiting on its
// own goroutine, so nothing that marks waits on the folder.
func markStuckAfter(keys []string, root string, check StuckCheck, after <-chan struct{}) *stuckMark {
	mark := &stuckMark{check: check, after: after}
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
// folder, unless its holder has removed it already: once a removal of a
// marker left beside the lock that was under way as the hold was given up has
// ended (after), outside mu, so its holder's removal never waits for it.
func (mark *stuckMark) writeMarker() {
	data, err := json.Marshal(mark.check)
	if err != nil {
		return
	}
	if mark.after != nil {
		if hook := onMarkerWrite.Load(); hook != nil {
			(*hook)("waits", mark.marker)
		}
		<-mark.after
	}
	mark.mu.Lock()
	defer mark.mu.Unlock()
	if mark.removed {
		return
	}
	if err := os.Remove(mark.marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	if mark.beforeCreate != nil {
		mark.beforeCreate()
	}
	f, err := os.OpenFile(mark.marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return
	}
	_ = fsutil.KeepOwner(f, mark.ref)
	_, _ = f.Write(data)
	_ = f.Close()
	if hook := onMarkerWrite.Load(); hook != nil {
		(*hook)("writes", mark.marker)
	}
}

// onMarkerWrite, nil in production, hears a marker's write: "waits" as it
// begins to wait for a removal under way (after), "writes" once it has
// written the marker, each with its path (SetOnMarkerWriteForTest).
var onMarkerWrite atomic.Pointer[func(event, marker string)]

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
// so a marker there is a process's that has ended. A restore's holder runs it
// through hold.clearStale, which a mark of its own waits for.
func clearStaleMarker(root string) {
	_ = removeStaleMarker(filepath.Join(root, stuckMarkerName))
}

// removeStaleMarker is clearStaleMarker's removal (os.Remove), and a test's
// stand-in for one that does not answer (SetRemoveStaleMarkerForTest).
var removeStaleMarker = os.Remove

// readStuckMarker is the read of a marker (fsutil.ReadAsFile), and a test's
// stand-in for a read that does not answer (SetReadStuckMarkerForTest).
var readStuckMarker = fsutil.ReadAsFile

// stuckMarker reads the marker in the backups folder at root, if any: a file,
// never a named pipe (fsutil.ReadAsFile), of a size a marker has.
func stuckMarker(root string) (StuckCheck, bool) {
	data, err := readStuckMarker(filepath.Join(root, stuckMarkerName))
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
// ended), or, where its probe's release of that lock does not end within its
// bound, that release, marked in by's name (letProbeGo, backlog B871). Past
// this process's own by the spelling given, it asks the folder, which may not
// answer: the doctor bounds it. It makes nothing, and lets go at once of the
// lock it tries.
func StuckCheckOf(backupsRoot string, by Prober) (StuckCheck, bool) {
	if check, held := stuckBy(backupsRoot); held {
		return check, true
	}
	key := ownerKey(backupsRoot)
	if check, held := stuckAt(key); held {
		return check, true
	}
	mark, ok := stuckMarker(backupsRoot)
	if !ok {
		return StuckCheck{}, false
	}
	if lockHeldElsewhere(backupsRoot, key, by) {
		return mark, true
	}
	// The probe's own release, past its bound, holds the folder now, marked
	// in by's name (letProbeGo): the hold given up that holds it.
	return stuckAt(key)
}

// lockHeldElsewhere reports whether another open holds the backups folder's
// lock now: a try on its lock file, made nowhere, opened for writing as an
// exclusive lock on a network mount wants, let go at once (letProbeGo). This
// process's own holder is no other (folderTokens). key is the folder's
// resolved key, by who asks.
func lockHeldElsewhere(backupsRoot, key string, by Prober) bool {
	token := folderToken(key)
	select {
	case token <- struct{}{}:
	default:
		return false
	}
	f, err := openProbe(filepath.Join(backupsRoot, LockFileName))
	if err != nil {
		<-token
		return false
	}
	err = tryLockBackups(f)
	letProbeGo(backupsRoot, f, err == nil, token, by.keys(backupsRoot, key), by)
	return errors.Is(err, fsutil.ErrLocked)
}

// letProbeGo lets go of what a probe of the lock of the backups folder at
// backupsRoot took, in this order: the lock, where its try took it (locked),
// the lock file, then this process's holder of the folder (token), so the
// holder is kept through the unlock and the close. It does so on a goroutine
// of its own, and waits for it within releaseBound. Past the bound the probe
// answers, the release left to end on its own, and the folder is marked in
// this process alone, under keys, in by's name, naming the release
// (stepProbeRelease), until the release has ended, the holder given back
// included: a verification, a claim, a prune or a restore here then meets the
// mark rather than waiting its lock wait out behind the holder, and a restore's
// look refuses before bridge restore's prompt in words that name it. No marker
// is written beside the lock for a release, whoever probed: once the unlock
// has answered another process may hold the lock, and a marker written then
// would stand beside that holder's lock as this probe's, or take its place
// (backlog B871).
func letProbeGo(backupsRoot string, f *os.File, locked bool, token chan struct{}, keys []string, by Prober) {
	var (
		mu    sync.Mutex
		ended bool
		mark  *stuckMark
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if locked {
			probeReleaseStep(backupsRoot, "unlock")
			_ = fsutil.Unlock(f)
		}
		probeReleaseStep(backupsRoot, "close")
		_ = f.Close()
		<-token
		mu.Lock()
		ended = true
		m := mark
		mu.Unlock()
		m.drop()
		probeReleaseStep(backupsRoot, "released")
	}()
	t := time.NewTimer(releaseBound)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	mu.Lock()
	defer mu.Unlock()
	if !ended {
		mark = markStuck(keys, "", by.mark(stepProbeRelease))
	}
}

// onProbeRelease, nil in production, runs as a probe of a backups folder's
// lock lets go of what it took, given the folder as the probe was given it:
// "unlock" before its unlock, where its try took the lock, "close" before
// its lock file's close, "released" once its release has ended and its mark,
// if one stood, is dropped (SetOnProbeReleaseForTest).
var onProbeRelease atomic.Pointer[func(dir, step string)]

// probeReleaseStep runs onProbeRelease's hook, if any.
func probeReleaseStep(dir, step string) {
	if hook := onProbeRelease.Load(); hook != nil {
		(*hook)(dir, step)
	}
}
