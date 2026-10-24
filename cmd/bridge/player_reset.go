package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// playerResetCmd gives up the player's identity under restore's exclusive
// data-dir lock and both admin probes. --yes skips only the confirmation.
func playerResetCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset-player", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	discard := fs.Bool("discard-floor", false, "discard a lost or exhausted floor and permanently seal player identity restores")
	yes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(yes, "y", false, "alias for --yes")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "reset-player: unexpected arguments")
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	const action = "reset-player"
	release, fallback, refused := holdDataDirForMutation(cfg.DataDir, action, stderr)
	defer release()
	if refused {
		return 1
	}
	if fallback {
		fmt.Fprintln(stderr, "reset-player: the data directory's exclusive lock could not be taken; fix its permissions or use storage that supports locking, then reset again.")
		return 1
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, action, stderr) {
		return 1
	}
	if *discard {
		fmt.Fprintln(stdout, "Discarding the floor permanently seals player identity restores on this box. Database, tokens and config can still be restored without a player identity.")
	}
	if !*yes {
		if *discard {
			fmt.Fprintln(stdout, "Discard the player's counter floor, clear its UUID and queue, and seal identity restores? Stop the bridge service first.")
		} else {
			fmt.Fprintln(stdout, "Rotate the player's identity and clear its queue, keeping its counter floor? Stop the bridge service first. The next start makes a new identity, a new device to controllers.")
		}
		fmt.Fprint(stderr, "Type 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, action, stderr) {
		return 1
	}
	var removed []string
	for _, name := range []string{"player-uuid", "player-queue.json"} {
		if _, statErr := os.Lstat(filepath.Join(cfg.DataDir, name)); statErr == nil {
			removed = append(removed, name)
		}
	}
	var changed bool
	if *discard {
		changed, err = backup.DiscardPlayerFloor(cfg.DataDir)
	} else {
		changed, err = backup.ResetPlayer(cfg.DataDir)
	}
	if err != nil {
		fmt.Fprintf(stderr, "reset-player: %v\n", err)
		return 1
	}
	if !changed {
		fmt.Fprintln(stdout, "The box has no player identity; nothing changed.")
		return 0
	}
	fmt.Fprint(stdout, "Player reset complete")
	if len(removed) != 0 {
		fmt.Fprintf(stdout, ": removed %s", strings.Join(removed, " and "))
	}
	if *discard {
		fmt.Fprintln(stdout, ". Player identity restores are permanently sealed on this box.")
	} else {
		fmt.Fprintln(stdout, ". The counter floor is retained.")
	}
	fmt.Fprintln(stdout, "The next start makes a new identity with no queue, a new device to controllers.")
	return 0
}
