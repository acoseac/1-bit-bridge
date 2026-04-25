package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// updateCmd implements `bridge update [--check] [--yes]`.
//
// `--check` polls GitHub once and prints the result without
// installing. `--yes` skips the interactive confirmation. The CLI
// has no `--force`: the active-stream gate only applies to installs
// triggered through the admin console of a running bridge, and a
// CLI invocation runs in its own short-lived process — there's no
// session tracker to consult, so the install always proceeds
// regardless of whether some other bridge instance is serving
// downloads. The admin-console "Install anyway" affordance is the
// surface for that workflow.
//
// Subcommand exits:
//
//	0 — success (or "no update available" with --check)
//	1 — install failed
//	2 — usage error
//
// Restart: the CLI does NOT trigger restart itself. After a
// successful install the operator is told what to run; this keeps
// the install + restart steps decoupled in the operator's mental
// model, matching the admin-console workflow.
func updateCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	check := fs.Bool("check", false, "poll for an update and print the result; don't install")
	yes := fs.Bool("yes", false, "non-interactive: skip the confirmation prompt before install")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}

	upd := updater.New(updater.Options{})
	st := upd.CheckNow(ctx)

	fmt.Fprintf(stdout, "Current: %s (channel %s)\n", st.CurrentVersion, st.Channel)
	if st.LastError != "" {
		fmt.Fprintf(stdout, "Check error: %s\n", st.LastError)
	}
	if st.LatestVersion == "" {
		fmt.Fprintln(stdout, "Latest:  (no release information yet)")
		return 0
	}
	fmt.Fprintf(stdout, "Latest:  %s\n", st.LatestVersion)
	if st.ReleaseNotesURL != "" {
		fmt.Fprintf(stdout, "Notes:   %s\n", st.ReleaseNotesURL)
	}

	if !st.UpdateAvailable {
		fmt.Fprintln(stdout, "You're already running the latest release.")
		return 0
	}
	if *check {
		fmt.Fprintln(stdout, "Update available. Run `bridge update --yes` to install.")
		return 0
	}

	if !*yes {
		fmt.Fprint(stdout, "Install update? [y/N] ")
		var resp string
		fmt.Fscanln(stdin, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 0
		}
	}

	binaryPath, exeErr := os.Executable()
	if exeErr != nil {
		fmt.Fprintf(stderr, "os.Executable failed: %v\n", exeErr)
		return 1
	}

	// CLI install: pass nil sessions tracker (no running HTTP server
	// in this process) and Force=true (no inflight downloads to gate
	// on by construction).
	st, err = upd.Install(ctx, updater.InstallOptions{
		DataDir:    cfg.DataDir,
		BinaryPath: binaryPath,
		Sessions:   nil,
		Force:      true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Install failed: %v\n", err)
		if errors.Is(err, updater.ErrInstallNotSupported) {
			fmt.Fprintln(stderr, "On Windows, stop the bridge service, replace bridge.exe, and start it back up.")
		}
		return 1
	}

	fmt.Fprintf(stdout, "Installed %s. Restart the bridge to load the new binary:\n", st.LatestVersion)
	fmt.Fprintln(stdout, "  - launchd:  launchctl kickstart -k gui/$UID/com.acoseac.1-bit-bridge")
	fmt.Fprintln(stdout, "  - systemd:  systemctl --user restart com.acoseac.1-bit-bridge")
	fmt.Fprintln(stdout, "  - or hit \"Restart\" in the admin console.")
	fmt.Fprintf(stdout, "\nA backup of the previous binary is at %s.bak — startup housekeeping will roll back automatically if the new bridge fails to come up at version %s within %s of restart.\n",
		binaryPath, st.LatestVersion, updater.RecencyWindow())
	_ = version.ServerVersion // silence unused-import warning if version isn't referenced elsewhere
	return 0
}
