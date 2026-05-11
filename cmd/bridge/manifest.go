package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
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
func manifestCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bridge manifest <subcommand>")
		fmt.Fprintln(stderr, "subcommands:")
		fmt.Fprintln(stderr, "  clear-missing   purge rows that have been missing across recent scans")
		return 2
	}
	switch args[0] {
	case "clear-missing":
		return manifestClearMissingCmd(args[1:], stdin, stdout, stderr)
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
// can target a specific bridge install. Defaults to the shared default
// config dir, same shape `bridge status` etc. use.
func manifestClearMissingCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifest clear-missing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "skip the WIPE confirmation prompt")
	fs.BoolVar(&yes, "y", false, "short alias for --yes")
	cfgPath := fs.String("config", "", "path to bridge.yaml (default: <default config dir>/bridge.yaml)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := loadConfigForCLI(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 1
	}

	if !yes {
		fmt.Fprintln(stdout, "This will delete every track / folder row whose `missing_count > 0`")
		fmt.Fprintln(stdout, "in the manifest DB. Use this only when you KNOW a mount has been")
		fmt.Fprintln(stdout, "permanently removed (decommissioned NAS, moved data dir, etc.).")
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

	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()

	n, err := store.ClearMissingCounts()
	if err != nil {
		fmt.Fprintf(stderr, "clear-missing: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Purged %d rows (missing_count > 0) from manifest.\n", n)
	return 0
}

// loadConfigForCLI is a thin wrapper around config.Load that honors
// `--config <path>` overrides. Other manifest subcommands can share it
// once they exist.
func loadConfigForCLI(path string) (*config.Config, string, error) {
	resolved := path
	if resolved == "" {
		dir, err := packaging.DefaultConfigDir()
		if err != nil {
			return nil, "", fmt.Errorf("resolve default config dir: %w", err)
		}
		resolved = filepath.Join(dir, "bridge.yaml")
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return nil, resolved, err
	}
	return cfg, resolved, nil
}
