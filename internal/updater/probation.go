package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// An install's probation (backlog B660). The version installed booting proves
// only that its binary started: its boot's housekeeping runs before anything
// listens, so the install stays on probation (Status installing, the backup
// kept) until a start of it has answered as ready and stayed up for
// ProbationWindow (ConfirmInstall). Each start counts itself at its admission
// (AdmitBoot, State.FailedStarts) and takes its count back when it ends
// cleanly (TakeBackFailedStart), so the count is the starts that crashed, were
// killed, panicked or returned an error; after ProbationFailedStarts of them
// the next start puts the backup back, before it writes anything, where
// CrashRollback and the backup's own two checks allow it (cmd/bridge's
// recoverCrashedInstall). A migration of the store that began refuses that
// rollback for good (State.StoreMigratedBy). ProbationStatus says where an
// install stands, in one set of words, for serve's stderr, `bridge doctor`
// and the console's Updates panel.

// ConfirmInstall stamps the install of current confirmed: serve calls it once
// one start of current has answered as ready and stayed up for
// ProbationWindow. Under the install marker's lock (lockMarkerForUpdate, on
// ctx, serve's: held, and ctx asked again after the take, since LockState
// takes a free lock on an ended context), it reads the marker again and, only
// where it still awaits current's confirmation (AwaitsConfirmation), writes
// Status installed, InstalledAt now, and clears FailedStarts, RollbackRefused
// and NotReady; the boot after it removes the backup (BootCleanupBak). Any
// other marker, an install of a newer version recorded while it waited
// included, stays as it is, and it reports whether it stamped. A marker that
// awaits no confirmation asks for no lock. A lock it cannot hold is its error
// (ErrMarkerLockUnavailable; KeepsNoLocks tells a filesystem that keeps none,
// where no install is confirmed), and so is ctx's stop, with nothing written.
func ConfirmInstall(ctx context.Context, dataDir, current string, now time.Time) (bool, error) {
	if st, err := LoadState(dataDir); err == nil && !AwaitsConfirmation(st, current) {
		return false, nil
	}
	lock, err := lockMarkerForUpdate(ctx, dataDir, "the install was not confirmed")
	if err != nil {
		return false, err
	}
	defer lock.Release()
	// The marker as it is under the lock: another process may have changed
	// it since the read above (an install of a newer version, a rollback).
	st, err := LoadState(dataDir)
	if err != nil {
		return false, fmt.Errorf("the install was not confirmed, since the install marker cannot be read: %w", markerIOError{err})
	}
	if !AwaitsConfirmation(st, current) {
		return false, nil
	}
	st.Status, st.InstalledAt = "installed", now
	st.FailedStarts, st.RollbackRefused, st.NotReady, st.RunningStart = 0, "", false, ""
	if err := SaveState(dataDir, st); err != nil {
		return false, fmt.Errorf("the install was not confirmed: %w", markerIOError{err})
	}
	return true, nil
}

// RecordNotReady records in the install marker that a start of current did
// not answer as ready within ProbationWindow of beginning to serve
// (State.NotReady), under the install marker's lock as ConfirmInstall takes
// it, only where the marker awaits current's confirmation and does not say so
// already; it reports whether it wrote. Not ready rolls nothing back: the
// next start's admission and the confirmation clear it.
func RecordNotReady(ctx context.Context, dataDir, current string) (bool, error) {
	if st, err := LoadState(dataDir); err == nil && (!AwaitsConfirmation(st, current) || st.NotReady) {
		return false, nil
	}
	lock, err := lockMarkerForUpdate(ctx, dataDir, "the install marker does not record that this start has not answered as ready")
	if err != nil {
		return false, err
	}
	defer lock.Release()
	st, err := LoadState(dataDir)
	if err != nil {
		return false, fmt.Errorf("the install marker cannot be read: %w", markerIOError{err})
	}
	if !AwaitsConfirmation(st, current) || st.NotReady {
		return false, nil
	}
	st.NotReady = true
	if err := SaveState(dataDir, st); err != nil {
		return false, fmt.Errorf("the install marker does not record that this start has not answered as ready: %w", markerIOError{err})
	}
	return true, nil
}

// KeepsNoLocks reports whether err is an updater write refused because the
// data dir's filesystem keeps no locks (lockMarkerForUpdate), where it will
// never be held: a confirmation is never made there, as the boot's
// housekeeping makes none.
func KeepsNoLocks(err error) bool {
	var refused *markerLockError
	return errors.As(err, &refused) && refused.noLocks
}

// TakeBackFailedStart takes back the count a start of current added at its
// admission, counted being what the admission tried to write
// (DisarmResult.CountTried): serve calls it once that start has ended
// cleanly, as its stop arrives once it has answered as ready, and otherwise
// once its teardown has run after a stop, a restart request, or a listener's
// bind the host refused, holding the data dir's lock. ctx bounds the wait for
// the install marker's lock (serve's stop budget, from the stop's arrival); on
// a filesystem that keeps none it goes on without it, as a disarm does. It
// reads the marker again and writes only where the marker is still the
// install of current that the start counted against (countsStart) and holds
// the count that start wrote: a stop after the confirmation, a restart into a
// newer install, and a count that never landed take nothing back, so no
// earlier start's count goes with this one. The same write clears the start's
// token (State.RunningStart, which the count's write carried; backlog B660's
// third round), so no reader after it takes the start, still tearing down, for
// one running. It reports whether it wrote. A take-back that does not land
// leaves one count, which errs toward a rollback of an install whose starts
// did end cleanly; the next start that ends cleanly does not take it back, and
// the confirmation clears it.
func TakeBackFailedStart(ctx context.Context, dataDir, current string, counted int) (bool, error) {
	if counted <= 0 {
		return false, nil
	}
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		return false, err
	}
	defer lock.Release()
	st, err := LoadState(dataDir)
	if err != nil {
		return false, fmt.Errorf("the install marker cannot be read: %w", markerIOError{err})
	}
	if !countsStart(st, current) || st.FailedStarts != counted {
		return false, nil
	}
	st.FailedStarts--
	st.RunningStart = ""
	if err := SaveState(dataDir, st); err != nil {
		return false, markerIOError{err}
	}
	return true, nil
}

// CountFailedStartAgain counts again a start of current whose stop took its
// count back (TakeBackFailedStart) and which then did not end cleanly after
// all, counted being what its admission tried to write (backlog B760): serve
// calls it where a panic on its own goroutine ends a start whose take-back
// began as its stop arrived, the start having answered as ready, holding the
// data dir's lock. ctx bounds the wait for the install marker's lock, as the
// take-back's does; on a filesystem that keeps none it goes on without it. It
// reads the marker again and writes only where the marker is still an
// install of current that the start counted against (countsStart) and holds
// what the take-back left, counted less one, so it undoes that take-back and
// nothing else: a take-back that never landed, a confirmation and an install
// of another version since leave it nothing to write. It reports whether it
// wrote.
func CountFailedStartAgain(ctx context.Context, dataDir, current string, counted int) (bool, error) {
	if counted <= 0 {
		return false, nil
	}
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		return false, err
	}
	defer lock.Release()
	st, err := LoadState(dataDir)
	if err != nil {
		return false, fmt.Errorf("the install marker cannot be read: %w", markerIOError{err})
	}
	if !countsStart(st, current) || st.FailedStarts != counted-1 {
		return false, nil
	}
	st.FailedStarts = counted
	if err := SaveState(dataDir, st); err != nil {
		return false, markerIOError{err}
	}
	return true, nil
}

// CrashRollback reports whether a start of version current may put back the
// backup the install marker st describes because ProbationFailedStarts starts
// of current did not end cleanly (backlog B660), and why not where it may
// not. It is the first of the crash rollback's three checks, the one that
// starts no process, so that a store that was migrated does not run the
// backup at every start: cmd/bridge's recoverCrashedInstall then requires the
// backup to report PreviousVersion (ProbeVersion) and to load the settings
// (CheckConfig).
//
// It may where an install of current is armed (an install marker, or the
// crash rollback's own record of one, CrashRollbackRecord, whose rename did
// not land), its count is at the limit or past it, the install reached its
// swap, it names another version to go back to, no migration of the store
// began under it (State.StoreMigratedBy), and no version but current is
// recorded as having opened the data dir. No recency window: the count has no
// clock, and an install first started the next day (backlog B705) deserves
// the same. DataDirOpenedBy naming current refuses nothing here, where it
// refuses the other three rollbacks: current's own migrations record
// StoreMigratedBy before their first step, which does refuse it. why is ""
// where there is nothing to explain (no install of current, or fewer failed
// starts than the limit), and otherwise names no path and no value.
//
// What current wrote without a migration goes back under the backup with no
// check: the token store, the console's account and its tickets, the
// harvest state and the install marker are JSON the backup's lenient decoders
// read, dropping a field they do not know at their next write; the player's
// files that a strict decoder refuses (its DSD state, its queue) turn the
// player off, never serve; rows indexed above the backup's ExtractorVersion
// are served as indexed until their file changes; the settings are what
// CheckConfig asks. None of them stops the backup starting, so none is a
// refusal here (the record of B660 in ops/engineering-log.md has the list).
func CrashRollback(st State, current string) (ok bool, why string) {
	if !InstallArmed(st) || !SameVersion(st.TargetVersion, current) || st.FailedStarts < ProbationFailedStarts {
		return false, ""
	}
	switch {
	case !st.SwapStarted:
		return false, "the install never reached its swap, so the backup may not be the version it replaced"
	case st.PreviousVersion == "" || SameVersion(st.PreviousVersion, current):
		return false, "the install marker names no other version to go back to"
	case st.StoreMigratedBy != "":
		return false, fmt.Sprintf("%s began a migration of the store, which may have changed it whether or not the migration finished, "+
			"and %s does not read a store past its own migrations", st.StoreMigratedBy, st.PreviousVersion)
	case st.DataDirOpenedBy != "" && !SameVersion(st.DataDirOpenedBy, current):
		return false, fmt.Sprintf("%s has opened the data dir since the install, so its store may hold changes %s does not read",
			st.DataDirOpenedBy, st.PreviousVersion)
	}
	return true, ""
}

// CrashRollbackAtLimit is CrashRollback asked of a copy of st whose count is
// at least ProbationFailedStarts: whether the crash rollback's own checks of
// the marker would put the backup back once the count gets there, and why
// not (backlog B660's second round). The probation's words, its hold on the
// auto-installer and its hold on a command's migration all ask it, so none
// of them keeps or promises a backup the marker alone rules out. It reads no
// backup: the crash rollback's two other checks (the backup's version, its
// settings check) run a process, and only the start that acts runs them.
func CrashRollbackAtLimit(st State, current string) (ok bool, why string) {
	st.FailedStarts = max(st.FailedStarts, ProbationFailedStarts)
	return CrashRollback(st, current)
}

// CrashRollbackRecord is the marker the crash rollback writes, durably, just
// before it puts the backup back over the install of current st describes
// (cmd/bridge's recoverCrashedInstall; backlog B660): a copy of st with
// Status "", the rollback under way (RollbackStarted), current held back
// whatever the settings (RejectedVersion, a reason naming the starts that did
// not end cleanly, and no digest, since this is no fault of the settings) and
// its store migrations vetoed (MigrationVeto). Unlike RollbackRecord, which
// drops them by design, it keeps DataDirOpenedBy and FailedStarts: the count
// is what tells this record from the early rollback's and an operator's, and
// a start of current that finds it with the rename not landed finishes it
// where the rollback's checks hold again. RollbackRefused goes, since the
// rollback is no longer refused. The backup's version, whatever it is, reads
// it as a rollback done (BootRollbackFinished, in 0.2.3 already) and keeps
// the hold, so the auto-installer waits for a newer release, and a manual
// install still takes current.
func CrashRollbackRecord(st State, current string) State {
	st.Status, st.InstalledAt, st.RollbackStarted = "", time.Time{}, true
	st.RejectedVersion = current
	st.RejectedReason = fmt.Sprintf("%s was rolled back to %s, since %d of its starts since its install did not end cleanly",
		current, st.PreviousVersion, st.FailedStarts)
	st.RejectedConfigDigest = ""
	st.MigrationVeto = st.TargetVersion
	st.RollbackRefused = ""
	return st
}

// ProbationLevel is how an install on probation reads, for the doctor and
// the console.
type ProbationLevel int

const (
	// ProbationNone: no install of the version that reads it awaits its
	// confirmation, so there is nothing to say.
	ProbationNone ProbationLevel = iota
	// ProbationOK: on probation, and no start of it has failed to end
	// cleanly but one still running, which counts until it stops cleanly.
	ProbationOK
	// ProbationWarn: a start of it besides one still running did not end
	// cleanly.
	ProbationWarn
	// ProbationFail: the starts that did not end cleanly are at the limit, a
	// crash rollback was refused, or a start did not answer as ready.
	ProbationFail
)

// Probation is ProbationStatus's answer: where an install of the version
// that reads it stands, in words that name no path and no value.
type Probation struct {
	// Level grades Says.
	Level ProbationLevel
	// Says is for a reader beside the bridge (`bridge doctor`): the starts
	// that did not end cleanly, less the one a serve running now counts until
	// it stops cleanly.
	Says string
	// ThisStartLevel grades ThisStart, on the starts before the running one.
	ThisStartLevel ProbationLevel
	// ThisStart is for the start of the version that is running (its stderr
	// line, the console's Updates panel): its own start taken off the count,
	// so the words are of the starts before it.
	ThisStart string
}

// probationLevel grades an install whose starts that did not end cleanly,
// the running one left out, are failed.
func probationLevel(failed int, st State) ProbationLevel {
	switch {
	case failed >= ProbationFailedStarts || st.RollbackRefused != "" || st.NotReady:
		return ProbationFail
	case failed >= 1:
		return ProbationWarn
	}
	return ProbationOK
}

// ProbationStatus says where the install st describes stands for a start of
// version current (backlog B660): nothing where no install of current awaits
// its confirmation; OK on probation where no start but a running one is
// counted; a warning from one that did not end cleanly; a failure at
// ProbationFailedStarts, where a crash rollback was refused
// (State.RollbackRefused), or where a start did not answer as ready
// (State.NotReady). running says the count holds a start of current running
// now (RunningServeCounted: the start the marker's token names holds its own
// lock): its start is in the count until it stops cleanly, and Says takes it
// off; with none running every start counted is one that did not end cleanly
// (B660's second round: until then the doctor took one off whatever ran; the
// third: a held data dir lock, a command's too, is no running start). ThisStart is the running start's own, and
// takes it off always. Neither promises a put-back: the words follow the
// crash rollback's own checks of the marker at the limit
// (CrashRollbackAtLimit), saying why not where those refuse, and otherwise
// that the next start tries to put the backup back, if the backup can run
// these settings, since its version and its settings check are asked only by
// the start that acts. It is the one source of the words serve prints,
// `bridge doctor` prints and the console's Updates panel shows.
func ProbationStatus(st State, current string, running bool) Probation {
	if !AwaitsConfirmation(st, current) {
		return Probation{}
	}
	n, back := st.FailedStarts, st.PreviousVersion
	if back == "" {
		back = "the backup's version"
	}
	failed, earlier := n, max(n-1, 0)
	if running {
		failed = earlier
	}
	p := Probation{Level: probationLevel(failed, st), ThisStartLevel: probationLevel(earlier, st)}
	// Why the backup is not put back, where the marker says it is not: a
	// crash rollback refused, or the crash rollback's own checks at the limit.
	notPutBack := st.RollbackRefused
	if notPutBack == "" {
		if ok, why := CrashRollbackAtLimit(st, current); !ok {
			notPutBack = why
		}
	}
	tries := fmt.Sprintf("the next start tries to put %s back, if the backup can run these settings", back)
	once := fmt.Sprintf(": once %d have not, %s", ProbationFailedStarts, tries)
	switch {
	case failed == 0:
		p.Says = fmt.Sprintf("%s is on probation since its install: a start of it is confirmed once it has answered as ready "+
			"and stayed up for %s", st.TargetVersion, ProbationWindow)
	default:
		p.Says = fmt.Sprintf("%d of %s's starts since its install did not end cleanly", failed, st.TargetVersion)
		if running {
			p.Says += ", besides the one running now, which counts until it stops cleanly"
		}
		switch {
		case notPutBack != "":
			p.Says += "; the backup is not put back: " + notPutBack
		case n >= ProbationFailedStarts && running:
			p.Says += ": if the one running does not end cleanly either, " + tries
		case n >= ProbationFailedStarts:
			p.Says += ", so " + tries
		default:
			p.Says += once
		}
	}
	switch {
	case earlier == 0:
		p.ThisStart = fmt.Sprintf("%s is on probation since its install: this start is confirmed once it has answered as ready "+
			"and stayed up for %s", st.TargetVersion, ProbationWindow)
	default:
		p.ThisStart = fmt.Sprintf("%d of %s's starts before this one did not end cleanly", earlier, st.TargetVersion)
		switch {
		case notPutBack != "":
			p.ThisStart += "; the backup is not put back: " + notPutBack
		case n >= ProbationFailedStarts:
			p.ThisStart += ": if this one does not end cleanly either, " + tries
		default:
			p.ThisStart += once
		}
	}
	// A refusal recorded with nothing counted is still said, as it was.
	if st.RollbackRefused != "" {
		why := "; the backup is not put back: " + st.RollbackRefused
		if failed == 0 {
			p.Says += why
		}
		if earlier == 0 {
			p.ThisStart += why
		}
	}
	if st.NotReady {
		notReady := fmt.Sprintf("; a start of it did not answer as ready (its API listener, /v1/health and its store) within %s, "+
			"so it is not confirmed, and the bridge log says which answer was missing", ProbationWindow)
		p.Says += notReady
		p.ThisStart += notReady
	}
	return p
}

// ProbationHolds says why the automatic install of another version waits,
// "" where it does not (backlog B660): the marker st is an unconfirmed install
// of the running version current, this process has been up for less than
// twice ProbationWindow, and the crash rollback would put back the backup
// beside binaryPath, the binary serve runs as: no rollback of it was refused
// (State.RollbackRefused), the backup is there (a regular file at
// binaryPath+".bak"), and the crash rollback's own checks of the marker would
// pass at the limit (CrashRollbackAtLimit: a migration of the store begun, an
// install that never reached its swap, a data dir another version opened since
// all refuse it). B660's second round added the first two, and the third
// beside the migration it asked alone: a hold kept for a backup no rollback
// would put back held a newer release, perhaps the fix, for nothing. Until
// the install is confirmed, that backup is the last version known to run
// here; an install now would replace it with current, which has not shown it
// runs. Past the bound an install that never answered does not wait on
// itself, since a newer release may be its fix. A manual install is never
// held, and replaces the backup with the unconfirmed version.
func ProbationHolds(st State, current, binaryPath string, uptime time.Duration) string {
	if !AwaitsConfirmation(st, current) || uptime >= 2*ProbationWindow || st.RollbackRefused != "" {
		return ""
	}
	if ok, _ := CrashRollbackAtLimit(st, current); !ok || !backupBeside(binaryPath) {
		return ""
	}
	return fmt.Sprintf("%s awaits its confirmation (answered and up for %s), and the backup beside it, which a crash rollback "+
		"would put back, would be replaced by an install now: auto-install waits until it is confirmed, or for at most %s",
		current, ProbationWindow, 2*ProbationWindow)
}

// backupBeside reports whether a backup a crash rollback could put back
// stands beside binaryPath: a regular file at binaryPath+".bak", as the crash
// rollback requires it. A path that is not absolute names no backup.
func backupBeside(binaryPath string) bool {
	if !filepath.IsAbs(binaryPath) {
		return false
	}
	fi, err := os.Lstat(binaryPath + ".bak")
	return err == nil && fi.Mode().IsRegular()
}

// MigrationHeld says why a store open of current other than serve's own
// start does not migrate the store in dataDir, "" where it may (backlog
// B660's second round): the marker st is an install of current awaiting its
// confirmation, a start of it before now did not end cleanly, and the crash
// rollback's own checks would put the backup back at their limit
// (CrashRollbackAtLimit). The migration's record (State.StoreMigratedBy)
// refuses that rollback for good, so a command run to see what is wrong (a
// `bridge doctor`, a scan) would take the backup away from the starts it is
// for. The starts before now are State.FailedStarts less the start a serve
// running now counts until it stops cleanly (serveRunning;
// RunningServeCounted), so none before now is as it was before the round.
// Serve's own start migrates the store with its admission's permission
// (MigrationPermit), whatever the count.
func MigrationHeld(st State, current string, serveRunning bool) string {
	if !AwaitsConfirmation(st, current) {
		return ""
	}
	if ok, _ := CrashRollbackAtLimit(st, current); !ok {
		return ""
	}
	prior := st.FailedStarts
	if serveRunning {
		prior--
	}
	if prior <= 0 {
		return ""
	}
	return fmt.Sprintf("the install of %s awaits its confirmation, and %d of its starts did not end cleanly: a migration of the store "+
		"now would keep a crash rollback from ever putting %s back, so only a start of bridge serve migrates it until the install "+
		"is confirmed", st.TargetVersion, prior, st.PreviousVersion)
}
