package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// servingBinaryPath is the binary serve runs as: os.Executable, its links
// resolved, or the boot test's stand-in (serveOpts.binaryPath). The updater
// swaps the file at this path and keeps its backup beside it, so this value
// feeds maybeRollbackOnBoot, updateInfoAdapter.binaryPath,
// AutoInstallOpts.BinaryPath and the early rollback of an install whose
// version refuses the settings.
//
// os.Executable may fail in unusual environments (a deleted binary
// running); argv[0] is the fallback, so the failure surfaces later in
// Install's preflight rather than blocking the whole server boot.
//
// Links are resolved so a symlinked install swaps the REAL binary, not the
// link, matching what update.go, init.go and menu.go do at their own
// os.Executable sites (update.go's comment: "Mirrors the binary resolution
// in init.go's service install"). On darwin os.Executable returns the
// INVOCATION path unresolved, so an admin-console install via
// /usr/local/bin/bridge -> /opt/.../bridge replaced the symlink with a
// regular file: the real binary stayed at the old version, launchd kept
// running it, and `bridge version` on PATH reported the new one.
// Best-effort: a resolve failure keeps the unresolved path rather than
// blocking boot (F0c).
func servingBinaryPath(opts serveOpts, stderr io.Writer) string {
	if opts.binaryPath != "" {
		return opts.binaryPath
	}
	binaryPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "updater: os.Executable failed (install path may not work): %v\n", err)
		binaryPath = os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(binaryPath); err == nil {
		binaryPath = resolved
	}
	return binaryPath
}

// recoverRefusedInstall is serve's answer to settings this version refuses at
// the first start after the updater installed it (backlog B613). An updater
// that runs no settings check before its swap (0.2.1's and 0.2.2's) installs
// a version that exits 2 at the settings' load, before the boot rollback
// (maybeRollbackOnBoot) is reached, and the service manager starts it again
// and again. Here, before serve has written anything, it puts the previous
// binary back and exits for the service manager to start that, when it can
// prove the install, and otherwise does nothing: serve then refuses to start
// with the settings' error, as it always has. reasons are the refusal's, as
// the settings check would give them; acted says whether it did anything, and
// code is then serve's exit.
//
// Everything is read without the full parse, which is what failed: the data
// dir from the settings' dataDir key, the environment and the settings
// file's directory, as Load would give it (config.DataDirOf), and the
// marker there. The install is proved when the marker is a recent install
// of this version over another, which reached its swap, and which no
// version but the backup's has gone on past its settings under
// (updater.EarlyRollback); when the data dir's lock is this process's, so no
// other serve, restore or clear-missing owns it; and when the backup beside
// this binary is a regular file whose `version` names the version the
// marker says the install replaced (updater.ProbeVersion: a version check,
// not a proof of its bytes); and the install marker's lock is this process's
// too, from the marker's read to the rename and any write after it, so that
// no store open migrates in between (updater.LockState). Then this version
// is held back from the auto-installer for these settings, recorded durably
// first (a hold that cannot be recorded leaves the install as it is), the
// backup is renamed over this binary (updater.RollbackBinary: one rename on
// Unix; on Windows the running image is moved aside first and put back if
// the second rename fails), and serve exits with supervision.RestartExitCode,
// which every service definition the bridge installs restarts: systemd's
// Restart=always, launchd's KeepAlive on an unsuccessful exit, the SCM's
// recovery actions for a service-specific exit code. The settings are never
// written.
//
// The store has not been opened when this runs: it runs before the data
// dir's lock, which comes before serve's first write, so no migration of this
// version's can have run in this start, and disarmEarlyRollback covers the
// earlier ones.
func recoverRefusedInstall(ctx context.Context, configPath string, reasons []string, opts serveOpts, stderr io.Writer) (code int, acted bool) {
	dataDir, err := config.DataDirOf(configPath)
	if err != nil {
		return 0, false
	}
	st, err := updater.LoadState(dataDir)
	if err != nil {
		return 0, false
	}
	if ok, why := updater.EarlyRollback(st, version.ServerVersion, time.Now().UTC()); !ok {
		noRollback(stderr, st, why)
		return 0, false
	}
	binaryPath := servingBinaryPath(opts, stderr)
	if !filepath.IsAbs(binaryPath) {
		noRollback(stderr, st, "this binary's path is not known, nor so where its backup is")
		return 0, false
	}
	backup := binaryPath + ".bak"
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		var held *serveDataDirHeldError
		if errors.As(err, &held) {
			noRollback(stderr, st, held.Error())
		} else {
			noRollback(stderr, st, fmt.Sprintf("the data dir cannot be locked (%v), and only its owner may put the backup back", err))
		}
		return 0, false
	}
	defer release()
	// And the install marker's lock, inside the data dir's (backlog B613's
	// round 3): a store open of this version that migrates (a `bridge doctor`,
	// a scan) holds it while it records that it opened the data dir, so that
	// record cannot land between this read and the rename, and this rollback
	// holds it from its read through the rename and any write that puts the
	// install back, so that such an open waits for it and then refuses to
	// migrate under the version put back (updater.MigrationVetoed: the record
	// vetoes this version's store migrations, and the veto outlives it, round
	// 4). It is never held without the exclusion: a lock that cannot be
	// taken, or a filesystem that keeps none, leaves the install as it is, and
	// so does serve's stop while it waits.
	stateLock, err := updater.LockState(ctx, dataDir)
	if err != nil {
		var refused *updater.StateLockError
		if !errors.As(err, &refused) {
			// Serve's own stop, while it waited (round 4).
			noRollback(stderr, st, "this start was stopped while it waited for the install marker's lock")
			return 0, false
		}
		noRollback(stderr, st, fmt.Sprintf("the install marker's lock cannot be taken (%v), and only its holder may put the backup back", err))
		return 0, false
	}
	defer stateLock.Release()
	if !stateLock.Held() {
		noRollback(stderr, st, "the data dir's filesystem keeps no locks, so nothing would keep a store's migration out while the backup is put back")
		return 0, false
	}
	// The marker as it is now that the data dir is this process's: another
	// process may have changed it since the first read.
	if st, err = updater.LoadState(dataDir); err != nil {
		noRollback(stderr, updater.State{Status: "installing", TargetVersion: version.ServerVersion},
			fmt.Sprintf("the install marker cannot be read again (%s)", fsutil.ErrorWithoutPath(err)))
		return 0, false
	}
	if ok, why := updater.EarlyRollback(st, version.ServerVersion, time.Now().UTC()); !ok {
		noRollback(stderr, st, why)
		return 0, false
	}
	if fi, err := os.Lstat(backup); err != nil || !fi.Mode().IsRegular() {
		noRollback(stderr, st, fmt.Sprintf("there is no backup to put back, a regular file at %s", backup))
		return 0, false
	}
	got, err := updater.ProbeVersion(ctx, backup)
	if err != nil {
		noRollback(stderr, st, fmt.Sprintf("the backup at %s did not say its version: %v", backup, err))
		return 0, false
	}
	if !updater.SameVersion(got, st.PreviousVersion) {
		noRollback(stderr, st, fmt.Sprintf("the backup at %s is %s, where the install replaced %s", backup, got, st.PreviousVersion))
		return 0, false
	}
	// The hold first, durably, then the rename (backlog B613's round 2,
	// Codex's finding 5 and Argon's): the backup is 0.2.1's or 0.2.2's binary
	// as often as not, whose boot reads an install marker it cannot finish (it
	// is not the version the marker names, and the backup is gone) as an
	// install that failed and clears it, so a rename that landed before the
	// hold was recorded, or a hold that then could not be written, left that
	// binary free to install this version again, unchecked. The record keeps
	// the install it undoes (updater.RollbackRecord): a start of this version
	// that finds it, the backup not back, finishes the rollback.
	reason := fmt.Sprintf("%s refused this bridge's settings at its first start and was rolled back to %s: %s",
		version.ServerVersion, st.PreviousVersion, strings.Join(reasons, "; "))
	digest, err := updater.ConfigDigest(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "updater: could not read the settings to hold %s back against (%v); holding it back whatever they are\n",
			version.ServerVersion, err)
		digest = ""
	}
	save := opts.saveUpdateState
	if save == nil {
		save = updater.SaveState
	}
	if err := save(dataDir, updater.RollbackRecord(st, version.ServerVersion, reason, digest)); err != nil {
		noRollback(stderr, st, fmt.Sprintf("could not record %s as held back (%v), and a backup put back without that record would install it again",
			version.ServerVersion, err))
		return 0, false
	}
	restore := opts.restoreBackup
	if restore == nil {
		restore = updater.RollbackBinary
	}
	if err := restore(binaryPath, ".bak"); err != nil {
		// The install marker back as it was, so that a start that gets past
		// these settings confirms the install as it would have.
		if perr := save(dataDir, st); perr != nil {
			noRollback(stderr, st, fmt.Sprintf("could not put the backup back (%v), nor the install marker as it was (%v)", err, perr))
			return 0, false
		}
		noRollback(stderr, st, fmt.Sprintf("could not put the backup back: %v", err))
		return 0, false
	}
	fmt.Fprintf(stderr, "updater: %s refuses this bridge's settings, so the install is rolled back: %s is back at %s and %s is held back from the "+
		"auto-installer; the settings file is unchanged, so fix it before installing %s again; exiting for the service manager to start %s\n",
		version.ServerVersion, st.PreviousVersion, binaryPath, version.ServerVersion, version.ServerVersion, st.PreviousVersion)
	return supervision.RestartExitCode, true
}

// noRollback reports why the early rollback leaves the install st describes,
// for an install of this version; any other marker is nothing to explain.
func noRollback(stderr io.Writer, st updater.State, why string) {
	if why == "" || !updater.InstallArmed(st) || !updater.SameVersion(st.TargetVersion, version.ServerVersion) {
		return
	}
	fmt.Fprintf(stderr, "updater: not rolling back the install of %s: %s\n", st.TargetVersion, why)
}

// disarmEarlyRollback is the boot's admission: it records, before serve's
// first write to the data dir and so before the store's migrations, that this
// version went on past its settings while an install marker was armed
// (updater.AdmitBoot, under the install marker's lock, inside the data dir's,
// released before the store opens; the store's open asks
// updater.DisarmBeforeMigrating before it migrates, for every command that
// opens it). From then on the early rollback of a later start
// (recoverRefusedInstall) and the boot rollback (BootInstallFailed) leave
// the backup where it is: what this version writes may be what the backup's
// version does not know, so a restore would be a downgrade past it. Over the
// early rollback's record of an install of this version, left by a start
// stopped before the backup was back, the record resolves the rollback, so
// the store's open migrates after it, and the housekeeping confirms the
// install (BootRollbackOvertaken; backlog B613's round 3). It lifts a veto of
// this version's store migrations that a rollback left (round 4): this start
// got past its settings, which is what the veto waited for. An error is
// serve's to refuse to start over, but for ctx's own (serve's stop while it
// waits for the install marker's lock): it is called where nothing has been
// written yet.
func disarmEarlyRollback(ctx context.Context, dataDir string, opts serveOpts, stderr io.Writer) error {
	res, err := updater.AdmitBoot(ctx, dataDir, version.ServerVersion, opts.saveUpdateState)
	if err != nil {
		return err
	}
	if res.Unreadable != nil {
		fmt.Fprintf(stderr, "updater: removed an install marker that could not be read (%s)\n", fsutil.ErrorWithoutPath(res.Unreadable))
	}
	if res.VetoLifted {
		fmt.Fprintf(stderr, "updater: %s starts on settings it loads, so the veto of its store migrations that a rollback left is lifted\n",
			version.ServerVersion)
	}
	return nil
}
