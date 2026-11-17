package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// restoreHandBackSection is the section of docs/backup.md that says how to
// put a database-only restore's originals back by hand.
const restoreHandBackSection = `"Put the originals back by hand" in docs/backup.md`

// restoreRemedy is the one text that says how to go on from an unfinished
// restore of the library database (backlog B653 PR 2b1 §4.9): the command
// that finishes it, with the config this process read, where its originals
// are, absolute (the folder its journal names, read as the recovery reads
// it, else the folder of every run's originals), and the section of
// docs/backup.md that says how to put them back by hand. Serve's refusal of
// its store, the full restore's refusal, every stop of the bounded path and
// the doctor say it.
func restoreRemedy(configPath, dataDir string) string {
	if configPath == "" {
		configPath = "<bridge.yaml>"
	}
	return fmt.Sprintf("Finish it with `bridge restore --database-only --config %s`, run by the bridge version that began it (no snapshot argument): "+
		"it completes a restore whose database is installed, and puts any other back. Its original database files are in %s; "+
		"to put them back by hand, see %s.", restoreConfigArg(configPath), backup.RestoreRemedyArchive(dataDir), restoreHandBackSection)
}

// restoreConfigArg is a config path as a command line takes it: as it is
// where it holds nothing a shell reads, quoted for this platform's shell
// otherwise.
func restoreConfigArg(path string) string {
	plain := path != ""
	for _, r := range path {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(`/._-:\`, r)) {
			plain = false
			break
		}
	}
	switch {
	case plain, path == "<bridge.yaml>":
		return path
	case runtime.GOOS == "windows":
		return quoteCmd(path)
	}
	return quotePosix(path)
}

// databaseOnlyRestoreCmd is `bridge restore --database-only <snapshot-dir>`
// once its config, the data dir's lock, the admin probe, the store's
// lifetime lock and the journal have been taken and asked, and the snapshot
// looked at (restoreCmd): the look's refusals (a schema this bridge does not
// take, no database with a baseline, a database not VERIFIED as every reader
// reads it), the free space the copy and its preparation need, the
// description on stdout and the question on stderr, the probe again, and the
// restore (backup.RestoreDatabase).
func databaseOnlyRestoreCmd(ctx context.Context, cfg *config.Config, cfgPath string, cfgRead []byte, snapshotDir string,
	look backup.SourceLook, full, autoYes, fallback bool, stdin io.Reader, stdout, stderr io.Writer) int {
	if why := backup.DatabaseRestoreRefusal(look); why != "" {
		fmt.Fprintf(stderr, "restore: %s: %s; nothing was restored\n", snapshotDir, why)
		return 1
	}
	var base int64
	for _, b := range look.Manifest.Baselines {
		if b.Name == backup.ManifestDBFileName {
			base = b.Size
		}
	}
	if avail, err := transcode.AvailableDiskSpace(cfg.DataDir); err == nil && avail < 2*base {
		fmt.Fprintf(stderr, "restore: the data directory's volume has %d bytes free, and the restore needs twice the snapshot's database (%d bytes) to stage and prepare it; nothing was restored\n",
			avail, 2*base)
		return 1
	}
	if !autoYes {
		describeDatabaseRestore(stdout, snapshotDir, cfg.DataDir, look)
		fmt.Fprint(stderr, "Type 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, "restore", stderr) {
		return 1
	}
	res, err := backup.RestoreDatabase(snapshotDir, cfg.DataDir, backup.DatabaseOptions{
		Context: ctx, LockWait: verifyCmdLockWait, Full: full, ConfigPath: cfgPath, ConfigAsRead: cfgRead,
		Warn: func(s string) { fmt.Fprintln(stdout, s) }, HarvestState: harvestStatePath(cfg.DataDir),
	})
	if err != nil {
		if errors.Is(err, backup.ErrRestoreStopped) {
			fmt.Fprintf(stderr, "restore: %v\n", err)
			fmt.Fprintln(stderr, restoreRemedy(cfgPath, cfg.DataDir))
			return 1
		}
		if !sayRestoreRefusal(stderr, err) {
			fmt.Fprintf(stderr, "restore: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "Restored the library database from %s.\n", snapshotDir)
	fmt.Fprintf(stdout, "The database's files it replaced are kept in %s:\n", res.Archive)
	sayOriginals(stdout, res.Originals)
	fmt.Fprintf(stdout, "files:    %s (not restored)\n", backup.FilesWords(res.Verdict))
	fmt.Fprintln(stdout, "Start the bridge service: bridge start")
	return 0
}

// describeDatabaseRestore is the database-only restore's description before
// its question: the snapshot, its record as read before the prompt (the
// restore reads it again once answered), what it replaces and keeps, and
// what it restores nothing of.
func describeDatabaseRestore(w io.Writer, snapshotDir, dataDir string, look backup.SourceLook) {
	m := look.Manifest
	fmt.Fprintf(w, "Restore the library database alone from %s into %s?\n", snapshotDir, dataDir)
	fmt.Fprintf(w, "snapshot: taken %s by bridge %s\n", m.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"), m.BridgeVersion)
	fmt.Fprintln(w, "Its record, as read before this question (the restore reads it again once you answer):")
	describeVerdictLed(w, "  ", look.Verdict)
	var replaced []string
	db := manifest.DefaultDBPath(dataDir)
	for _, p := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		if fi, err := os.Lstat(p); err == nil {
			replaced = append(replaced, fmt.Sprintf("%s (%d bytes)", filepath.Base(p), fi.Size()))
		}
	}
	if len(replaced) == 0 {
		replaced = append(replaced, "nothing: no database stands")
	}
	fmt.Fprintf(w, "It replaces %s, moving each into %s, where it is kept.\n", strings.Join(replaced, ", "),
		filepath.Join(dataDir, backup.OriginalsDirName)+string(filepath.Separator))
	fmt.Fprintln(w, "It restores nothing else: the tokens, the TLS pair, bridge.yaml, the console account, the player's files and the library stay as they are, "+
		"and the harvest's sync position is rewound.")
	fmt.Fprintln(w, "WARNING: stop the bridge service first.")
}

// sayOriginals says each original the restore kept, and its size.
func sayOriginals(w io.Writer, originals []backup.FileFact) {
	for _, o := range originals {
		if o.Present {
			fmt.Fprintf(w, "  %s (%d bytes)\n", o.Name, o.Size)
		}
	}
}

// finishDatabaseRestoreCmd is the bounded path (backlog B653 PR 2b1 §4.5):
// `bridge restore --database-only` with a journal standing and nothing else,
// under the data dir's lock and the store's lifetime lock, which restoreCmd
// holds. It reads the journal and what the data dir holds, refusing a
// journal that does not read and one another bridge version began, and
// asks, unless --yes, naming the snapshot, the phase, what it found and
// what it will do; then probes the admin port again and completes the
// restore (an `installed` one) or puts its originals back (any other). It
// reads no snapshot and opens no database.
func finishDatabaseRestoreCmd(ctx context.Context, cfg *config.Config, cfgPath string, autoYes, fallback bool, stdin io.Reader, stdout, stderr io.Writer) int {
	plan, err := backup.PlanFinishDatabaseRestore(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "restore: %v\n", err)
		if !errors.Is(err, backup.ErrRestoreByAnotherVersion) {
			fmt.Fprintf(stderr, "Nothing was moved. Its original database files are in %s; to put them back by hand, see %s.\n",
				backup.RestoreRemedyArchive(cfg.DataDir), restoreHandBackSection)
		}
		return 1
	}
	j := plan.Journal
	if !autoYes {
		fmt.Fprintf(stdout, "An unfinished restore of the library database stands: of %s, begun %s, its journal at phase %s.\n",
			j.Snapshot.Given, j.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"), j.Phase)
		for _, line := range plan.Where {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
		if plan.Complete {
			fmt.Fprintln(stdout, "The restored database is installed: this completes the restore (the harvest's sync position rewound, the journal removed).")
		} else {
			fmt.Fprintln(stdout, "This puts the original database files back where they were and removes the journal; nothing is restored.")
		}
		fmt.Fprint(stderr, "Type 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, "restore", stderr) {
		return 1
	}
	err = backup.FinishDatabaseRestore(cfg.DataDir, plan, backup.FinishOptions{
		Warn: func(s string) { fmt.Fprintln(stdout, s) }, HarvestState: harvestStatePath(cfg.DataDir),
	})
	if err != nil {
		fmt.Fprintf(stderr, "restore: %v\n", err)
		fmt.Fprintln(stderr, restoreRemedy(cfgPath, cfg.DataDir))
		return 1
	}
	if plan.Complete {
		fmt.Fprintf(stdout, "Completed the restore of the library database from %s.\n", j.Snapshot.Given)
		fmt.Fprintf(stdout, "The database's files it replaced are kept in %s:\n", plan.Archive)
		sayOriginals(stdout, j.Originals)
	} else {
		fmt.Fprintln(stdout, "Put the original database files back; nothing was restored:")
		sayOriginals(stdout, j.Originals)
	}
	fmt.Fprintln(stdout, "Start the bridge service: bridge start")
	return 0
}
