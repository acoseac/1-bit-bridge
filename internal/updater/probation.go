package updater

import (
	"context"
	"errors"
	"fmt"
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
	st.FailedStarts, st.RollbackRefused, st.NotReady = 0, "", false
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
// cleanly, in serve's stop, a restart request, or a listener's bind the host
// refused, after every teardown, holding the data dir's lock. ctx bounds the
// wait for the install marker's lock (serve gives it two seconds); on a
// filesystem that keeps none it goes on without it, as a disarm does. It
// reads the marker again and writes only where the marker is still the
// install of current that the start counted against (countsStart) and holds
// the count that start wrote: a stop after the confirmation, a restart into a
// newer install, and a count that never landed take nothing back, so no
// earlier start's count goes with this one. It reports whether it wrote. A
// take-back that does not land leaves one count, which errs toward a rollback
// of an install whose starts did end cleanly; the next start that ends
// cleanly does not take it back, and the confirmation clears it.
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
	// ProbationOK: on probation, no start of it but one still running has
	// failed to end cleanly.
	ProbationOK
	// ProbationWarn: a start of it besides one still running did not end
	// cleanly.
	ProbationWarn
	// ProbationFail: the count is at the limit, a crash rollback was refused,
	// or a start did not answer as ready.
	ProbationFail
)

// Probation is ProbationStatus's answer: where an install of the version
// that reads it stands, in words that name no path and no value.
type Probation struct {
	Level ProbationLevel
	// Says is for a reader beside the bridge (`bridge doctor`): the count as
	// the marker holds it, a start still running among it, since a start
	// counts itself until it ends cleanly.
	Says string
	// ThisStart is for the start of the version that is running (its stderr
	// line, the console's Updates panel): its own start taken off the count,
	// so the words are of the starts before it.
	ThisStart string
}

// ProbationStatus says where the install st describes stands for a start of
// version current (backlog B660): nothing where no install of current awaits
// its confirmation; OK on probation with at most one start counted (the one
// running, whose take-back comes at its end, so a healthy install reads 1);
// a warning from two; a failure at ProbationFailedStarts, where a crash
// rollback was refused (State.RollbackRefused), or where a start did not
// answer as ready (State.NotReady). It is the one source of the words serve
// prints, `bridge doctor` prints and the console's Updates panel shows.
func ProbationStatus(st State, current string) Probation {
	if !AwaitsConfirmation(st, current) {
		return Probation{}
	}
	n, back := st.FailedStarts, st.PreviousVersion
	if back == "" {
		back = "the backup's version"
	}
	earlier := max(n-1, 0)
	p := Probation{Level: ProbationOK}
	switch {
	case n >= ProbationFailedStarts || st.RollbackRefused != "" || st.NotReady:
		p.Level = ProbationFail
	case n >= 2:
		p.Level = ProbationWarn
	}
	confirmed := fmt.Sprintf("a start of it is confirmed once it has answered as ready and stayed up for %s", ProbationWindow)
	// A crash rollback refused promises no put-back: the refusal's clause,
	// added below, says why the backup stays where it is.
	refused := st.RollbackRefused != ""
	switch {
	case n <= 1:
		p.Says = fmt.Sprintf("%s is on probation since its install: %s", st.TargetVersion, confirmed)
	case refused:
		p.Says = fmt.Sprintf("%d of %s's starts since its install have not ended cleanly, one running now among them "+
			"(a start counts until it stops cleanly)", n, st.TargetVersion)
	case n < ProbationFailedStarts:
		p.Says = fmt.Sprintf("%s is on probation since its install, and %d of its starts have not ended cleanly, "+
			"one running now among them (a start counts until it stops cleanly): once %d have not, the next start puts %s back",
			st.TargetVersion, n, ProbationFailedStarts, back)
	default:
		p.Says = fmt.Sprintf("%d of %s's starts since its install have not ended cleanly, one running now among them "+
			"(a start counts until it stops cleanly), so the next start puts %s back unless one stops cleanly first",
			n, st.TargetVersion, back)
	}
	switch {
	case earlier == 0:
		p.ThisStart = fmt.Sprintf("%s is on probation since its install: this start is confirmed once it has answered as ready "+
			"and stayed up for %s", st.TargetVersion, ProbationWindow)
	case refused:
		p.ThisStart = fmt.Sprintf("%d of %s's starts before this one did not end cleanly", earlier, st.TargetVersion)
	case earlier < ProbationFailedStarts-1:
		p.ThisStart = fmt.Sprintf("%s is on probation since its install, and %d of its starts before this one did not end cleanly: "+
			"once %d have not, the next start puts %s back", st.TargetVersion, earlier, ProbationFailedStarts, back)
	case earlier == ProbationFailedStarts-1:
		p.ThisStart = fmt.Sprintf("%d of %s's starts before this one did not end cleanly: if this one does not either, "+
			"the next start puts %s back", earlier, st.TargetVersion, back)
	default:
		p.ThisStart = fmt.Sprintf("%d of %s's starts before this one did not end cleanly", earlier, st.TargetVersion)
	}
	if refused {
		why := fmt.Sprintf("; the backup is not put back: %s", st.RollbackRefused)
		p.Says += why
		p.ThisStart += why
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
// of the running version current with no migration of the store begun, and
// this process has been up for less than twice ProbationWindow. Until the
// install is confirmed, the backup beside the binary is the last version
// known to run here, which the crash rollback puts back; an install now would
// replace it with current, which has not shown it runs. Past that bound an
// install that never answered does not wait on itself, since a newer release
// may be its fix; and where a migration began the backup cannot be put back
// anyway, so nothing is held. A manual install is never held, and replaces
// the backup with the unconfirmed version.
func ProbationHolds(st State, current string, uptime time.Duration) string {
	if !AwaitsConfirmation(st, current) || st.StoreMigratedBy != "" || uptime >= 2*ProbationWindow {
		return ""
	}
	return fmt.Sprintf("%s awaits its confirmation (answered and up for %s), and the backup beside it, which a crash rollback "+
		"puts back, would be replaced by an install now: auto-install waits until it is confirmed, or for at most %s",
		current, ProbationWindow, 2*ProbationWindow)
}
