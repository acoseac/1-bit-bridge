package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// manifestCmd dispatches `bridge manifest <subcommand>`. The single
// subcommand today is `clear-missing` — an operator escape hatch for
// the missing_count grace period. An operator who KNOWS a mount has
// been permanently removed (decommissioned NAS, moved data dir, etc.)
// doesn't want to wait N scans for cleanup; this purges every row
// across `tracks` and `folders` whose missing_count > 0 in a single
// SQLite transaction.
//
// Destructive: requires typed `WIPE` confirmation by default. Mirrors
// `bridge tsnet logout`'s exact-phrase pattern — a `[y/N]` fat-finger
// prompt would be too easy to misuse on a destructive action.
func manifestCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bridge manifest <subcommand>")
		fmt.Fprintln(stderr, "subcommands:")
		fmt.Fprintln(stderr, "  clear-missing   purge rows that have been missing across recent scans")
		return 2
	}
	switch args[0] {
	case "clear-missing":
		return manifestClearMissingCmd(ctx, args[1:], stdin, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "usage: bridge manifest <subcommand>")
		fmt.Fprintln(stdout, "subcommands:")
		fmt.Fprintln(stdout, "  clear-missing   purge rows that have been missing across recent scans")
		return 0
	default:
		fmt.Fprintf(stderr, "unknown manifest subcommand: %s\n", args[0])
		return 2
	}
}

// manifestClearMissingCmd implements `bridge manifest clear-missing
// [--yes]`. Defaults to interactive confirmation requiring the operator
// to type the literal phrase `WIPE` on stdin. `--yes` / `-y` skips the
// prompt for scripted use, matching the convention `bridge update` /
// `bridge init` / `bridge cert rotate` already established.
//
// `--config <path>` is honored so an operator with a non-default config
// can target a specific bridge install. Without it, resolution follows
// the shared CLI precedence — ./bridge.yaml first, THEN the platform
// config dir. It used to jump straight to the platform dir, the exact
// inversion resolveConfigPath's docblock warns about: an operator with
// both a platform install and a local fixture running this from the
// fixture directory silently opened the PRODUCTION database and deleted
// rows there. The resolved config + DB paths are printed before anything
// is touched (on the --yes path too, so a scripted run leaves a record
// of which database it hit).
func manifestClearMissingCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifest clear-missing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "skip the WIPE confirmation prompt")
	fs.BoolVar(&yes, "y", false, "short alias for --yes")
	cfgPath := fs.String("config", "", "path to bridge.yaml (default: ./bridge.yaml, else the platform config dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, resolvedCfgPath, err := loadCLIConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 1
	}
	dbPath := manifest.DefaultDBPath(cfg.DataDir)
	// Absolute, because the whole point of printing it is to say WHICH
	// install: the local-first resolver hands back a bare relative
	// "bridge.yaml", which names the very ambiguity this line exists to
	// resolve. Abs failure (an unreadable CWD) falls back to the raw
	// value rather than losing the line entirely.
	if abs, absErr := filepath.Abs(resolvedCfgPath); absErr == nil {
		resolvedCfgPath = abs
	}
	fmt.Fprintf(stdout, "Config:   %s\n", resolvedCfgPath)
	fmt.Fprintf(stdout, "Database: %s\n", dbPath)

	// Refuse while a bridge is up. This is a two-table DELETE from a SECOND
	// process, and Store.mu serialises writers within one process only —
	// busy_timeout is a retry, not a serializer. enrichmentRetryCmd and
	// tryLibraryViaAdmin both refuse the same state for the same reason.
	if refuseIfBridgeMayBeRunning(ctx, cfg, "manifest clear-missing", stderr) {
		return 1
	}

	if !yes {
		fmt.Fprintln(stdout, "This will delete every track / folder row whose `missing_count > 0`")
		fmt.Fprintln(stdout, "in the manifest DB above. Use this only when you KNOW a mount has")
		fmt.Fprintln(stdout, "been permanently removed (decommissioned NAS, moved data dir, etc.).")
		fmt.Fprintln(stdout, "Library files on disk are unaffected.")
		fmt.Fprint(stdout, "Type WIPE to confirm: ")
		scanner := bufio.NewScanner(stdin)
		if !scanner.Scan() {
			fmt.Fprintln(stderr, "aborted: no input")
			return 1
		}
		if strings.TrimSpace(scanner.Text()) != "WIPE" {
			fmt.Fprintln(stderr, "aborted: confirmation phrase did not match")
			return 1
		}
	}

	store, err := manifest.OpenStore(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()

	n, err := store.ClearMissingCounts(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "clear-missing: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Purged %d rows (missing_count > 0) from manifest.\n", n)
	return 0
}
