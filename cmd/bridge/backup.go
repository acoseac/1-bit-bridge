package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// backupCmd is `bridge backup [--config X] [--keep N]` — captures a
// timestamped snapshot of the bridge state under `<dataDir>/backups/`
// and optionally prunes older snapshots to retain at most N.
//
// Safe to run while `bridge serve` is up: SQLite's `VACUUM INTO` on
// a WAL-mode database produces an atomic clean copy without locking
// out the running process. The other captured files (tokens.json,
// cert/key, config) are read normally — a write race with serve is
// harmless for these because they're mutated rarely (token mint /
// revoke) and atomically (temp + rename in the auth store).
func backupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	keep := fs.Int("keep", 7, "number of snapshots to retain (0 disables prune)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, resolvedCfgPath, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}

	src := buildBackupSources(cfg, resolvedCfgPath)
	// CLI runs to completion in a short-lived process; a non-context
	// signal handler isn't wired here. context.Background() lets the
	// underlying SQLite VACUUM run unconstrained — typical bundle
	// build is well under a second.
	dst, err := backup.Snapshot(context.Background(), src)
	if err != nil {
		fmt.Fprintf(stderr, "snapshot: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Snapshot written: %s\n", dst)
	fmt.Fprintln(stdout, backup.SensitivityNotice)

	// Unconditional, even at --keep 0. That disables the KEEP-POLICY, not
	// the crash-orphan sweep: an orphan is never a snapshot worth
	// retaining, it carries a near-full DB copy, and nothing else in the
	// backup package can ever reclaim it. Guarding this call on keep > 0
	// meant "retention off" also silently meant "orphans accumulate
	// forever, and their warnings are never seen".
	backupsRoot := filepath.Join(cfg.DataDir, backup.BackupsDirName)
	res, err := backup.Prune(backupsRoot, *keep)
	// The orphan sweep's outcome is a WARNING, never the command's
	// exit status: a directory it can't classify (an unreadable
	// manifest it deliberately refuses to delete) is permanent, so
	// treating it as failure made every subsequent `bridge backup`
	// exit 1 even though the snapshot and the prune both worked.
	if res.ReapErr != nil {
		fmt.Fprintf(stderr, "prune: orphan sweep reported a problem (snapshot and prune are unaffected): %v\n", res.ReapErr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "prune: %v\n", err)
		return 1
	}
	if res.Deleted > 0 {
		fmt.Fprintf(stdout, "Pruned %d older snapshot(s) (kept %d most recent).\n", res.Deleted, *keep)
	}
	return 0
}

// restoreCmd is `bridge restore [--config X] [--yes] <snapshot-dir>`
// — copies files from `<snapshot-dir>` back into the live data dir.
//
// The bridge serve process MUST be stopped before restore. Running
// restore while serve is up would leave the SQLite WAL inconsistent
// with the new main file, surface as silent corruption on next
// query. The CLI surfaces this as a `--yes`-gated prompt rather
// than enforcing a process check (no PID file today, and operators
// know to stop their service before invasive operations).
func restoreCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
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

	if !backup.LooksLikeSnapshotDir(snapshotDir) {
		fmt.Fprintf(stderr, "restore: %q is not a recognized snapshot directory (no readable manifest.json with a supported schema)\n", snapshotDir)
		return 2
	}

	cfg, resolvedCfgPath, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}

	if !*autoYes {
		fmt.Fprintf(stdout, "Restore from %s into %s?\n", snapshotDir, cfg.DataDir)
		fmt.Fprintln(stdout, "WARNING: stop the bridge service first. Restoring while `bridge serve` is running can corrupt the live database.")
		fmt.Fprint(stdout, "Type 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	targets := buildRestoreTargets(cfg, resolvedCfgPath)
	if err := backup.Restore(snapshotDir, targets); err != nil {
		fmt.Fprintf(stderr, "restore: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Restore complete. Start the bridge service.")
	return 0
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
		DataDir:    cfg.DataDir,
		ManifestDB: manifest.DefaultDBPath(cfg.DataDir),
		TokensJSON: filepath.Join(cfg.DataDir, tokensFileName),
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: configPath,
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
// Runs an immediate snapshot on startup unless a recent one already
// exists (so an operator who just finished `bridge init` has a
// baseline before any pairings, but a debugging restart loop doesn't
// cost one snapshot per boot), and then on the configured interval.
// All errors are logged to stderr — never fatal, since a failed
// backup must never take down the running bridge.
// runBackupTicker takes its cadence AND its retention as providers,
// re-read per use rather than captured once.
//
// `keep` is read at PRUNE time, which is the only moment it means
// anything: retention is not a lifecycle, it is an argument to a function
// that runs on a schedule, and capturing it was never buying anything.
//
// `interval` is read before each wait. A provider returning <= 0 parks
// the loop instead of ending it — the operator disabled the cadence, and
// re-enabling it must not require a restart. That is why the goroutine is
// now started UNCONDITIONALLY by the caller: the old shape only spawned
// it when hours > 0, so 0 → N had no loop alive to notice the change.
//
// `rearm` wakes the wait so a changed cadence is re-read immediately
// rather than after the old interval — up to a day, on the 24 h default.
func runBackupTicker(ctx context.Context, src backup.Sources, keep func() int, interval func() time.Duration, rearm <-chan struct{}, stdout, stderr io.Writer, status *sweepStatus[struct{}]) {
	backupsRoot := filepath.Join(src.DataDir, backup.BackupsDirName)
	doSnapshot := func(triggered string) {
		status.sweepStarted()
		dst, err := backup.Snapshot(ctx, src)
		if err != nil {
			fmt.Fprintf(stderr, "backup (%s): snapshot failed: %v\n", triggered, err)
			status.sweepFinished(nil)
			return
		}
		defer status.sweepFinished(&struct{}{})
		fmt.Fprintf(stdout, "backup (%s): wrote %s\n", triggered, dst)
		// PruneContext, and unconditionally — see backupCmd for both
		// rationales. The ticker's ctx is the one shutdown cancels, so a
		// prune stuck listing a wedged mount can't burn the whole grace.
		res, err := backup.PruneContext(ctx, backupsRoot, keep())
		// Warning, not a failure — see the same handling in
		// backupCmd. Pre-split this early-returned and suppressed
		// the "pruned N" line on every tick, forever.
		if res.ReapErr != nil {
			fmt.Fprintf(stderr, "backup (%s): orphan sweep reported a problem (prune unaffected): %v\n", triggered, res.ReapErr)
		}
		if err != nil {
			fmt.Fprintf(stderr, "backup (%s): prune failed: %v\n", triggered, err)
			return
		}
		if res.Deleted > 0 {
			fmt.Fprintf(stdout, "backup (%s): pruned %d older snapshot(s)\n", triggered, res.Deleted)
		}
	}

	// Throttle: if a snapshot exists within the skip threshold, the
	// startup snapshot is redundant — skip it. List errors are
	// non-fatal (rare; surfaces a misconfig or disk problem the user
	// should see) and fall through to writing the snapshot anyway.
	skip, latest, err := startupSnapshotShouldSkip(backupsRoot, time.Now().UTC(), startupBackupSkipThreshold)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "backup (startup): list existing snapshots: %v (writing anyway)\n", err)
		doSnapshot("startup")
	case skip:
		fmt.Fprintf(stdout, "backup (startup): recent snapshot at %s — skipping (threshold %s)\n",
			latest.Format(time.RFC3339), startupBackupSkipThreshold)
	default:
		doSnapshot("startup")
	}

	for {
		d := interval()
		var tickC <-chan time.Time
		var t *time.Timer
		if d > 0 {
			t = time.NewTimer(d)
			tickC = t.C
			status.scheduleNext(time.Now().Add(d))
		} else {
			// Dormant. Clearing the schedule matters: a stale "next
			// backup at 03:00" on the Jobs card after the operator set
			// intervalHours to 0 is a promise the loop will not keep.
			status.scheduleNext(time.Time{})
		}
		select {
		case <-ctx.Done():
			stopTimer(t)
			return
		case <-tickC:
			doSnapshot("scheduled")
		case <-rearm:
			// Cadence changed; re-read it. Deliberately no snapshot — a
			// settings save is not a request to back up.
			stopTimer(t)
		}
	}
}

// startupSnapshotShouldSkip reports whether the most-recent existing
// snapshot under backupsRoot is younger than threshold (i.e. recent
// enough that a startup snapshot would be redundant). Returns the
// most-recent CreatedAt timestamp on hit so the caller can log it.
// Returns (false, zero, err) on a List failure — caller should fall
// back to writing the snapshot.
func startupSnapshotShouldSkip(backupsRoot string, now time.Time, threshold time.Duration) (bool, time.Time, error) {
	entries, err := backup.List(backupsRoot)
	if err != nil {
		return false, time.Time{}, err
	}
	if len(entries) == 0 {
		return false, time.Time{}, nil
	}
	// backup.List sorts newest-first, so entries[0] is the most recent.
	latest := entries[0].CreatedAt
	if latest.IsZero() {
		return false, time.Time{}, nil
	}
	return now.Sub(latest) < threshold, latest, nil
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
		ManifestDB: manifest.DefaultDBPath(cfg.DataDir),
		TokensJSON: filepath.Join(cfg.DataDir, tokensFileName),
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: configPath,
	}
}
