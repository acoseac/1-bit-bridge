package updater

import (
	"context"
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
	// VetoLifted says serve's admission lifted the marker's veto of this
	// version's store migrations (State.MigrationVeto).
	VetoLifted bool
}

// ErrMigrationVetoed is what a migrating store open's refusal matches
// (DisarmBeforeMigrating) where the marker vetoes this version's migrations
// (MigrationVetoed).
var ErrMigrationVetoed = errors.New("this version's store migrations are vetoed")

// MigrationVetoed reports whether st refuses current's store migrations: the
// early rollback's record of an install of current (State.RollbackStarted,
// Status "": RollbackRecord) that no start has gone on past (no
// State.DataDirOpenedBy): the backup may be back over current's binary
// already, or about to be, so current, running from the image it was started
// with, must not migrate the store past what that version knows (backlog
// B613's round 3). Serve's admission (AdmitBoot) ends it: its record of a
// start that got past its settings vetoes the rollback for good
// (EarlyRollback).
func MigrationVetoed(st State, current string) bool {
	return st.Status == "" && st.RollbackStarted && SameVersion(st.TargetVersion, current) && st.DataDirOpenedBy == ""
}

// AdmitBoot is serve's first disarm, the boot's admission, run before its
// first write to the data dir (disarmEarlyRollback in cmd/bridge), once the
// settings have loaded: it records durably in the install marker in dataDir
// that current, a version other than the backup's, is about to write the data
// dir while an install is armed (State.DataDirOpenedBy; ArmsDataDirRecord).
// From then on neither the early rollback (EarlyRollback) nor the boot
// rollback (BootInstallFailed) puts the backup back: what current writes, its
// store's migrations above all, may be what the backup's version does not
// know, and a restore would be a downgrade past it (backlog B613). save
// writes the marker; nil is SaveState, which syncs the file and its
// directory.
//
// The early rollback's own record of an install of current, left by a start
// stopped before the backup was back (MigrationVetoed), is armed, so the
// admission records over it too, and that record resolves it: no rollback
// can finish after it, and the store's migrations may run (round 3). The
// install's confirmation is left to the housekeeping after the store opens
// (BootRollbackOvertaken), which stamps it installed: stamped here, the same
// boot's housekeeping would remove the backup.
//
// It does nothing where no install is armed or a version is recorded
// already, but for the barrier: it fsyncs the data dir first (syncMarkerDir),
// so that a record or a removal an earlier start made, whose barrier then
// failed, is durable before this start trusts it, and refuses while that
// fails (round 3). A marker it cannot read cannot be told an armed install's
// or not, so it is removed, durably, and the read's error returned in
// Unreadable for the caller to say: the boot rollback cannot read it either,
// and a later start that could read it again must not take it for an install
// no start of this version went on past. An error says the record, the
// removal or the barrier did not land, durably, and is the caller's to refuse
// its write over.
//
// It holds the install marker's lock (LockState) from its read to its
// record's barrier: serve holds its data dir's lock already and takes this
// one inside it, and releases it before the store opens. Where the lock
// cannot be taken (another holder past the wait, no lock to give, a failure)
// it writes nothing: it refuses where it would record or remove, and goes on
// as before where it has nothing to write (no install armed, or one recorded
// already). On a filesystem that keeps no locks, where no process can hold
// one and the early rollback refuses, it goes on, saying so in NoLocks.
func AdmitBoot(ctx context.Context, dataDir, current string, save func(dataDir string, st State) error) (DisarmResult, error) {
	return disarm(ctx, dataDir, current, save, false)
}

// DisarmBeforeMigrating is AdmitBoot for a store's open with a migration step
// pending, whichever command opens it (internal/manifest; B613's round 2,
// Codex's finding 1): the record is made before the first step, and an
// error refuses the open before any step runs. It takes the install
// marker's lock inside the store's own migration lock (the ladder), which
// it holds already. Unlike serve's admission it refuses, with
// ErrMigrationVetoed, where the marker is the early rollback's record of an
// install of current that no start has gone on past (MigrationVetoed): a
// command's settings loading says nothing of serve's, and the binary on disk
// may be the backup's version already (round 3).
func DisarmBeforeMigrating(dataDir, current string) (DisarmResult, error) {
	return disarm(context.Background(), dataDir, current, nil, true)
}

// disarm takes the install marker's lock and runs the disarm under it
// (disarmHeld), or without it where it cannot be taken (disarmUnlocked).
func disarm(ctx context.Context, dataDir, current string, save func(dataDir string, st State) error, migrating bool) (DisarmResult, error) {
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		return disarmUnlocked(dataDir, current, migrating, err)
	}
	defer lock.Release()
	res, err := disarmHeld(dataDir, current, save, migrating)
	res.NoLocks = lock.Unheld()
	return res, err
}

// disarmUnlocked is a disarm that could not take the install marker's lock,
// lockErr why: it reads the marker, and goes on only where it has nothing to
// write, after the barrier; it refuses where it would record over an armed
// install, remove a marker it cannot read, or finds a rollback under way.
func disarmUnlocked(dataDir, current string, migrating bool, lockErr error) (DisarmResult, error) {
	st, err := LoadState(dataDir)
	switch {
	case err != nil:
		return DisarmResult{}, fmt.Errorf("the install marker cannot be read (%v), nor removed without its lock: %w", err, lockErr)
	case migrating && MigrationVetoed(st, current):
		return DisarmResult{}, migrationVetoedError(st)
	case ArmsDataDirRecord(st, current):
		return DisarmResult{}, fmt.Errorf("could not record in the install marker that %s opened the data dir, "+
			"since its lock cannot be taken: %w", current, lockErr)
	}
	if err := syncMarkerDir(dataDir); err != nil {
		return DisarmResult{}, fmt.Errorf("the install marker as it stands may not outlive a power loss: %w", err)
	}
	return DisarmResult{}, nil
}

// disarmHeld is the disarm itself, with the install marker's lock held:
// AdmitBoot's when migrating is false, DisarmBeforeMigrating's when it is
// true. It takes no lock.
func disarmHeld(dataDir, current string, save func(dataDir string, st State) error, migrating bool) (DisarmResult, error) {
	st, err := LoadState(dataDir)
	if err != nil {
		if rmErr := ClearStateDurably(dataDir); rmErr != nil {
			return DisarmResult{}, fmt.Errorf("the install marker can neither be read (%v) nor removed (%w)", err, rmErr)
		}
		return DisarmResult{Unreadable: err}, nil
	}
	if migrating && MigrationVetoed(st, current) {
		return DisarmResult{}, migrationVetoedError(st)
	}
	if !ArmsDataDirRecord(st, current) {
		// Nothing to record, which this start may take only once what an
		// earlier start wrote here is durable: a record whose barrier failed
		// after its rename, or a removal whose barrier failed after its
		// unlink, is live and may not outlive a power loss.
		if err := syncMarkerDir(dataDir); err != nil {
			return DisarmResult{}, fmt.Errorf("the install marker as it stands may not outlive a power loss: %w", err)
		}
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

// migrationVetoedError is a migration's refusal over the early rollback's
// record st: it names the versions and what ends it, never a path.
func migrationVetoedError(st State) error {
	return fmt.Errorf("%w: the early rollback of the install of %s to %s was started and no start has gone on past it, "+
		"so the store is not migrated: start bridge serve, which finishes the rollback where %s still refuses the settings "+
		"and confirms the install where it loads them", ErrMigrationVetoed, st.TargetVersion, st.PreviousVersion, st.TargetVersion)
}

// beforeDisarmRecord, nil in production, runs just before a disarm writes its
// record, with the data dir it writes in: a test's hold on a disarm at the
// point where it has read the marker and decided to record, the install
// marker's lock held.
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
