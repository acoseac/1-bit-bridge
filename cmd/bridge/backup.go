package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// backupCmd is `bridge backup [--config X] [--keep N]` — captures a
// timestamped snapshot of the bridge state under `<dataDir>/backups/`
// and optionally prunes older snapshots to retain at most N.
//
// Safe to run while `bridge serve` is up: SQLite's `VACUUM INTO` on
// a WAL-mode database produces an atomic clean copy without locking
// out the running process. The other captured files (tokens.json,
// cert/key, config, adminauth.json when the console has an account,
// atlas-harvest.json when a harvest credential is stored, player-uuid
// once the player has an identity, and its matching durable reservation,
// and the player's DSD state, player-dsd.json, once serve's player writes
// one) are read normally — a write race with
// serve is harmless for these because they're mutated rarely and
// atomically (temp + rename). A restore brings the console account back
// and signs its sessions out, and gives the player the identity it had with
// both reservation marks raised past the snapshot and live marks.
func backupCmd(args []string, stdout, stderr io.Writer) int {
	return backupCmdContext(context.Background(), args, stdout, stderr)
}

// backupCmdContext is backupCmd on ctx, which run's signal handling ends: a
// verification it stops writes INCOMPLETE and says so (backlog B653).
//
// Every snapshot takes the one path (backup.SnapshotAndVerify): the capture,
// then its verification (the database's baseline, SQLite's quick_check and
// the schema its version requires; the other files' sizes and hashes), then
// the prune, whatever the verdict. The command says what was kept, and
// exits 0 only for a snapshot both of whose verdicts are VERIFIED (or one of
// an install that has no database yet, whose files are).
//
// `bridge backup --verify <snapshot> [--full]` takes no snapshot: it checks
// the one given, which must be a snapshot in the backups folder of the
// install its config names, under that folder's lock, records its verdict
// and says it (verifyCmd). --full runs SQLite's integrity_check and
// foreign_key_check where the default runs quick_check.
func backupCmdContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	keep := fs.Int("keep", 7, "number of snapshots to retain (0 disables prune)")
	verify := fs.String("verify", "", "check the snapshot at this path and record its verdict; takes no snapshot")
	full := fs.Bool("full", false, "with --verify: run SQLite's integrity_check and foreign_key_check (slower) in place of quick_check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "backup: unexpected argument %q\n", fs.Arg(0))
		fmt.Fprintln(stderr, "Usage: bridge backup [--config bridge.yaml] [--keep N] | bridge backup --verify <snapshot-dir> [--full]")
		return 2
	case strings.HasPrefix(*verify, "-"):
		fmt.Fprintln(stderr, "backup: --verify takes the snapshot's folder: bridge backup --verify <snapshot-dir> [--full]")
		return 2
	case *full && *verify == "":
		fmt.Fprintln(stderr, "backup: --full goes with --verify <snapshot-dir>")
		return 2
	case *verify != "":
		return verifyCmd(ctx, *configPath, *verify, *full, stdout, stderr)
	}

	cfg, resolvedCfgPath, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	if refuseDataDirAway("backup", resolvedCfgPath, cfg.DataDir, stderr) {
		return 1
	}

	src := buildBackupSources(cfg, resolvedCfgPath)
	// The prune runs unconditionally, even at --keep 0. That disables the
	// KEEP-POLICY, not the crash-orphan sweep: an orphan is never a snapshot
	// worth retaining, it carries a near-full DB copy, and nothing else in
	// the backup package can ever reclaim it. Guarding the call on keep > 0
	// meant "retention off" also silently meant "orphans accumulate forever,
	// and their warnings are never seen". It runs after a kept snapshot
	// whatever its verdict (backup.Finish).
	res := backup.SnapshotAndVerify(ctx, src, backup.SnapshotOptions{
		Keep: func() int { return *keep },
		OnKept: func(dir string) {
			fmt.Fprintf(stdout, "Snapshot written: %s\n", dir)
			fmt.Fprintln(stdout, backup.SensitivityNotice)
		},
	})
	if !res.Kept() {
		fmt.Fprintf(stderr, "snapshot: %v\n", res.CaptureErr)
		return 1
	}
	code := 0
	if !reportVerdict(stdout, stderr, "", res.Verify, res.VerifyErr) {
		code = 1
	}
	// The orphan sweep's outcome is a WARNING, never the command's
	// exit status: a directory it can't classify (an unreadable
	// manifest it deliberately refuses to delete) is permanent, so
	// treating it as failure made every subsequent `bridge backup`
	// exit 1 even though the snapshot and the prune both worked.
	if res.Prune.ReapErr != nil {
		fmt.Fprintf(stderr, "prune: orphan sweep reported a problem (snapshot and prune are unaffected): %v\n", res.Prune.ReapErr)
	}
	switch {
	case res.PruneErr != nil:
		fmt.Fprintf(stderr, "prune: %v\n", res.PruneErr)
		return 1
	case res.PruneSkipped && ctx.Err() == nil:
		fmt.Fprintf(stdout, "Prune skipped: %s.\n", pruneSkipWords(res.VerifyErr))
	case res.Prune.Deleted > 0:
		fmt.Fprintf(stdout, "Pruned %d older snapshot(s) (kept %d most recent, and what recovery needs).\n", res.Prune.Deleted, *keep)
	}
	return code
}

// verifyCmdLockWait is how long bridge backup --verify waits for another
// verification or prune of the snapshots: backup.VerifyCmdLockWait, which a
// test shortens.
var verifyCmdLockWait = backup.VerifyCmdLockWait

// verifyCmd is `bridge backup [--config X] --verify <snapshot> [--full]`: the
// check of a snapshot in the backups folder of the install the config names,
// under that folder's lock, however the path names it (a relative path, `.`,
// a link: backup resolves each, and the folder, before it compares them),
// waiting for another verification or prune at most verifyCmdLockWait, its
// verdict recorded and said. 0 for a snapshot both of whose verdicts are
// VERIFIED once the record is written, 1 otherwise, 2 for a path that holds no
// snapshot of that install, or a config that does not load.
func verifyCmd(ctx context.Context, configPath, path string, full bool, stdout, stderr io.Writer) int {
	dir := backup.CleanSnapshotPath(path)
	if !backup.LooksLikeSnapshotDir(dir) {
		fmt.Fprintf(stderr, "backup: %q is not a recognized snapshot directory (no readable manifest.json with a supported schema)\n", dir)
		return 2
	}
	// The install's config says where its backups folder is, whose lock the
	// prune takes: a snapshot is verified there and nowhere else.
	cfg, resolvedCfgPath, err := loadCLIConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	if refuseDataDirAway("backup", resolvedCfgPath, cfg.DataDir, stderr) {
		return 1
	}
	backupsRoot := filepath.Join(cfg.DataDir, backup.BackupsDirName)
	level := backup.CheckQuick
	if full {
		level = backup.CheckFull
	}
	fmt.Fprintf(stdout, "Verifying %s (%s check)…\n", dir, level)
	res, err := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{Level: level, LockWait: verifyCmdLockWait, BackupsRoot: backupsRoot})
	switch {
	case errors.Is(err, backup.ErrOutsideBackups):
		fmt.Fprintf(stderr, "backup: %q is not a snapshot in the backups folder of the install %s configures (%s); a copy elsewhere is not verified in place: copy it into that folder, or name its own install's config with --config\n",
			dir, resolvedCfgPath, backupsRoot)
		return 2
	case errors.Is(err, backup.ErrBackupsBusy):
		fmt.Fprintf(stderr, "backup: another verification or prune of these snapshots held their lock for %s (a serving bridge's, or another command's); run this again once it ends\n",
			verifyCmdLockWait)
		return 1
	case errors.Is(err, backup.ErrBackupsUnlocked):
		fmt.Fprintf(stderr, "backup: nothing was verified: %v; with no lock nothing would keep a prune from removing the snapshot under its check. Put the data directory on a filesystem that grants file locks (a local disk, or a network mount whose lock service runs).\n", err)
		return 1
	case errors.Is(err, backup.ErrCheckStillRunning):
		fmt.Fprintf(stderr, "backup: nothing was verified: %v. %s\n", err,
			stillRunningEnd(err, "restart the bridge, or stop the bridge backup --verify that runs it"))
		return 1
	}
	if res.Legacy {
		describeVerdict(stdout, res.Found)
		fmt.Fprintln(stdout, "This snapshot was written before snapshots were verified: it records no file's size or hash, so only its files' presence was checked, and nothing was recorded.")
		return 1
	}
	if !reportVerdict(stdout, stderr, "", res, err) {
		return 1
	}
	return 0
}

// reportVerdict says what a kept snapshot's verification found and what its
// record holds now, on stdout, and why the verdict was not recorded or the
// snapshot does not pass, on stderr, each line led by prefix. It reports
// whether the snapshot passes: both verdicts VERIFIED (the database's
// UNVERIFIED where the install had none to capture), recorded, and read
// back. Where the verdict was not recorded it says the two apart: what this
// check found, and what the record reads, as every reader reads it until a
// check is recorded; of a snapshot not checked again
// (backup.ErrRecheckRefused), the record an earlier check left, which this
// one did not repeat; of one checked again where no check over its VERIFIED
// is recorded (backup.ErrRecheckNotRecorded: Windows), what this check found
// and the record as readers now read it; and of a verification given up
// (backup.ErrCheckGivenUp), how far its write went and what its record reads
// as read back, never what the INCOMPLETE it was given up with would say (its
// merge keeps a completed verdict, and its write may not have landed), and
// that the step still holds the snapshots. How far a write went is said from
// its outcome (backup.VerifyResult.Write), never from the error, and apart
// from the read-back: a record that was not read back is said so in every
// case, and why (readWords), never printed, and what the check found never
// stands in for it, so a durable write whose read-back failed says it wrote
// its record and that its record could not be read back, and fails (backlog
// B653's sixth round).
func reportVerdict(stdout, stderr io.Writer, prefix string, res backup.VerifyResult, err error) bool {
	switch {
	case res.NotStarted(err):
		fmt.Fprintf(stderr, "%sthe verification did not start: %v\n", prefix, err)
		return false
	case errors.Is(err, backup.ErrCheckGivenUp):
		fmt.Fprintf(stderr, "%sthe verification was given up, and its step still holds the snapshots until it ends: %v\n", prefix, err)
		describeRecordRead(stdout, prefix, res)
		return false
	case errors.Is(err, backup.ErrRecheckNotRecorded):
		fmt.Fprintf(stdout, "%sthis check found:\n", prefix)
		describeVerdictLed(stdout, prefix+"  ", res.Found)
		describeRecordOr(stdout, prefix, res, "its record reads now:")
		fmt.Fprintf(stderr, "%swhat this check found was not recorded: %v\n", prefix, err)
		return false
	case errors.Is(err, backup.ErrRecheckRefused):
		describeRecordOr(stdout, prefix, res, "its record, from an earlier check, which this one did not repeat:")
		fmt.Fprintf(stderr, "%sthe snapshot was not checked: %v\n", prefix, err)
		return false
	case err != nil && res.Found.DatabaseState != "":
		fmt.Fprintf(stdout, "%sthis check found:\n", prefix)
		describeVerdictLed(stdout, prefix+"  ", res.Found)
		describeRecordOr(stdout, prefix, res, "its record reads, for every reader until a check is recorded:")
		fmt.Fprintf(stderr, "%sthe snapshot was kept, and %s: %v\n", prefix, keptWords(res), err)
		return false
	case err != nil:
		fmt.Fprintf(stderr, "%sthe snapshot was kept, and %s: %v\n", prefix, keptWords(res), err)
		return false
	}
	rec := res.Record
	if res.Found.DatabaseState != "" {
		describeVerdictLed(stdout, prefix, rec)
	}
	if !rec.Passes() {
		fmt.Fprintf(stderr, "%sthe snapshot was kept, and it is not a verified recovery point: database %s, files %s\n",
			prefix, backup.StateWords(rec.DatabaseState, rec.DatabaseReasons), backup.FilesWords(rec))
		return false
	}
	return true
}

// describeRecordOr prints the record as read back, led by lead, or says why
// it was not read back (readWords).
func describeRecordOr(w io.Writer, prefix string, res backup.VerifyResult, lead string) {
	if !res.RecordRead {
		fmt.Fprintf(w, "%s%s\n", prefix, readWords(res))
		return
	}
	fmt.Fprintf(w, "%s%s\n", prefix, lead)
	describeVerdictLed(w, prefix+"  ", res.Record)
}

// describeRecordRead says how far a verification given up went with its
// record and what that record reads as read back, its two verdicts, or why it
// was not read back, which says nothing of whether a write landed
// (recordWords, backlog B653's fifth and sixth rounds).
func describeRecordRead(w io.Writer, prefix string, res backup.VerifyResult) {
	if !res.RecordRead {
		fmt.Fprintf(w, "%s%s\n", prefix, recordWords(res))
		return
	}
	fmt.Fprintf(w, "%s%s:\n", prefix, recordWords(res))
	describeVerdictLed(w, prefix+"  ", res.Record)
}

// recordWords says how far a verification's write of its record went
// (writeWords), and leads into what its record reads as read back ("…
// reads"), or says why it was not read back (readWords).
func recordWords(res backup.VerifyResult) string {
	switch {
	case !res.RecordRead:
		return writeWords(res.Write) + "; " + readWords(res)
	case res.Write == backup.WriteRecorded:
		return "it wrote its record, which reads"
	case res.Write == backup.WriteNotDurable:
		return "it wrote its record, which is not known to survive a crash and reads"
	}
	return writeWords(res.Write) + ", and its record reads"
}

// keptWords says how far the write of what a check found went, of a kept
// snapshot whose verification answered an error once its check had run: from
// the write's outcome (res.Write), never from the error, which may be its
// read-back's alone; and, where its record was not read back, that too, and
// why (readWords).
func keptWords(res backup.VerifyResult) string {
	w := "what this check found was not recorded"
	switch res.Write {
	case backup.WriteNotDurable:
		w = "what this check found was written and is not known to survive a crash"
	case backup.WriteRecorded, backup.WriteUnchanged, backup.WriteUnfinished, backup.WriteUnknown:
		w = writeWords(res.Write)
	}
	if !res.RecordRead {
		w += "; " + readWords(res)
	}
	return w
}

// readWords says why a verification's record was not read back: its
// read-back was given up, or it read nothing (the error the verification
// answers says why), or none was made.
func readWords(res backup.VerifyResult) string {
	switch {
	case errors.Is(res.ReadErr, backup.ErrReadBackGivenUp):
		return "its read-back was given up, so its record was not read"
	case res.ReadErr != nil:
		return "its record could not be read back"
	}
	return "its record was not read back"
}

// writeWords says how far a verification's write of its record went: "it
// wrote nothing" only where no write began, and "its write did not finish"
// where one began and was given up, which may have landed.
func writeWords(o backup.WriteOutcome) string {
	switch o {
	case backup.WriteRecorded:
		return "it wrote its record"
	case backup.WriteNotDurable:
		return "it wrote its record, which is not known to survive a crash"
	case backup.WriteUnchanged:
		return "its write found nothing to change"
	case backup.WriteFailed:
		return "its write failed"
	case backup.WriteUnfinished:
		return "its write did not finish"
	case backup.WriteUnknown:
		return "its write outcome is unknown"
	}
	return "it wrote nothing"
}

// describeVerdict prints v's two verdicts, a line each.
func describeVerdict(w io.Writer, v backup.Verification) { describeVerdictLed(w, "", v) }

// describeVerdictLed is describeVerdict with each line led by prefix.
func describeVerdictLed(w io.Writer, prefix string, v backup.Verification) {
	db := backup.StateWords(v.DatabaseState, v.DatabaseReasons)
	var facts []string
	if v.Level != "" && (v.DatabaseState == backup.StateVerified || v.DatabaseState == backup.StateFailed) {
		facts = append(facts, string(v.Level)+" check")
	}
	if v.UserVersion > 0 {
		facts = append(facts, fmt.Sprintf("user_version %d", v.UserVersion))
	}
	if len(v.Counts) > 0 {
		var counts []string
		for _, table := range []string{"tracks", "folders", "playlists", "playlist_items"} {
			if n, ok := v.Counts[table]; ok {
				counts = append(counts, fmt.Sprintf("%s %d", table, n))
			}
		}
		facts = append(facts, strings.Join(counts, ", "))
	}
	if len(facts) > 0 {
		db += " (" + strings.Join(facts, "; ") + ")"
	}
	fmt.Fprintf(w, "%sdatabase: %s\n", prefix, db)
	fmt.Fprintf(w, "%sfiles:    %s\n", prefix, backup.FilesWords(v))
}

// restoreRandom is the random source bridge restore hands RestoreWithOptions:
// nil, crypto/rand's, in production. A test fixes a restore's draws through it
// and puts it back in t.Cleanup; nothing else sets it.
var restoreRandom io.Reader

// restoreCmd is `bridge restore [--config X] [--yes] <snapshot-dir>`
// — copies files from `<snapshot-dir>` back into the live data dir.
//
// The bridge serve process MUST be stopped before restore. Running
// restore while serve is up would leave the SQLite WAL inconsistent
// with the new main file, surface as silent corruption on next
// query. The CLI requires the exclusive data-dir lock throughout restore,
// refusing even a lock error other than contention. `--yes` skips only the
// confirmation, never the lock or the admin probe.
//
// It ALSO refuses outright when a bridge is answering on the admin port.
// The prompt is advice; the lock and probe are enforcement, and they are not
// interchangeable on the one command that renames a database over a
// possibly-open one. (The docblock used to justify having no check with
// "no PID file today" — false since cmd/bridge/pidfile.go landed;
// writeServerPIDFile runs from runServe and doctor.go already reads
// <dataDir>/server.pid. The admin probe is the cheaper signal and the one
// every sibling CLI mutator already uses.)
//
// The snapshot's own side is locked too (backlog B826): once confirmed, the
// restore takes the lock of the folder that holds the snapshot, the one the
// bridge that keeps those snapshots verifies and prunes under, inside the
// data dir's and ahead of bridge.yaml's, waiting for a verification or prune
// there for verifyCmdLockWait, and is refused, nothing restored, while a
// check, a prune or another restore that did not end holds it, past that
// wait, where no lock can be had there, and where the snapshot is gone by then
// (sayRestoreRefusal). It is not held across the prompt, which would hold that
// bridge's checks up for as long as the prompt waits. Before the prompt the
// command looks at the snapshot, bounded as that admission is
// (backup.CheckRestoreSource): a folder that does not answer, and one a hold
// given up holds (another process's marker counted only while the folder's
// lock is held), are refused before anything is asked.
func restoreCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	autoYes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(autoYes, "y", *autoYes, "alias for --yes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "restore: missing snapshot directory argument")
		fmt.Fprintln(stderr, "Usage: bridge restore [--config bridge.yaml] [--yes] <snapshot-dir>")
		return 2
	}
	snapshotDir := backup.CleanSnapshotPath(fs.Arg(0))

	// The snapshot is looked at before anything else, bounded as the
	// restore's admission is (backup.CheckRestoreSource, backlog B826's second
	// round): a hold given up in this process, asked first of no filesystem,
	// then on a worker the folder it resolves into, another process's marker
	// there counted only while that folder's lock is held, and the manifest; a
	// folder that does not answer is refused within the bound, before any
	// prompt. The restore asks again, under the folder's lock, once confirmed.
	switch err := backup.CheckRestoreSource(ctx, snapshotDir, verifyCmdLockWait); {
	case errors.Is(err, backup.ErrNotASnapshot):
		fmt.Fprintf(stderr, "restore: %q is not a recognized snapshot directory (no readable manifest.json with a supported schema)\n", snapshotDir)
		return 2
	case err != nil:
		if !sayRestoreRefusal(stderr, err) {
			fmt.Fprintf(stderr, "restore: %v\n", err)
		}
		return 1
	}

	// The bytes the config is read from go to the restore (ConfigAsRead):
	// the targets below come from this read, its data dir and its TLS pair,
	// and the restore refuses, with nothing restored, a config another
	// writer changed since (while this command asks for confirmation, say),
	// once it has the config's turn (backlog B506).
	cfg, resolvedCfgPath, cfgRead, err := loadCLIConfigFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}

	// Hold the data-dir lock for the whole run, including the confirmation
	// wait. backup.Restore unlinks the live -wal and -shm and renames a
	// fresh bridge.db over the open one; a serve that starts at the prompt
	// is refused by the lock. The admin-port probe still runs when that
	// port is fixed, including while this command holds the lock: a serve
	// that continued after a lock error other than contention holds none.
	// An ephemeral admin port cannot be probed; the lock alone decides.
	releaseLock, fallback, refused := holdDataDirForMutation(resolvedCfgPath, cfg.DataDir, "restore", stderr)
	defer releaseLock()
	if refused {
		return 1
	}
	if fallback {
		fmt.Fprintln(stderr, "restore: the data directory's exclusive lock could not be taken; fix its permissions or use storage that supports locking, then restore again.")
		return 1
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, "restore", stderr) {
		return 1
	}

	if !*autoYes {
		fmt.Fprintf(stdout, "Restore from %s into %s?\n", snapshotDir, cfg.DataDir)
		fmt.Fprintln(stdout, "WARNING: stop the bridge service first. Restoring while `bridge serve` is running can corrupt the live database.")
		// F14: the PROMPT goes to stderr, the description stays on stdout.
		// `bridge restore > log` is the natural way to capture a paper trail
		// for an invasive operation, and it used to hide the question in the
		// redirected stream and block with no visible cue. update.go asserts
		// this as the repo convention and names `cert rotate` and `restore`
		// as following it; neither did.
		fmt.Fprint(stderr, "Type 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	// The probe's second look, on the same rule as the first. On the
	// fallback a bridge can have started while the prompt waited. With
	// the lock held, an unlocked serve can have bound its admin port in
	// that window.
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, "restore", stderr) {
		return 1
	}

	// The restore takes the lock of the folder that holds the snapshot first,
	// waiting for a verification or prune there as bridge backup --verify
	// waits (backlog B826), and reads the snapshot again under it, since a
	// prune can have removed it while the prompt waited. ctx stops that wait
	// and nothing after it.
	targets := buildRestoreTargets(cfg, resolvedCfgPath)
	opts := backup.RestoreOptions{
		Random: restoreRandom, Warn: func(s string) { fmt.Fprintln(stdout, s) }, ConfigAsRead: cfgRead,
		Context: ctx, LockWait: verifyCmdLockWait,
	}
	if err := backup.RestoreWithOptions(snapshotDir, targets, opts); err != nil {
		if !sayRestoreRefusal(stderr, err) {
			fmt.Fprintf(stderr, "restore: %v\n", err)
		}
		return 1
	}
	fmt.Fprintln(stdout, "Restore complete. Start the bridge service.")
	return 0
}

// sayRestoreRefusal says on stderr why a restore was refused at the lock of
// the folder that holds its snapshot (backup.RestoreWithOptions, backlog
// B826), in the words verifyCmd gives each kind, and reports whether err was
// such a refusal: a check that did not end still holding that folder,
// another verification or prune holding it for all of the restore's wait, a
// folder where no lock can be had, and a snapshot that is no longer there.
// Each exits 1, as verifyCmd's do; the error says nothing was restored.
func sayRestoreRefusal(stderr io.Writer, err error) bool {
	switch {
	case errors.Is(err, backup.ErrCheckStillRunning):
		fmt.Fprintf(stderr, "restore: %v. %s\n", err,
			stillRunningEnd(err, "restart the bridge that keeps those snapshots, or stop the bridge backup --verify that runs it"))
	case errors.Is(err, backup.ErrBackupsBusy):
		fmt.Fprintf(stderr, "restore: %v. It is a serving bridge's, or another command's: run this again once it ends.\n", err)
	case errors.Is(err, backup.ErrBackupsUnlocked):
		fmt.Fprintf(stderr, "restore: %v. With no lock nothing would keep a prune from removing the snapshot while it is restored: copy the snapshot, its whole folder, into a folder this user can write on a filesystem that grants file locks (a local disk), and restore from the copy.\n", err)
	case errors.Is(err, backup.ErrSnapshotGone):
		fmt.Fprintf(stderr, "restore: %v. It was there when this command read it, and a prune may have removed it since: choose another snapshot.\n", err)
	default:
		return false
	}
	return true
}

// stillRunningEnd says what ends the hold given up behind err, a refusal for
// one (backup.ErrCheckStillRunning), for the operator: a restore's is ended by
// the bridge restore command that runs it (backup.HeldByRestore, backlog
// B826's second round), which no restart of a bridge ends; a verification's or
// a prune's by others, the words of the command that answers.
func stillRunningEnd(err error, others string) string {
	if backup.HeldByRestore(err) {
		return "The end of the bridge restore that runs it ends it (bridge doctor's backup-check line names its snapshot): stop that command, then run this again."
	}
	return "The end of the process that runs it ends it (bridge doctor's backup-check line names its snapshot): " + others + ", then run this again."
}

// buildBackupSources resolves the live state-file paths from a
// loaded config. Used by the CLI and by the periodic ticker in
// serveCmd so both paths land identical bundles.
func buildBackupSources(cfg *config.Config, configPath string) backup.Sources {
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	return backup.Sources{
		DataDir:      cfg.DataDir,
		ManifestDB:   manifest.DefaultDBPath(cfg.DataDir),
		TokensJSON:   filepath.Join(cfg.DataDir, tokensFileName),
		ServerCert:   certPath,
		ServerKey:    keyPath,
		BridgeYAML:   configPath,
		AdminAuth:    filepath.Join(cfg.DataDir, adminauth.FileName),
		AtlasHarvest: harvestStatePath(cfg.DataDir),
		PlayerUUID:   filepath.Join(cfg.DataDir, config.PlayerUUIDFile),
		PlayerDSD:    filepath.Join(cfg.DataDir, dsdstate.FileName),
	}
}

// startupBackupSkipThreshold is the minimum age of the most-recent
// existing snapshot before a `bridge serve` cold-start writes a fresh
// startup snapshot. An operator restarting the bridge 10x/day for
// debugging or config tweaks shouldn't pay a 10-snapshot/day cost; the
// scheduled-tick + manual `bridge backup` paths still cover the
// "haven't booted in a week" case. Tied to the default scheduled
// interval (24h) so a startup that lands inside a freshly-completed
// scheduled snapshot's window is treated as a no-op.
const startupBackupSkipThreshold = 24 * time.Hour

// runBackupTicker is the periodic snapshot loop wired into serveCmd.
// With the cadence on, its boot pass follows the startup rule
// (decideStartupPass: a snapshot on startup unless one from the last day is
// verified, or the newest of them still owes its check, which the boot
// finishes in its place), so an operator who just finished `bridge init` has
// a baseline before any pairings, but a restart loop doesn't cost one
// snapshot per boot; and then a snapshot on the configured interval. Each
// snapshot takes the one path every snapshot takes (backup.SnapshotAndVerify:
// the capture, its verification, the prune after it whatever the verdict),
// and none is taken while the backups folder grants no lock and its newest
// snapshot owes its check (captureHeldBack). With the cadence off at boot it
// takes no snapshot and prunes nothing (backlog B209). All errors are logged
// to stderr — never fatal, since a failed backup must never take down the
// running bridge.
// runBackupTicker takes its cadence AND its retention as providers,
// re-read per use rather than captured once.
//
// `keep` is read at PRUNE time, which is the only moment it means
// anything: retention is not a lifecycle, it is an argument to a function
// that runs on a schedule, and capturing it was never buying anything.
//
// `interval` is read before the startup snapshot and before each wait. A
// provider returning <= 0 parks the loop instead of ending it — the
// operator disabled the cadence, and re-enabling it must not require a
// restart. That is why the goroutine is now started UNCONDITIONALLY by the
// caller: the old shape only spawned it when hours > 0, so 0 → N had no
// loop alive to notice the change.
//
// `rearm` wakes the wait so a changed cadence is re-read immediately
// rather than after the old interval — up to a day, on the 24 h default.
func runBackupTicker(ctx context.Context, src backup.Sources, keep func() int, interval func() time.Duration, rearm <-chan struct{}, stdout, stderr io.Writer, status *sweepStatus[struct{}]) {
	backupsRoot := filepath.Join(src.DataDir, backup.BackupsDirName)
	// report says what a pass's kept snapshot's verification and the prune
	// after it did.
	report := func(triggered string, res backup.Result) {
		reportTickerVerdict(ctx, stdout, stderr, triggered, res)
		// Warning, not a failure — see the same handling in
		// backupCmd. Pre-split this early-returned and suppressed
		// the "pruned N" line on every tick, forever.
		if problem := ctxerr.WithoutCancellation(ctx, res.Prune.ReapErr); problem != nil {
			fmt.Fprintf(stderr, "backup (%s): orphan sweep reported a problem (prune unaffected): %v\n", triggered, problem)
		}
		// A prune the cancel stopped falls through to the count: the
		// snapshots it removed before stopping are gone, and saying so is
		// a fact about this pass
		// (TestABackupCancelledMidPruneReportsWhatItDeleted).
		if failure := ctxerr.WithoutCancellation(ctx, res.PruneErr); failure != nil {
			fmt.Fprintf(stderr, "backup (%s): prune failed: %v\n", triggered, failure)
			return
		}
		switch {
		case res.PruneSkipped && ctx.Err() == nil:
			fmt.Fprintf(stdout, "backup (%s): prune skipped: %s\n", triggered, pruneSkipWords(res.VerifyErr))
		case res.Prune.Deleted > 0:
			fmt.Fprintf(stdout, "backup (%s): pruned %d older snapshot(s)\n", triggered, res.Prune.Deleted)
		}
	}
	doSnapshot := func(triggered string) {
		// Where no lock is granted on the backups folder nothing there is
		// verified or pruned, so a capture each pass would grow it without
		// end: while its newest snapshot still owes its check, no other is
		// taken (backlog B653's second round), nor while a check that did not
		// end holds the folder (its third round), and bridge doctor says why.
		if why, held := captureHeldBack(ctx, backupsRoot); held {
			if why != "" {
				fmt.Fprintf(stderr, "backup (%s): no snapshot taken: %s\n", triggered, why)
			}
			return
		}
		status.sweepStarted()
		// The one path every snapshot takes (backlog B653): the capture,
		// its verification, and the prune after it whatever the verdict,
		// all on the ticker's ctx, which shutdown cancels: a verification
		// it stops writes INCOMPLETE within backup.StopWriteBound and
		// returns, so serve's wait for this goroutine (backupShutdownWait)
		// is never held up by it. PruneContext's rule, and unconditionally:
		// see backupCmd for both rationales.
		res := backup.SnapshotAndVerify(ctx, src, backup.SnapshotOptions{
			Keep: keep,
			OnKept: func(dir string) {
				fmt.Fprintf(stdout, "backup (%s): wrote %s\n", triggered, dir)
			},
		})
		if !res.Kept() {
			// A snapshot the shutdown stopped is not reported: Snapshot has
			// removed its partial directory, so the backups directory is as
			// this pass found it, and the next pass decides from there. nil
			// still goes to the run state, which means "no new counts" for a
			// failed pass and a stopped one alike, since `running` has to clear.
			if failure := ctxerr.WithoutCancellation(ctx, res.CaptureErr); failure != nil {
				fmt.Fprintf(stderr, "backup (%s): snapshot failed: %v\n", triggered, failure)
			}
			status.sweepFinished(nil)
			return
		}
		defer status.sweepFinished(&struct{}{})
		report(triggered, res)
	}
	// verifyInPlace finishes the check the newest snapshot owes in place of
	// a new snapshot (startupVerifyInPlace), claimed under the backups
	// folder's lock so no prune removes it before its check, and reports
	// whether that check found its database FAILED, regardless of recording,
	// or found a FAILED component while its vetoed record could not be
	// re-recorded on this platform. A leftover veto alone is no reason to
	// capture again: failed veto removal would add a snapshot at every start.
	verifyInPlace := func(name string) bool {
		dir := filepath.Join(backupsRoot, name)
		c, err := backup.Claim(ctx, dir, backupsRoot)
		if err != nil {
			if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
				fmt.Fprintf(stderr, "backup (startup): %s owes its check, which was not begun: %v\n", dir, failure)
			}
			return false
		}
		status.sweepStarted()
		defer status.sweepFinished(&struct{}{})
		fmt.Fprintf(stdout, "backup (startup): %s owes its check: verifying it in place of a new snapshot\n", dir)
		res := backup.FinishCapture(ctx, c, keep, backup.VerifyOptions{BackupsRoot: backupsRoot})
		report("startup", res)
		return res.Verify.Found.DatabaseState == backup.StateFailed ||
			(errors.Is(res.VerifyErr, backup.ErrRecheckNotRecorded) &&
				(res.Verify.Found.DatabaseState == backup.StateFailed || res.Verify.Found.FilesState == backup.StateFailed) &&
				res.Verify.RecordRead && (slices.Contains(res.Verify.Record.DatabaseReasons, backup.ReasonUnsynced) ||
				slices.Contains(res.Verify.Record.FilesReasons, backup.ReasonUnsynced)))
	}

	// The startup snapshot and the scheduled ones differ, so the loop's
	// single `sweep` closure takes the loop's word for which pass is the
	// boot one. Never "the first call": with the cadence off at boot the
	// loop takes no boot pass, and the first call is a scheduled pass after
	// a 0 → N rearm, which the startup rule below would have skipped.
	//
	// The boot pass follows the startup rule (decideStartupPass). A listing
	// that fails is non-fatal (rare; it surfaces a misconfig or disk problem
	// the user should see) and falls through to writing the snapshot anyway.
	sweep := func(boot bool) {
		if !boot {
			doSnapshot("scheduled")
			return
		}
		read, answered := readBounded(ctx, func() statusRead {
			st, err := backup.ReadStatus(ctx, backupsRoot)
			return statusRead{st, err}
		})
		if !answered {
			return
		}
		st, err := read.st, read.err
		if err != nil {
			fmt.Fprintf(stderr, "backup (startup): list existing snapshots: %v (writing anyway)\n", err)
			doSnapshot("startup")
			return
		}
		pass := decideStartupPass(st, time.Now().UTC(), startupBackupSkipThreshold)
		switch pass.action {
		case startupSkipVerified:
			fmt.Fprintf(stdout, "backup (startup): recent verified snapshot at %s — skipping (threshold %s)\n",
				pass.snapshot.CreatedAt.Format(time.RFC3339), startupBackupSkipThreshold)
		case startupSkipOwned:
			fmt.Fprintf(stdout, "backup (startup): the newest snapshot, %s, has its check queued or under way — skipping\n",
				pass.snapshot.DirName)
		case startupVerifyInPlace:
			if verifyInPlace(pass.snapshot.DirName) {
				doSnapshot("startup")
			}
		default:
			doSnapshot("startup")
		}
	}

	// The SHARED loop, not a second hand-rolled ticker.
	//
	// It already carries every property this needs — re-read the interval
	// before the boot pass and each wait, park (never exit) when it is
	// non-positive, clear the scheduled-next while dormant, and re-arm
	// without doing the work — and a private copy here would be a second
	// place to get the 0 -> N transition wrong. Settle delay 0: the startup
	// snapshot is the point of running at boot, and deferring it would
	// change what an operator gets from a short-lived process. No nudge:
	// there is no "back up now" button on this path (the CLI's `bridge
	// backup` runs its own snapshot).
	runSweepLoop(ctx, status, 0, interval, nil, rearm, sweep)
}

// reportTickerVerdict says what the pass's kept snapshot's verification
// found: on stdout when both its verdicts are VERIFIED (or the install had
// no database to capture and its files are), on stderr otherwise, and
// nothing at shutdown for a cancellation-only error whose found verdict is
// unknown, passing or CancelledOnly, a give-up the stop made alone being
// that stop (backup.AsTheStop: the outcome is one whichever of the two
// bounds the stop arms fires first, backlog B895). Without an error, a
// stopped context silences only a CancelledOnly record. Known aborts and
// deadlines are reported, a step given up at a bound of its own among them.
// A verification given up is said by how far its write went and what its
// record reads as read back, and that its step still holds the snapshots; a
// kept snapshot whose verification answered an error once its check had run,
// by what its check found, how far its write went (keptWords' rule, from the
// write's outcome alone) and what its record reads; and in each case a record
// that was not read back is said so, and why (readWords), never printed
// (backlog B653's sixth round).
func reportTickerVerdict(ctx context.Context, stdout, stderr io.Writer, triggered string, res backup.Result) {
	if res.VerifyErr != nil {
		failure := ctxerr.WithoutCancellation(ctx, backup.AsTheStop(ctx, res.VerifyErr))
		found, rec, read := res.Verify.Found, res.Verify.Record, res.Verify.RecordRead
		if failure == nil {
			if found.DatabaseState == "" || found.Passes() || found.CancelledOnly() {
				return
			}
			// The verdict still needs reporting. Keep the stop's own error
			// beside it without changing the error shared with other callers.
			failure = res.VerifyErr
		}
		switch {
		case res.Verify.NotStarted(failure):
			fmt.Fprintf(stderr, "backup (%s): the verification did not start for %s: %v\n", triggered, res.Dir, failure)
		case errors.Is(failure, backup.ErrCheckGivenUp):
			reads := recordWords(res.Verify)
			if read {
				reads = fmt.Sprintf("%s database %s, files %s", reads,
					backup.StateWords(rec.DatabaseState, rec.DatabaseReasons), backup.FilesWords(rec))
			}
			fmt.Fprintf(stderr, "backup (%s): the verification of %s was given up, and its step still holds the snapshots until it ends; %s: %v\n",
				triggered, res.Dir, reads, failure)
		case errors.Is(failure, backup.ErrRecheckRefused) && read:
			fmt.Fprintf(stderr, "backup (%s): %s was not checked, and its record, from an earlier check, reads database %s, files %s: %v\n", triggered, res.Dir,
				backup.StateWords(rec.DatabaseState, rec.DatabaseReasons), backup.FilesWords(rec), failure)
		case errors.Is(failure, backup.ErrRecheckRefused):
			fmt.Fprintf(stderr, "backup (%s): %s was not checked, and %s: %v\n", triggered, res.Dir, readWords(res.Verify), failure)
		case found.DatabaseState != "":
			fmt.Fprintf(stderr, "backup (%s): %s was kept, and its check found database %s, files %s, %s: %v\n", triggered, res.Dir,
				backup.StateWords(found.DatabaseState, found.DatabaseReasons), backup.FilesWords(found), tickerKeptWords(res.Verify), failure)
		default:
			fmt.Fprintf(stderr, "backup (%s): %s was kept, and %s: %v\n", triggered, res.Dir, keptWords(res.Verify), failure)
		}
		return
	}
	v := res.Verify.Record
	if v.Passes() {
		fmt.Fprintf(stdout, "backup (%s): verified %s: database %s, files %s\n", triggered, res.Dir,
			backup.StateWords(v.DatabaseState, v.DatabaseReasons), backup.FilesWords(v))
		return
	}
	if ctx.Err() != nil && v.CancelledOnly() {
		return
	}
	fmt.Fprintf(stderr, "backup (%s): %s was kept, and it is not a verified recovery point: database %s, files %s\n",
		triggered, res.Dir, backup.StateWords(v.DatabaseState, v.DatabaseReasons), backup.FilesWords(v))
}

// tickerKeptWords is keptWords in the ticker's one line, after what the check
// found: how far its write went, from the write's outcome alone, then what
// the record reads as read back, or why it was not read back.
func tickerKeptWords(v backup.VerifyResult) string {
	w := "which was not recorded"
	switch v.Write {
	case backup.WriteNotDurable:
		w = "which was written and is not known to survive a crash"
	case backup.WriteRecorded, backup.WriteUnchanged, backup.WriteUnfinished, backup.WriteUnknown:
		w = "and " + writeWords(v.Write)
	}
	if !v.RecordRead {
		return w + "; " + readWords(v)
	}
	return fmt.Sprintf("%s; its record reads database %s, files %s", w,
		backup.StateWords(v.Record.DatabaseState, v.Record.DatabaseReasons), backup.FilesWords(v.Record))
}

// pruneSkipWords says why a kept snapshot's prune did not run, from its
// verification's error.
func pruneSkipWords(verifyErr error) string {
	switch {
	case errors.Is(verifyErr, backup.ErrBackupsUnlocked):
		return "no lock is granted on the backups folder, so nothing there is verified or pruned (bridge doctor's backup-lock line)"
	case backup.HeldByRestore(verifyErr):
		return "a restore that did not end holds the snapshots until it ends; the first snapshot after it prunes (bridge doctor's backup-check line)"
	case errors.Is(verifyErr, backup.ErrCheckGivenUp), errors.Is(verifyErr, backup.ErrCheckStillRunning):
		return "a check that did not end holds the snapshots until it ends; the first snapshot after it prunes (bridge doctor's backup-check line)"
	case errors.Is(verifyErr, backup.ErrRecheckNotRecorded), errors.Is(verifyErr, backup.ErrRecheckRefused):
		return "the check of a snapshot whose record holds a verified verdict recorded nothing, so no prune trusts that record; the next snapshot prunes"
	}
	return "another verification or prune of the snapshots held their lock; the next snapshot prunes"
}

// startupAction is what the ticker's boot pass does (decideStartupPass).
type startupAction int

const (
	// startupCapture takes a snapshot.
	startupCapture startupAction = iota
	// startupSkipVerified takes none: a snapshot from the last day has a
	// database VERIFIED.
	startupSkipVerified
	// startupSkipOwned takes none: the newest snapshot from the last day
	// has its check queued or under way, its owner live.
	startupSkipOwned
	// startupVerifyInPlace takes none, and finishes the check the newest
	// snapshot from the last day owes in its place.
	startupVerifyInPlace
)

// String names the action, for a log or a test's failure.
func (a startupAction) String() string {
	switch a {
	case startupCapture:
		return "capture"
	case startupSkipVerified:
		return "skip for a recent verified snapshot"
	case startupSkipOwned:
		return "skip for a check queued or under way"
	case startupVerifyInPlace:
		return "verify the newest snapshot in place"
	}
	return fmt.Sprintf("startupAction(%d)", int(a))
}

// startupPass is the boot pass's decision, and the snapshot it is about.
type startupPass struct {
	action   startupAction
	snapshot backup.SnapshotStatus
}

// decideStartupPass is the startup rule (backlog B653 and its second round),
// over the snapshots as a reader reads them (backup.ReadStatus) at now, the
// last day being threshold (startupBackupSkipThreshold). Where a snapshot
// from the last day has a database VERIFIED, the boot takes none. Otherwise,
// where the newest from the last day still owes its check (owesCheck: its
// database PENDING, or INCOMPLETE, a check that did not end or a capture whose
// process died before it), the boot takes none either: it skips where a live
// owner holds that snapshot, its check queued or under way, in this process
// or another, and otherwise finishes the check in place, taking a snapshot
// after it where that check finds the database FAILED, or cannot re-record a
// vetoed record on a platform without a directory barrier. Otherwise, no
// snapshot from the last day, or the newest of them FAILED, written before
// verification or holding a record this bridge cannot read, the boot takes
// one. So a restart loop whose boots die before their verification ends
// keeps one snapshot, not one per boot, and the first boot that lives long
// enough verifies it.
func decideStartupPass(st backup.Status, now time.Time, threshold time.Duration) startupPass {
	recent := func(sn backup.SnapshotStatus) bool {
		return !sn.CreatedAt.IsZero() && now.Sub(sn.CreatedAt) < threshold
	}
	// Newest first: past the first one older than the threshold, every one
	// is.
	for _, sn := range st.Snapshots {
		if !recent(sn) {
			break
		}
		if sn.Verdict.DatabaseState == backup.StateVerified {
			return startupPass{action: startupSkipVerified, snapshot: sn}
		}
	}
	newest, found := st.Newest()
	switch {
	case !found || !recent(newest) || !owesCheck(newest):
		return startupPass{action: startupCapture}
	case newest.Owned:
		return startupPass{action: startupSkipOwned, snapshot: newest}
	}
	return startupPass{action: startupVerifyInPlace, snapshot: newest}
}

// owesCheck reports whether sn's check is still to be finished: a snapshot
// this bridge verifies whose database a reader reads PENDING or INCOMPLETE.
func owesCheck(sn backup.SnapshotStatus) bool {
	if sn.Legacy {
		return false
	}
	switch sn.Verdict.DatabaseState {
	case backup.StatePending, backup.StateIncomplete:
		return true
	}
	return false
}

// statusRead is a bounded read of the backups folder's status.
type statusRead struct {
	st  backup.Status
	err error
}

// readBounded runs read, a read of the backups folder that can wait on a
// filesystem (a hung mount; a named pipe where a manifest goes), on a
// goroutine of its own, and waits for it until it answers, or, once ctx has
// ended, for backup.StopWriteBound more: answered false then, the read left to
// end on its own, so a stop of the ticker never waits on it longer (backlog
// B653's third round). The read only reads, and holds nothing past its
// return.
func readBounded[T any](ctx context.Context, read func() T) (v T, answered bool) {
	out := make(chan T, 1)
	go func() { out <- read() }()
	select {
	case v = <-out:
		return v, true
	case <-ctx.Done():
	}
	t := time.NewTimer(backup.StopWriteBound)
	defer t.Stop()
	select {
	case v = <-out:
		return v, true
	case <-t.C:
		return v, false
	}
}

// captureHeldBack reports whether the ticker takes no snapshot this pass,
// and why, in its log line's words: a check that did not end holds the
// backups folder still (backup.StuckCheckOf), so nothing there is verified or
// pruned until it ends; or no lock is granted on the folder
// (backup.ProbeBackupsLock), so nothing there is verified or pruned, and its
// newest snapshot still owes its check. Each read is bounded by the stop
// (readBounded); one the stop came first to holds the pass back, why "".
func captureHeldBack(ctx context.Context, backupsRoot string) (why string, held bool) {
	type stuckRead struct {
		check backup.StuckCheck
		held  bool
	}
	stuck, answered := readBounded(ctx, func() stuckRead {
		c, held := backup.StuckCheckOf(backupsRoot)
		return stuckRead{c, held}
	})
	switch {
	case !answered:
		return "", true
	case stuck.held:
		what := "a check of " + stuck.check.Snapshot
		switch {
		case stuck.check.Restoring():
			what = "a restore of " + stuck.check.Snapshot
		case stuck.check.Pruning():
			what = "a prune of the snapshots"
		}
		return fmt.Sprintf("%s that did not end still holds the backups folder, so nothing there is verified or pruned; none is taken until it ends (bridge doctor's backup-check line)",
			what), true
	}
	lockErr, answered := readBounded(ctx, func() error { return backup.ProbeBackupsLock(backupsRoot) })
	switch {
	case !answered:
		return "", true
	case !errors.Is(lockErr, backup.ErrBackupsUnlocked):
		return "", false
	}
	read, answered := readBounded(ctx, func() statusRead {
		st, err := backup.ReadStatus(ctx, backupsRoot)
		return statusRead{st, err}
	})
	switch {
	case !answered:
		return "", true
	case read.err != nil:
		return "", false
	}
	if newest, found := read.st.Newest(); found && owesCheck(newest) {
		return fmt.Sprintf("no lock is granted on the backups folder, so its newest snapshot, %s, has not been verified and nothing there is pruned; none is taken until a lock is (bridge doctor's backup-lock line)",
			newest.DirName), true
	}
	return "", false
}

// buildRestoreTargets is the inverse of buildBackupSources — points
// at the same live paths so a restore lands files exactly where
// serve / pair / scan expect to find them.
func buildRestoreTargets(cfg *config.Config, configPath string) backup.Targets {
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	return backup.Targets{
		ManifestDB:   manifest.DefaultDBPath(cfg.DataDir),
		TokensJSON:   filepath.Join(cfg.DataDir, tokensFileName),
		ServerCert:   certPath,
		ServerKey:    keyPath,
		BridgeYAML:   configPath,
		AdminAuth:    filepath.Join(cfg.DataDir, adminauth.FileName),
		AtlasHarvest: harvestStatePath(cfg.DataDir),
		PlayerUUID:   filepath.Join(cfg.DataDir, config.PlayerUUIDFile),
		PlayerDSD:    filepath.Join(cfg.DataDir, dsdstate.FileName),
	}
}
