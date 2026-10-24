package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// playerResetCmd gives up the player's identity under restore's exclusive
// data-dir lock and both admin probes. --yes skips only the confirmation.
func playerResetCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("output player reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	yes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(yes, "y", false, "alias for --yes")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "output player reset: unexpected arguments")
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	const action = "output player reset"
	release, fallback, refused := holdDataDirForMutation(cfg.DataDir, action, stderr)
	defer release()
	if refused {
		return 1
	}
	if fallback {
		fmt.Fprintln(stderr, "output player reset: the data directory's exclusive lock could not be taken; fix its permissions or use storage that supports locking, then reset again.")
		return 1
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, action, stderr) {
		return 1
	}
	if !*yes {
		fmt.Fprintln(stdout, "Give up the player's identity and clear its queue? Stop the bridge service first. The next start makes a new identity, a new device to controllers.")
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
	changed, err := backup.ResetPlayer(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "output player reset: %v\n", err)
		return 1
	}
	if !changed {
		fmt.Fprintln(stdout, "The box has no player identity; nothing changed.")
		return 0
	}
	fmt.Fprintln(stdout, "Player reset complete: recorded the identities given up and removed the UUID, queue and reservation. The next start makes a new identity with no queue, a new device to controllers.")
	return 0
}
