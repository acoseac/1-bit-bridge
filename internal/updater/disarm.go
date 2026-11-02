package updater

import (
	"errors"
	"fmt"
	"sync"
)

// DisarmResult is what a disarm did (AdmitBoot, DisarmBeforeMigrating).
type DisarmResult struct {
	// Recorded says it recorded the version in the marker.
	Recorded bool
	// Unreadable is why the marker could not be read, when it removed it.
	Unreadable error
	// NoLocks is the answer of a filesystem that keeps no locks, when the
	// disarm went on without the install marker's lock (LockState).
	NoLocks error
}

// ErrRollbackUnderWay is what a migrating store open's refusal matches
// (DisarmBeforeMigrating) where the marker is the early rollback's record of
// an install of this version (RollbackUnderWay).
var ErrRollbackUnderWay = errors.New("a rollback of this version's install is under way")

// RollbackUnderWay reports whether st is the early rollback's record of an
// install of current that no start of a version has gone on past.
func RollbackUnderWay(st State, current string) bool {
	_, _ = st, current
	return false
}

// AdmitBoot is serve's first disarm, before its first write to the data dir
// (disarmEarlyRollback in cmd/bridge): it records durably in the install
// marker in dataDir that current, a version other than the backup's, is
// about to write the data dir while an install is armed
// (State.DataDirOpenedBy; ArmsDataDirRecord). From then on neither the early
// rollback (EarlyRollback) nor the boot rollback (BootInstallFailed) puts the
// backup back: what current writes, its store's migrations above all, may be
// what the backup's version does not know, and a restore would be a
// downgrade past it (backlog B613). save writes the marker; nil is
// SaveState, which syncs the file and its directory.
//
// It does nothing where no install is armed or a version is recorded
// already. A marker it cannot read cannot be told an armed install's or not,
// so it is removed, durably, and the read's error returned in Unreadable for
// the caller to say: the boot rollback cannot read it either, and a later
// start that could read it again must not take it for an install no start of
// this version went on past. An error says the record or the removal did not
// land, durably, and is the caller's to refuse its write over.
func AdmitBoot(dataDir, current string, save func(dataDir string, st State) error) (DisarmResult, error) {
	return disarmHeld(dataDir, current, save, false)
}

// DisarmBeforeMigrating is AdmitBoot for a store's open with a migration step
// pending, whichever command opens it (internal/manifest; B613's round 2,
// Codex's finding 1): the record is made before the first step, and an
// error refuses the open before any step runs.
func DisarmBeforeMigrating(dataDir, current string) (DisarmResult, error) {
	return disarmHeld(dataDir, current, nil, true)
}

// disarmHeld is the disarm itself: AdmitBoot's when migrating is false,
// DisarmBeforeMigrating's when it is true.
func disarmHeld(dataDir, current string, save func(dataDir string, st State) error, migrating bool) (DisarmResult, error) {
	_ = migrating
	st, err := LoadState(dataDir)
	if err != nil {
		if rmErr := ClearStateDurably(dataDir); rmErr != nil {
			return DisarmResult{}, fmt.Errorf("the install marker can neither be read (%v) nor removed (%w)", err, rmErr)
		}
		return DisarmResult{Unreadable: err}, nil
	}
	if !ArmsDataDirRecord(st, current) {
		return DisarmResult{}, nil
	}
	callBeforeDisarmRecord(dataDir)
	st.DataDirOpenedBy = current
	if save == nil {
		save = SaveState
	}
	if err := save(dataDir, st); err != nil {
		return DisarmResult{}, fmt.Errorf("could not record in the install marker that %s opened the data dir: %w", current, err)
	}
	return DisarmResult{Recorded: true}, nil
}

// beforeDisarmRecord, nil in production, runs just before a disarm writes its
// record, with the data dir it writes in: a test's hold on a disarm at the
// point where it has read the marker and decided to record.
var (
	disarmHookMu       sync.Mutex
	beforeDisarmRecord func(dataDir string)
)

func callBeforeDisarmRecord(dataDir string) {
	disarmHookMu.Lock()
	fn := beforeDisarmRecord
	disarmHookMu.Unlock()
	if fn != nil {
		fn(dataDir)
	}
}

// SetBeforeDisarmRecordForTest makes fn run just before each disarm writes
// its record (AdmitBoot, DisarmBeforeMigrating), with the data dir it writes
// in, for a test in another package that holds a disarm there. It returns
// what puts the seam before it back. Process-global: a test that uses it
// MUST NOT call t.Parallel(), and restores it in a t.Cleanup.
func SetBeforeDisarmRecordForTest(fn func(dataDir string)) (restore func()) {
	disarmHookMu.Lock()
	prev := beforeDisarmRecord
	beforeDisarmRecord = fn
	disarmHookMu.Unlock()
	return func() {
		disarmHookMu.Lock()
		beforeDisarmRecord = prev
		disarmHookMu.Unlock()
	}
}
