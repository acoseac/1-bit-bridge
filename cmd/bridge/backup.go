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
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	keep := fs.Int("keep", 7, "number of snapshots to retain (0 disables prune)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}

	src := buildBackupSources(cfg, *configPath)
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

	if *keep > 0 {
		backupsRoot := filepath.Join(cfg.DataDir, backup.BackupsDirName)
		deleted, err := backup.Prune(backupsRoot, *keep)
		if err != nil {
			fmt.Fprintf(stderr, "prune: %v\n", err)
			return 1
		}
		if deleted > 0 {
			fmt.Fprintf(stdout, "Pruned %d older snapshot(s) (kept %d most recent).\n", deleted, *keep)
		}
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
	configPath := fs.String("config", "bridge.yaml", "path to config file")
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

	cfg, err := config.Load(*configPath)
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

	targets := buildRestoreTargets(cfg, *configPath)
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
		TokensJSON: filepath.Join(cfg.DataDir, "tokens.json"),
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: configPath,
	}
}

// runBackupTicker is the periodic snapshot loop wired into serveCmd.
// Runs an immediate snapshot on startup (so an operator who just
// finished `bridge init` has a baseline before any pairings) and
// then on the configured interval. All errors are logged to stderr
// — never fatal, since a failed backup must never take down the
// running bridge.
func runBackupTicker(ctx context.Context, src backup.Sources, keep int, interval time.Duration, stdout, stderr io.Writer) {
	doSnapshot := func(triggered string) {
		dst, err := backup.Snapshot(ctx, src)
		if err != nil {
			fmt.Fprintf(stderr, "backup (%s): snapshot failed: %v\n", triggered, err)
			return
		}
		fmt.Fprintf(stdout, "backup (%s): wrote %s\n", triggered, dst)
		if keep > 0 {
			backupsRoot := filepath.Join(src.DataDir, backup.BackupsDirName)
			deleted, err := backup.Prune(backupsRoot, keep)
			if err != nil {
				fmt.Fprintf(stderr, "backup (%s): prune failed: %v\n", triggered, err)
				return
			}
			if deleted > 0 {
				fmt.Fprintf(stdout, "backup (%s): pruned %d older snapshot(s)\n", triggered, deleted)
			}
		}
	}

	doSnapshot("startup")

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doSnapshot("scheduled")
		}
	}
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
		TokensJSON: filepath.Join(cfg.DataDir, "tokens.json"),
		ServerCert: certPath,
		ServerKey:  keyPath,
		BridgeYAML: configPath,
	}
}
