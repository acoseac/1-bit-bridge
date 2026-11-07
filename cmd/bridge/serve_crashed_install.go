package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// takeBackWait is how long a start that ended cleanly gives the install
// marker's lock before it leaves its count where it is (backlog B660): a stop
// is not held up for the lock, and a count left behind errs toward a rollback
// of an install whose starts did end cleanly, which the confirmation clears.
const takeBackWait = 2 * time.Second

// recoverCrashedInstall is serve's answer to an install of this version whose
// starts have not ended cleanly (backlog B660): at
// updater.ProbationFailedStarts of them, this start puts the backup back
// before it writes anything, where every check allows it, and exits for the
// service manager to start the backup's version; otherwise it says why not,
// records it in the marker (State.RollbackRefused) where it holds the install
// marker's lock, and serve goes on starting, since this start may be the one
// that works. That start counts itself at its admission, so every later start
// that does not end cleanly asks again. acted says whether it put the backup
// back, and code is then serve's exit.
//
// It runs right after serve took the data dir's lock, and only where it did
// (the backup is put back by the data dir's owner alone), before the boot's
// admission (disarmEarlyRollback), so nothing of this start has written yet,
// and a start that finds the crash rollback's own record with its rename not
// landed finishes it, the record being the install still. It follows the
// early rollback (recoverRefusedInstall) step for step: the install marker's
// lock, held from its read through the rename (never without the exclusion),
// the marker read again under it, the backup a regular file whose `version`
// names the install's PreviousVersion, the hold and the veto recorded durably
// before the rename, and the marker put back where the rename fails. The
// three checks are asked in this order, the first starting no process, so
// that a store that was migrated does not run the backup at every start:
// updater.CrashRollback (the count, the swap, the versions, no migration of
// the store begun, no version but this one recorded over the data dir), the
// backup's version (updater.ProbeVersion), and the backup's settings check
// (updater.CheckConfig), so that a rollback does not trade one crash loop for
// another over a setting this version saved that the backup refuses. A
// backup that predates that check (ConfigCheckUnsupported: 0.2.2 and older)
// is not put back, and the start records why (B660's second round; until
// then it was put back with a warning): it cannot say whether it loads the
// settings, one that does not stops at its load with this version held back
// from the auto-installer, and its strict decoder refuses any key it does not
// know, which a later version's settings may name. The operator puts it back
// by hand where it loads them.
//
// A crash before the admission is not counted: a candidate that never runs
// is what the settings check before the swap sees, and a start refused for
// its settings has the early rollback. This rollback counts what got past
// both.
func recoverCrashedInstall(ctx context.Context, dataDir, configPath string, opts serveOpts, stderr io.Writer) (code int, acted bool) {
	current := version.ServerVersion
	st, err := updater.LoadState(dataDir)
	if err != nil {
		// The admission removes a marker it cannot read, and says so.
		return 0, false
	}
	if ok, why := updater.CrashRollback(st, current); !ok {
		if why != "" {
			refuseCrashRollbackOnARead(ctx, dataDir, current, why, opts, stderr)
		}
		return 0, false
	}
	save := opts.saveUpdateState
	if save == nil {
		save = updater.SaveState
	}
	stateLock, err := updater.LockState(ctx, dataDir)
	if err != nil {
		var refused *updater.StateLockError
		if !errors.As(err, &refused) {
			crashRollbackLine(stderr, st, "this start was stopped while it waited for the install marker's lock")
			return 0, false
		}
		crashRollbackLine(stderr, st, fmt.Sprintf("the install marker's lock cannot be taken (%v), and only its holder may put the backup back", err))
		return 0, false
	}
	defer stateLock.Release()
	if !stateLock.Held() {
		crashRollbackLine(stderr, st, "the data dir's filesystem keeps no locks, so nothing would keep a store's migration out while the backup is put back")
		return 0, false
	}
	// The marker as it is now that its lock is this process's.
	if st, err = updater.LoadState(dataDir); err != nil {
		crashRollbackLine(stderr, updater.State{Status: "installing", TargetVersion: current, FailedStarts: updater.ProbationFailedStarts},
			"the install marker cannot be read again")
		return 0, false
	}
	refuse := func(said, recorded string) {
		crashRollbackLine(stderr, st, said)
		recordCrashRollbackRefusal(dataDir, st, recorded, save, stderr)
	}
	if ok, why := updater.CrashRollback(st, current); !ok {
		if why != "" {
			refuse(why, why)
		}
		return 0, false
	}
	binaryPath := servingBinaryPath(opts, stderr)
	if !filepath.IsAbs(binaryPath) {
		refuse("this binary's path is not known, nor so where its backup is", "this binary's path is not known, nor so where its backup is")
		return 0, false
	}
	backup := binaryPath + ".bak"
	if fi, err := os.Lstat(backup); err != nil || !fi.Mode().IsRegular() {
		refuse(fmt.Sprintf("there is no backup to put back, a regular file at %s", backup), "there is no backup beside the binary to put back")
		return 0, false
	}
	got, err := updater.ProbeVersion(ctx, backup)
	if err != nil {
		refuse(fmt.Sprintf("the backup at %s did not say its version: %v", backup, err), "the backup did not say its version")
		return 0, false
	}
	if !updater.SameVersion(got, st.PreviousVersion) {
		refuse(fmt.Sprintf("the backup at %s is %s, where the install replaced %s", backup, got, st.PreviousVersion),
			fmt.Sprintf("the backup is %s, where the install replaced %s", got, st.PreviousVersion))
		return 0, false
	}
	check := updater.CheckConfig(ctx, backup, configPath)
	switch check.Verdict {
	case updater.ConfigValid:
	case updater.ConfigCheckUnsupported:
		refuse(fmt.Sprintf("the backup, %s, predates the settings check, so it cannot say whether it loads this bridge's settings, "+
			"and one that does not would not start once put back", st.PreviousVersion),
			"the backup predates the settings check")
		return 0, false
	case updater.ConfigRefused:
		why := fmt.Sprintf("the backup, %s, refuses this bridge's settings, so it would not start on them: %s",
			st.PreviousVersion, strings.Join(check.Reasons, "; "))
		refuse(why, why)
		return 0, false
	default:
		if check.Err != nil {
			crashRollbackLine(stderr, st, "this start was stopped while the backup checked the settings")
			return 0, false
		}
		refuse(fmt.Sprintf("the backup's settings check did not answer: %s", check.Detail),
			fmt.Sprintf("the backup, %s, could not check this bridge's settings", st.PreviousVersion))
		return 0, false
	}
	// The hold and the veto first, durably, then the rename, as the early
	// rollback does: a rename that landed before them would leave the backup's
	// version free to install this one again, and this version's commands to
	// migrate the store past the binary put back.
	record := updater.CrashRollbackRecord(st, current)
	if err := save(dataDir, record); err != nil {
		crashRollbackLine(stderr, st, fmt.Sprintf("could not record %s as held back (%v), and a backup put back without that record would "+
			"install it again", current, err))
		return 0, false
	}
	restore := opts.restoreBackup
	if restore == nil {
		restore = updater.RollbackBinary
	}
	if err := restore(binaryPath, ".bak"); err != nil {
		// The install marker back as it was, with why: the binary in place is
		// still this version's.
		st.RollbackRefused = "the backup could not be put back over the binary"
		if perr := save(dataDir, st); perr != nil {
			crashRollbackLine(stderr, st, fmt.Sprintf("could not put the backup back (%v), nor the install marker as it was (%v)", err, perr))
			return 0, false
		}
		crashRollbackLine(stderr, st, fmt.Sprintf("could not put the backup back: %v", err))
		return 0, false
	}
	fmt.Fprintf(stderr, "updater: %d starts of %s since its install did not end cleanly, so the install is rolled back: %s is back at %s "+
		"and %s is held back from the auto-installer; exiting for the service manager to start %s (where nothing starts it, "+
		"run bridge serve again)\n", st.FailedStarts, current, st.PreviousVersion, binaryPath, current, st.PreviousVersion)
	return supervision.RestartExitCode, true
}

// crashRollbackLine says why the crash rollback leaves the install st
// describes, for an install of this version whose count asked for it; the
// line may name the backup's path, as the early rollback's do.
func crashRollbackLine(stderr io.Writer, st updater.State, why string) {
	fmt.Fprintf(stderr, "updater: not rolling back the install of %s after %d starts of it that did not end cleanly: %s; "+
		"this start goes on, and counts itself\n", st.TargetVersion, st.FailedStarts, why)
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

// refuseCrashRollbackOnARead says why the crash rollback leaves an install
// that its first check refused on a read without the install marker's lock,
// and records it under that lock, on the marker read again and checked again,
// where the lock can be held: a lock that cannot be leaves the line alone.
func refuseCrashRollbackOnARead(ctx context.Context, dataDir, current, why string, opts serveOpts, stderr io.Writer) {
	save := opts.saveUpdateState
	if save == nil {
		save = updater.SaveState
	}
	lock, err := updater.LockState(ctx, dataDir)
	if err != nil || !lock.Held() {
		lock.Release()
		if st, rerr := updater.LoadState(dataDir); rerr == nil {
			crashRollbackLine(stderr, st, why)
		}
		return
	}
	defer lock.Release()
	st, err := updater.LoadState(dataDir)
	if err != nil {
		return
	}
	if _, why := updater.CrashRollback(st, current); why != "" {
		crashRollbackLine(stderr, st, why)
		recordCrashRollbackRefusal(dataDir, st, why, save, stderr)
	}
}

// sayCrashRollbackNeedsTheDataDir says, where serve did not take its data
// dir's lock, that an install whose count asks for the crash rollback is left
// as it is: only the data dir's owner puts the backup back. Nothing is
// written.
func sayCrashRollbackNeedsTheDataDir(dataDir string, stderr io.Writer) {
	st, err := updater.LoadState(dataDir)
	if err != nil {
		return
	}
	if ok, _ := updater.CrashRollback(st, version.ServerVersion); ok {
		crashRollbackLine(stderr, st, "the data dir's lock is not this process's, and only its owner may put the backup back")
	}
}

// sayTheAdmissionsCount says what the boot's admission counted (backlog
// B660): one line where starts before this one did not end cleanly, in
// updater.ProbationStatus's words for this start, and one where this start
// could not be counted.
func sayTheAdmissionsCount(res updater.DisarmResult, stderr io.Writer) {
	if res.CountMissed != nil {
		fmt.Fprintf(stderr, "updater: %v; a start that does not end cleanly may then not count toward the install's rollback\n", res.CountMissed)
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

// tailnetNetworkFailure reports whether err, what the tailnet listener's
// Serve returned, is a failure of the tailnet's network (backlog B660's
// second round): an operation of the net package's that failed (its accept,
// tsnet's netstack answering a closed or reset endpoint as a *net.OpError),
// or the listener closed under it, which tsnet answers once its node stops
// (net.ErrClosed). Those the backup would meet too. Anything else the
// listener answers is not the network's, and counts.
func tailnetNetworkFailure(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) || errors.Is(err, net.ErrClosed)
}

// tailnetEndIsServes reports whether err, what the tailnet listener's Serve
// returned on f, ends this start as no failed start of an install.
func tailnetEndIsServes(f *tsnetFront, err error) bool {
	return tailnetNetworkFailure(err)
}

// takeBackFailedStart takes back this start's count of the install's starts
// that did not end cleanly (updater.TakeBackFailedStart), counted being what
// its admission tried to write: serve's defer calls it after every teardown,
// holding the data dir's lock, where the start ended in a listener's bind the
// host refused or a failure of the tailnet's network, and where a stop's own
// take-back (stopTakeBack) did not have its answer before the teardown ended.
// It gives the install marker's lock takeBackWait. A take-back that does not
// land leaves one count, said on one line.
func takeBackFailedStart(dataDir string, counted int, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), takeBackWait)
	defer cancel()
	if _, err := updater.TakeBackFailedStart(ctx, dataDir, version.ServerVersion, counted); err != nil {
		fmt.Fprintf(stderr, "updater: this start ended cleanly and its count of the install's starts was not taken back (%v): "+
			"it counts as one that did not\n", err)
	}
}
