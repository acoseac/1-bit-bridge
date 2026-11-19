package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// recoverCrashedInstall is serve's answer to an install of this version whose
// starts have not ended cleanly (backlog B660): at
// updater.ProbationFailedStarts of them, this start puts the backup back
// before it writes anything, where every check allows it, and exits for the
// service manager to start the backup's version; otherwise it says why not,
// records it in the marker (State.RollbackRefused) where it holds the install
// marker's lock, and serve goes on starting, since this start may be the one
// that works. That start counts itself at its admission, so every later start
// that does not end cleanly asks again. acted says whether the start ends
// here, and code is then serve's exit: it put the backup back (75), or its
// put-back left no binary at its path (1, backlog B898), the rollback's record
// kept and the admission not reached; a start that refused its settings then
// refuses them as before (acted false).
//
// It runs right after serve took the data dir's lock, and only where it did
// (the backup is put back by the data dir's owner alone), before the boot's
// admission (disarmEarlyRollback), so nothing of this start has written yet,
// and a start that finds the crash rollback's own record with its rename not
// landed finishes it, the record being the install still. A start that
// refused its settings runs it too, from the early rollback's place, where the
// early rollback leaves the install and the count asks for this one, under the
// data dir's lock it takes there (recoverCrashedRefusedInstall; backlog B760).
// then is what the start does where this leaves the install, which each line
// that says so ends with (crashRollbackGoesOn, crashRollbackRefusedStart; a
// start that goes on ends instead where no binary is left,
// crashRollbackEndsTheStart). The three checks are asked in this order, the
// first starting no process, so that a store that was migrated does not run
// the backup at every start:
// updater.CrashRollback (the count, the swap, the versions, no migration of
// the store begun, no version but this one recorded over the data dir), the
// backup's version (updater.ProbeVersionWithin), and the backup's settings
// check (updater.CheckConfig), so that a rollback does not trade one crash
// loop for another over a setting this version saved that the backup refuses. A
// backup that predates that check (ConfigCheckUnsupported: 0.2.2 and older)
// is not put back, and the start records why (B660's second round; until
// then it was put back with a warning): it cannot say whether it loads the
// settings, one that does not stops at its load with this version held back
// from the auto-installer, and its strict decoder refuses any key it does not
// know, which a later version's settings may name. The operator puts it back
// by hand where it loads them.
//
// The backup's two runs come before the install marker's lock, as an install
// runs its candidate's settings check before it (updater.Install), and the
// rollback decides again under the lock (backlog B894, B898's F1): a run that
// does not answer lasts its bound (a settings check updater.ConfigCheckBound),
// and under the lock it held every other taker past the 30 s each waits, at
// every start of a crash loop. Before them the backup is a regular file whose
// identity is fixed through a handle on it, closed before anything runs it,
// and the settings' digest is read (updater.ConfigDigest); every observation
// of the backup reads its identity as it is taken (fsutil.EntryIdentity, the
// handle's stat), so no comparison reads one, and one that cannot be read is a
// backup that could not be inspected (backlog B898FU's round 4). Then it
// follows the early rollback (recoverRefusedInstall) step for step: the
// install marker's lock, held from the marker's read again through the rename
// (never without the exclusion), and the decision made again under it with
// nothing run again: the marker still asks for the rollback, the version the
// backup answered is its PreviousVersion, the install is the one the runs were
// asked for (its TargetVersion and AttemptedAt compared, its PreviousVersion
// held through the version the backup answered, which matched the marker
// checked before the lock and must match the one read again under it: the
// three fields refuseCrashRollbackBeforeTheLock compares), the backup is the
// file they ran (os.SameFile, its size, its modification time) and the
// settings have the digest they were read with; then the hold and the veto
// recorded durably before the rename, and the marker put back where the rename
// fails. A refusal decided before the lock is said, then recorded under the
// lock that refuseCrashRollbackBeforeTheLock takes for it; a change found
// under the lock refuses this start's rollback, said and recorded, and the
// next start of the loop asks again. A backup that could not be inspected,
// before the lock or under it (a lookup or an open that failed otherwise than
// finding nothing there, an identity not read), is said and recorded nowhere
// (backlog B898FU's round 3): it shows the backup neither gone nor refused,
// and a record would end the auto-installer's hold (updater.ProbationHolds),
// which asks the backup's lookup itself; the words such a failure was recorded
// in until then are cleared where the same install still asks
// (clearInspectionRefusal). A stop while a run runs, or found once the lock is
// held, records nothing. A lock that cannot be held, or a filesystem that
// keeps none, still leaves the install as it is, said as before, the runs
// asked already.
//
// A crash before the admission is not counted: a candidate that never runs
// is what the settings check before the swap sees, and a start refused for
// its settings has the early rollback. This rollback counts what got past
// both.
func recoverCrashedInstall(ctx context.Context, dataDir, configPath string, opts serveOpts, then string, stderr io.Writer) (code int, acted bool) {
	current := version.ServerVersion
	st, err := updater.LoadState(dataDir)
	if err != nil {
		// The admission removes a marker it cannot read, and says so.
		return 0, false
	}
	ok, why := updater.CrashRollback(st, current)
	if !ok && why == "" {
		return 0, false
	}
	// An unfinished restore of the database leaves the install as it is,
	// before any marker write or rename, said on the rollback's line
	// (backlog B653 PR 2a): the backup's binary may not know the restore's
	// journal, and would open the half-done swap. The start then meets the
	// journal again at its store's open, and exits 1.
	if waits := rollbackWaitsForTheRestore(dataDir); waits != "" {
		crashRollbackLine(stderr, st, waits, then)
		return 0, false
	}
	if !ok {
		refuseCrashRollbackOnARead(ctx, dataDir, current, why, then, opts, stderr)
		return 0, false
	}
	say := func(st updater.State, why string) { crashRollbackLine(stderr, st, why, then) }
	save := opts.saveUpdateState
	if save == nil {
		save = updater.SaveState
	}
	// The backup, asked before the install marker's lock (backlog B894): what
	// refuses it here is said, and recorded under the lock, which
	// refuseCrashRollbackBeforeTheLock takes for it, but a backup that could
	// not be inspected, given no words to record (backlog B898FU's round 3).
	// checked is the marker the runs are asked for.
	checked := st
	refuseBefore := func(said, recorded string) {
		refuseCrashRollbackBeforeTheLock(ctx, dataDir, current, checked, said, recorded, then, save, stderr)
	}
	binaryPath := servingBinaryPath(opts, stderr)
	if !filepath.IsAbs(binaryPath) {
		refuseBefore("this binary's path is not known, nor so where its backup is", "this binary's path is not known, nor so where its backup is")
		return 0, false
	}
	backup := binaryPath + ".bak"
	// Each observation of the backup's entry reads its identity as it is
	// taken (fsutil.EntryIdentity, or a test's serveOpts.backupIdentity), so
	// the comparisons below read nothing (backlog B898FU's round 4): on
	// Windows an Lstat's identity is read by os.SameFile as it compares, by
	// opening the path then, and a read that failed there compared as another
	// file and was recorded as a backup that changed.
	identityOf := opts.backupIdentity
	if identityOf == nil {
		identityOf = fsutil.EntryIdentity
	}
	fi, err := identityOf(backup)
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		// A lookup that fails otherwise than finding nothing there (a
		// permission, a link loop on the way, an identity that could not be
		// read) shows no backup absent (backlog B898FU's rounds 2 and 4): said
		// as a backup that could not be inspected, never as none, and recorded
		// nowhere (its round 3), so the auto-installer's hold asks the
		// backup's lookup itself.
		refuseBefore(fmt.Sprintf("the backup at %s could not be inspected (%s)", backup, fsutil.ErrorWithoutPath(err)), "")
		return 0, false
	case err != nil || !fi.Mode().IsRegular():
		refuseBefore(fmt.Sprintf("there is no backup to put back, a regular file at %s", backup), "there is no backup beside the binary to put back")
		return 0, false
	}
	// Which file the runs ask, fixed before either: through a handle on it,
	// whose stat carries its file's identity as it is taken, as the entry's
	// observations above and under the lock carry theirs. Opened as a file
	// (fsutil.OpenAsFile: a named pipe put there is refused without a wait),
	// required to be the entry the first observation found, and closed at
	// once, before the runs and the restore, which opens the backup for
	// deletion and which this open would refuse while it stood.
	f, identity, err := fsutil.OpenAsFile(backup)
	if err != nil {
		// An open that fails, the Lstat having found a regular file, is said
		// in its own words and recorded nowhere (backlog B898FU's round 3),
		// whatever failed it: the next start's lookup says what stands there.
		refuseBefore(fmt.Sprintf("the backup at %s could not be opened to tell which file its checks ask (%s)", backup, fsutil.ErrorWithoutPath(err)), "")
		return 0, false
	}
	_ = f.Close()
	if !identity.Mode().IsRegular() || !os.SameFile(fi, identity) {
		refuseBefore("the backup changed while it was checked", "the backup changed while it was checked")
		return 0, false
	}
	// The settings the backup's check answers for, read before it: a read
	// that fails refuses, here and under the lock, so that two reads that
	// both failed never compare as the same settings.
	digest, err := updater.ConfigDigest(configPath)
	if err != nil {
		refuseBefore(fmt.Sprintf("the settings could not be read before the backup's settings check (%s)", fsutil.ErrorWithoutPath(err)),
			"the settings could not be read before the backup's settings check")
		return 0, false
	}
	// A stop in the version's run answers its context's error, said as a
	// version not said and recorded nowhere: the record's lock is asked for on
	// the stopped context, and the helper writes nothing past a stop.
	got, err := backupVersion(ctx, backup, opts)
	if err != nil {
		refuseBefore(fmt.Sprintf("the backup at %s did not say its version: %v", backup, err), "the backup did not say its version")
		return 0, false
	}
	if !updater.SameVersion(got, checked.PreviousVersion) {
		refuseBefore(fmt.Sprintf("the backup at %s is %s, where the install replaced %s", backup, got, checked.PreviousVersion),
			fmt.Sprintf("the backup is %s, where the install replaced %s", got, checked.PreviousVersion))
		return 0, false
	}
	checkConfig := opts.checkConfig
	if checkConfig == nil {
		checkConfig = updater.CheckConfig
	}
	check := checkConfig(ctx, backup, configPath)
	switch check.Verdict {
	case updater.ConfigValid:
	case updater.ConfigCheckUnsupported:
		refuseBefore(fmt.Sprintf("the backup, %s, predates the settings check, so it cannot say whether it loads this bridge's settings, "+
			"and one that does not would not start once put back", checked.PreviousVersion),
			"the backup predates the settings check")
		return 0, false
	case updater.ConfigRefused:
		why := fmt.Sprintf("the backup, %s, refuses this bridge's settings, so it would not start on them: %s",
			checked.PreviousVersion, strings.Join(check.Reasons, "; "))
		refuseBefore(why, why)
		return 0, false
	default:
		if check.Err != nil {
			say(checked, "this start was stopped while the backup checked the settings")
			return 0, false
		}
		refuseBefore(fmt.Sprintf("the backup's settings check did not answer: %s", check.Detail),
			fmt.Sprintf("the backup, %s, could not check this bridge's settings", checked.PreviousVersion))
		return 0, false
	}
	stateLock, err := updater.LockState(ctx, dataDir)
	if err != nil {
		var refused *updater.StateLockError
		if !errors.As(err, &refused) {
			say(checked, "this start was stopped while it waited for the install marker's lock")
			return 0, false
		}
		say(checked, fmt.Sprintf("the install marker's lock cannot be taken (%v), and only its holder may put the backup back", err))
		return 0, false
	}
	defer stateLock.Release()
	if !stateLock.Held() {
		say(checked, "the data dir's filesystem keeps no locks, so nothing would keep a store's migration out while the backup is put back")
		return 0, false
	}
	// A stop as the runs answered, or while a lock that needed no wait was
	// taken (a context that has ended does not stop such a take), writes
	// nothing.
	if ctx.Err() != nil {
		say(checked, "this start was stopped before it put the backup back")
		return 0, false
	}
	// The marker as it is now that its lock is this process's.
	if st, err = updater.LoadState(dataDir); err != nil {
		say(updater.State{Status: "installing", TargetVersion: current, FailedStarts: updater.ProbationFailedStarts},
			"the install marker cannot be read again")
		return 0, false
	}
	refuse := func(said, recorded string) {
		say(st, said)
		recordCrashRollbackRefusal(dataDir, st, recorded, save, stderr)
	}
	if ok, why := updater.CrashRollback(st, current); !ok {
		if why != "" {
			refuse(why, why)
		}
		return 0, false
	}
	// Decided again on what the runs answered for, neither run again: the
	// version the backup said against the marker read now, the install the
	// one they were asked for, the backup the file they ran, the settings
	// the ones its check read.
	if !updater.SameVersion(got, st.PreviousVersion) {
		refuse(fmt.Sprintf("the backup at %s is %s, where the install replaced %s", backup, got, st.PreviousVersion),
			fmt.Sprintf("the backup is %s, where the install replaced %s", got, st.PreviousVersion))
		return 0, false
	}
	if !updater.SameVersion(st.TargetVersion, checked.TargetVersion) || !st.AttemptedAt.Equal(checked.AttemptedAt) {
		refuse("the install changed while its backup was checked", "the install changed while its backup was checked")
		return 0, false
	}
	switch now, err := identityOf(backup); {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		// A lookup that fails otherwise than finding nothing there, an
		// identity that could not be read among them (round 4), shows the
		// backup neither gone nor changed (backlog B898FU's round 3): said,
		// and recorded nowhere, as before the lock.
		say(st, fmt.Sprintf("the backup at %s could not be inspected again under the install marker's lock (%s)", backup,
			fsutil.ErrorWithoutPath(err)))
		clearInspectionRefusal(dataDir, st, save, stderr)
		return 0, false
	case err != nil || !now.Mode().IsRegular() || !os.SameFile(identity, now) ||
		now.Size() != identity.Size() || !now.ModTime().Equal(identity.ModTime()):
		refuse("the backup changed while it was checked", "the backup changed while it was checked")
		return 0, false
	}
	switch now, err := updater.ConfigDigest(configPath); {
	case err != nil:
		refuse(fmt.Sprintf("the settings could not be read again after the backup's settings check (%s)", fsutil.ErrorWithoutPath(err)),
			"the settings could not be read again after the backup's settings check")
		return 0, false
	case now != digest:
		refuse("the settings changed while the backup checked them", "the settings changed while the backup checked them")
		return 0, false
	}
	// The hold and the veto first, durably, then the rename, as the early
	// rollback does: a rename that landed before them would leave the backup's
	// version free to install this one again, and this version's commands to
	// migrate the store past the binary put back.
	record := updater.CrashRollbackRecord(st, current)
	if err := save(dataDir, record); err != nil {
		say(st, fmt.Sprintf("could not record %s as held back (%v), and a backup put back without that record would "+
			"install it again", current, err))
		return 0, false
	}
	restore := opts.restoreBackup
	if restore == nil {
		restore = updater.RollbackBinary
	}
	if err := restore(binaryPath, ".bak"); err != nil {
		// No binary at its path (backlog B898): the record stays, its hold,
		// its veto and its count with it, and the start ends here, before the
		// admission, which would count it, lift the veto of its version and
		// let its store migrate past what the backup reads. A start that
		// refused its settings refuses them as before.
		if missing := binaryMissing(err); missing != "" {
			code, ends, next := 0, false, then
			if then == crashRollbackGoesOn {
				code, ends, next = 1, true, crashRollbackEndsTheStart
			}
			crashRollbackLine(stderr, st, fmt.Sprintf("could not put the backup back, and the bridge's program file is not at its path: %v; "+
				"the rollback's record is kept: %s", err, missing), next)
			return code, ends
		}
		// The install marker back as it was, with why: the binary in place is
		// still this version's.
		st.RollbackRefused = "the backup could not be put back over the binary"
		if perr := save(dataDir, st); perr != nil {
			say(st, fmt.Sprintf("could not put the backup back (%v), nor the install marker as it was (%v)", err, perr))
			return 0, false
		}
		say(st, fmt.Sprintf("could not put the backup back: %v", err))
		return 0, false
	}
	fmt.Fprintf(stderr, "updater: %d starts of %s since its install did not end cleanly, so the install is rolled back: %s is back at %s "+
		"and %s is held back from the auto-installer; exiting for the service manager to start %s (where nothing starts it, "+
		"run bridge serve again)\n", st.FailedStarts, current, st.PreviousVersion, binaryPath, current, st.PreviousVersion)
	return supervision.RestartExitCode, true
}

// rollbackWaitsForTheRestore is why a rollback leaves an install where an
// unfinished restore of the database stands in dataDir (its journal,
// manifest.CheckRestoreJournal; backlog B653 PR 2a), or "" where none does.
// The crash rollback and the early rollback ask it before any marker write or
// rename: a rollback over a half-done restore would hand the data dir to a
// binary that may not know the journal.
func rollbackWaitsForTheRestore(dataDir string) string {
	if err := manifest.CheckRestoreJournal(manifest.DefaultDBPath(dataDir)); err != nil {
		return fmt.Sprintf("the rollback waits for the unfinished restore of the library database (%v)", err)
	}
	return ""
}

// What a start does where its crash rollback leaves the install, with which
// each line that says so ends: a start whose settings loaded goes on and
// counts itself, and one that refused them ends at that refusal, as it did
// before its crash rollback was asked (backlog B760).
const (
	crashRollbackGoesOn       = "this start goes on, and counts itself"
	crashRollbackRefusedStart = "this start refuses its settings, as it did before, and is not counted"
	// crashRollbackEndsTheStart is a start whose settings loaded, where the
	// rollback left no binary at its path (backlog B898).
	crashRollbackEndsTheStart = "this start ends here, before it writes anything, and is not counted"
)

// crashRollbackLine says why the crash rollback leaves the install st
// describes, for an install of this version whose count asked for it, and
// then, what the start does next; the line may name the backup's path, as the
// early rollback's do.
func crashRollbackLine(stderr io.Writer, st updater.State, why, then string) {
	fmt.Fprintf(stderr, "updater: not rolling back the install of %s after %d starts of it that did not end cleanly: %s; %s\n",
		st.TargetVersion, st.FailedStarts, why, then)
}

// recordCrashRollbackRefusal records why the crash rollback left the install
// st describes (State.RollbackRefused), in words that name no path, no value
// and no error's text, where the marker does not say it already. The install
// marker's lock is the caller's. A record that cannot be written is said,
// and the refusal stands.
func recordCrashRollbackRefusal(dataDir string, st updater.State, why string, save func(string, updater.State) error, stderr io.Writer) {
	if st.RollbackRefused == why {
		return
	}
	st.RollbackRefused = why
	if err := save(dataDir, st); err != nil {
		fmt.Fprintf(stderr, "updater: the install marker does not record why the backup was not put back: %v\n", err)
	}
}

// inspectionRefusals are the words the crash rollback recorded, until backlog
// B898FU's round 3, for a backup it could not inspect before the install
// marker's lock: a lookup that failed otherwise than finding nothing there
// (its round 2) and an open that failed (backlog B894). It records nothing
// for one since: such a record shows the backup neither gone nor refused, and
// ends the auto-installer's hold (updater.ProbationHolds), which then would
// let a newer release replace the backup, the last version known to run.
var inspectionRefusals = []string{
	"the backup could not be inspected",
	"the backup could not be opened to tell which file its checks ask",
}

// clearInspectionRefusal clears a refusal recorded in inspectionRefusals'
// words, which a start before left, from the marker st describes, for a start
// whose own inspection of the backup failed, so the auto-installer's hold
// asks the backup's lookup again; a refusal of any other cause stands. The
// install marker's lock is the caller's, and st the marker read again under
// it, of the install the backup was checked for. A clear that cannot be
// written is said.
func clearInspectionRefusal(dataDir string, st updater.State, save func(string, updater.State) error, stderr io.Writer) {
	if !slices.Contains(inspectionRefusals, st.RollbackRefused) {
		return
	}
	st.RollbackRefused = ""
	if err := save(dataDir, st); err != nil {
		fmt.Fprintf(stderr, "updater: the install marker still records a backup that could not be inspected as a refused rollback: %v\n", err)
	}
}

// refuseCrashRollbackOnARead says why the crash rollback leaves an install
// that its first check refused on a read without the install marker's lock,
// and records it under that lock, on the marker read again and checked again,
// where the lock can be held: a lock that cannot be leaves the line alone.
// then is what the start does next (crashRollbackLine).
func refuseCrashRollbackOnARead(ctx context.Context, dataDir, current, why, then string, opts serveOpts, stderr io.Writer) {
	save := opts.saveUpdateState
	if save == nil {
		save = updater.SaveState
	}
	lock, err := updater.LockState(ctx, dataDir)
	if err != nil || !lock.Held() {
		lock.Release()
		if st, rerr := updater.LoadState(dataDir); rerr == nil {
			crashRollbackLine(stderr, st, why, then)
		}
		return
	}
	defer lock.Release()
	st, err := updater.LoadState(dataDir)
	if err != nil {
		return
	}
	if _, why := updater.CrashRollback(st, current); why != "" {
		crashRollbackLine(stderr, st, why, then)
		recordCrashRollbackRefusal(dataDir, st, why, save, stderr)
	}
}

// refuseCrashRollbackBeforeTheLock says why the crash rollback leaves the
// install whose marker, checked, it read without the install marker's lock,
// for a refusal decided before that lock (backlog B894: the backup's path, the
// backup a regular file and which file it is, the settings' read, the
// backup's version and its settings check), and records it under the lock,
// which it takes for that and lets go: said once and recorded (said, then
// recorded, which names no path) from the marker read again under it where
// the same install still asks for the rollback, its TargetVersion, its
// PreviousVersion and its AttemptedAt as checked has them (the three the main
// path holds the marker read again to, its PreviousVersion there through the
// version the backup answered); where the marker read again refuses the
// rollback itself, its own why said and recorded; and nothing said or written
// where it is another install, or none the rollback acts on. A refusal given
// no words to record (recorded empty: a backup that could not be inspected,
// backlog B898FU's round 3) is said there and recorded nowhere, and a refusal
// the marker records in the words such a failure had until then is cleared
// (clearInspectionRefusal), one of any other cause kept. Where the lock
// cannot be held, serve's stop has come (a lock that needed no wait is taken
// on a context that has ended), or the marker cannot be read again, said from
// checked and nothing recorded. Called only while this process does not hold
// the lock, a kernel lock on an open file, which a second take in one process
// waits on as on another process's hold. Nothing is put back. then is what the
// start does next (crashRollbackLine).
func refuseCrashRollbackBeforeTheLock(ctx context.Context, dataDir, current string, checked updater.State, said, recorded, then string,
	save func(string, updater.State) error, stderr io.Writer) {
	lock, err := updater.LockState(ctx, dataDir)
	if err != nil || !lock.Held() || ctx.Err() != nil {
		lock.Release()
		crashRollbackLine(stderr, checked, said, then)
		return
	}
	defer lock.Release()
	st, err := updater.LoadState(dataDir)
	if err != nil {
		crashRollbackLine(stderr, checked, said, then)
		return
	}
	switch ok, why := updater.CrashRollback(st, current); {
	case why != "":
		crashRollbackLine(stderr, st, why, then)
		recordCrashRollbackRefusal(dataDir, st, why, save, stderr)
	case ok && updater.SameVersion(st.TargetVersion, checked.TargetVersion) &&
		updater.SameVersion(st.PreviousVersion, checked.PreviousVersion) && st.AttemptedAt.Equal(checked.AttemptedAt):
		crashRollbackLine(stderr, st, said, then)
		if recorded == "" {
			clearInspectionRefusal(dataDir, st, save, stderr)
			return
		}
		recordCrashRollbackRefusal(dataDir, st, recorded, save, stderr)
	}
}

// sayCrashRollbackNeedsTheDataDir says, where serve did not take its data
// dir's lock, that an install whose count asks for the crash rollback is left
// as it is: only the data dir's owner puts the backup back. Nothing is
// written; then is what the start does next (crashRollbackLine).
func sayCrashRollbackNeedsTheDataDir(dataDir, then string, stderr io.Writer) {
	st, err := updater.LoadState(dataDir)
	if err != nil {
		return
	}
	if ok, _ := updater.CrashRollback(st, version.ServerVersion); ok {
		crashRollbackLine(stderr, st, "the data dir's lock is not this process's, and only its owner may put the backup back", then)
	}
}

// recoverCrashedRefusedInstall is the crash rollback (recoverCrashedInstall)
// for a start of this version that refused its settings, where the early
// rollback leaves the install and its count asks for the crash rollback
// (backlog B760): an install marker, or the crash rollback's own record whose
// rename did not land, with updater.ProbationFailedStarts starts of this
// version that did not end cleanly, which recorded that this version opened
// the data dir, so the early rollback refuses it. Until B760 such a start
// exited 2, as did every start after it while the settings stayed refused,
// and its crash rollback was never asked. It takes the data dir's lock, as the
// early rollback does, and asks the crash rollback whole, its own checks of
// the marker and the backup, the backup's settings check of the settings as
// they are now (a backup that refuses them too is not put back), and its
// record, its rename and its exit, its backup asked before the install
// marker's lock and the rollback decided again under it, as that rollback does
// (backlog B894); where it leaves the install it says why, and records it as it
// does at any start, and the start refuses its settings as it did before. A
// data dir's lock that cannot be taken leaves the install as it is, writes
// nothing, and says why: in the crash rollback's own words where it refuses
// whatever the lock, on the marker read again, and otherwise that only the
// data dir's owner puts the backup back (sayCrashRollbackNeedsTheDataDir).
// Until B760's second round the first was said nowhere, where before B760 the
// early rollback's reason was.
func recoverCrashedRefusedInstall(ctx context.Context, dataDir, configPath string, opts serveOpts, stderr io.Writer) (code int, acted bool) {
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		if st, rerr := updater.LoadState(dataDir); rerr == nil {
			if _, why := updater.CrashRollback(st, version.ServerVersion); why != "" {
				crashRollbackLine(stderr, st, why, crashRollbackRefusedStart)
				return 0, false
			}
		}
		sayCrashRollbackNeedsTheDataDir(dataDir, crashRollbackRefusedStart, stderr)
		return 0, false
	}
	defer release()
	return recoverCrashedInstall(ctx, dataDir, configPath, opts, crashRollbackRefusedStart, stderr)
}

// sayTheAdmissionsCount says what the boot's admission counted (backlog
// B660): one line where starts before this one did not end cleanly, in
// updater.ProbationStatus's words for this start, and one where its count's
// write answered an error, with what to check (backlog B760).
func sayTheAdmissionsCount(res updater.DisarmResult, stderr io.Writer) {
	if res.CountMissed != nil {
		fmt.Fprintf(stderr, "updater: %v; run bridge doctor, whose install-probation line says where the install stands\n", res.CountMissed)
		return
	}
	if !res.Counted || res.State.FailedStarts < 2 {
		return
	}
	if p := updater.ProbationStatus(res.State, version.ServerVersion, true); p.ThisStart != "" {
		fmt.Fprintf(stderr, "updater: %s\n", p.ThisStart)
	}
}

// bindRefusedByTheHost reports whether a listener's bind failed for an
// address another process holds, an address the host does not have (a static
// address not up yet after a reboot; B660's second round, where four such
// starts in a row had counted and the fourth put back a backup that met the
// same refusal), or a permission the host refuses (backlog B660): the host's
// trouble, which the backup would meet too, so the start takes back its
// count; a permission only where bindRefusalIsTheHosts says so. Asked
// through doctor's checks, whose Windows arm reads Winsock's errnos:
// syscall.EADDRINUSE, syscall.EADDRNOTAVAIL and syscall.EACCES alone are
// never what a bind answers there.
func bindRefusedByTheHost(err error) bool {
	return doctor.IsAddrInUse(err) || doctor.IsAddrNotAvail(err) || doctor.IsBindPermission(err)
}

// bindRefusalIsTheHosts reports whether a listener's bind that failed with err
// takes back the start's count (backlog B660): a refusal bindRefusedByTheHost
// reads as the host's, and for a permission one the backup would meet too. A
// permission is not the host's alone where the backup carries file
// capabilities this binary does not (the operator's setcap for a port below
// 1024, which a swap run by a user that may not set them does not carry,
// updater's carryCapabilities; B660's second round): the backup would bind,
// so the refusal stays counted, and the start says why. A capability that
// cannot be read, either side's, leaves the refusal the host's.
func bindRefusalIsTheHosts(err error, opts serveOpts, stderr io.Writer) bool {
	if !bindRefusedByTheHost(err) {
		return false
	}
	if !doctor.IsBindPermission(err) || doctor.IsAddrInUse(err) || doctor.IsAddrNotAvail(err) {
		return true
	}
	read := opts.fileCapability
	if read == nil {
		read = updater.FileCapabilities
	}
	binary := servingBinaryPath(opts, stderr)
	backup, berr := read(binary + ".bak")
	if berr != nil || len(backup) == 0 {
		return true
	}
	own, oerr := read(binary)
	if oerr != nil || bytes.Equal(own, backup) {
		return true
	}
	fmt.Fprintf(stderr, "updater: the backup beside this binary carries file capabilities this binary does not (a setcap the "+
		"update could not carry), and would bind where this one may not: this start counts as one that did not end cleanly\n")
	return false
}

// tailnetEndIsServes reports whether err, what the tailnet listener's Serve
// returned on f, ends this start as no failed start of an install (backlog
// B660's third round): only where serve itself began closing its tailnet side
// (f's stop, which marks itself begun before it shuts a server down or closes
// the node: closeBegun) or the server answers that it was closed
// (http.ErrServerClosed). The second round took any *net.OpError and
// net.ErrClosed for a failure of the tailnet's network the backup would meet
// too, but tsnet's listener (no TUN) answers net.ErrClosed once anything
// closes it and blocks while the tailnet is down, and an accept error is the
// listener's, so either can be this version's own fault, and both count. What
// is left of the classification: serve's select reads the listener's end only
// before its own stop of the tailnet side, which comes after it, so there
// every end but the server's closed answer counts today, and the begun close
// answers an end that races it.
func tailnetEndIsServes(f *tsnetFront, err error) bool {
	return errors.Is(err, http.ErrServerClosed) || (f != nil && f.closeBegun())
}
