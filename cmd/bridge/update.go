package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// updateReleases is the client the release check asks, nil for the GitHub
// releases API's own (updater.New's default): a test points one at a server of
// its own, so that `bridge update --check` asks nothing of the network.
var updateReleases *updater.Client

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
//	1 — install failed, or update checks are off
//	2 — usage error
//
// Update checks off (update.checkEnabled, backlog B761) refuse both things
// this command does, the check and the install, before anything is asked of
// GitHub or opened, read from the config the service loads through the same
// loader, its file and BRIDGE_UPDATE_CHECK_ENABLED alike: an off set only in
// the service's environment (a unit or a compose file) must be in this
// shell's too for the command to see it. A rollback is the console's, and
// the switch never refuses it.
//
// Restart: the CLI does NOT trigger restart itself. After a
// successful install the operator is told what to run; this keeps
// the install + restart steps decoupled in the operator's mental
// model, matching the admin-console workflow.
func updateCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	check := fs.Bool("check", false, "poll for an update and print the result; don't install")
	yes := fs.Bool("yes", false, "non-interactive: skip the install + post-install restart prompts")
	fs.BoolVar(yes, "y", *yes, "alias for --yes")
	overrideClientFloor := fs.Bool("override-client-floor", false,
		"install even if the candidate's MinClientVersion would orphan a still-paired older device. "+
			"Operator-only; the auto-installer never bypasses the gate.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, cfgPath, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	if !cfg.Update.EffectiveCheckEnabled() {
		refuseUpdateChecksOff(stderr, cfgPath)
		return 1
	}
	// An install writes its marker in the data dir, which would be made in
	// its place where its volume is not mounted (backlog B727); --check reads.
	if !*check && refuseDataDirAway("update", cfgPath, cfg.DataDir, stderr) {
		return 1
	}
	// The candidate is checked against this file before its swap (backlog
	// B613), and runs from another directory: an absolute path names the
	// same file there.
	if abs, err := fsutil.AbsAsWritten(cfgPath); err == nil {
		cfgPath = abs
	}

	// Open the token store so the compat gate has a snapshot to
	// consult on install. A failure here is non-fatal — without
	// the snapshot, the gate stays permissive (matches the
	// pre-Phase-C behaviour); the operator just won't get the
	// MinClientVersion-would-orphan refusal.
	var tokenSnapshot func() []auth.Token
	if store, err := auth.OpenStore(filepath.Join(cfg.DataDir, tokensFileName)); err == nil {
		tokenSnapshot = store.List
	} else {
		fmt.Fprintf(stderr, "warning: token store unavailable (%v) — compat gate will be permissive\n", err)
	}

	upd := updater.New(updater.Options{
		TokenSnapshot: tokenSnapshot,
		Client:        updateReleases,
		// The switch as the config loaded says it, which the refusal above
		// read: the updater's own gates ask it too.
		CheckEnabled: cfg.Update.EffectiveCheckEnabled,
	})
	st, err := upd.CheckNow(ctx)
	if err != nil {
		// The switch's refusal, the one error a check answers.
		fmt.Fprintf(stderr, "bridge update: %v\n", err)
		return 1
	}

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
	// A version held back for refusing these settings, or for predating
	// their check: said before the install asks for it again.
	if hold := updater.HeldBack(cfg.DataDir, cfgPath, st.LatestVersion); hold.Recorded {
		fmt.Fprintf(stdout, "Held:    %s\n", hold.Reason)
	}
	if *check {
		fmt.Fprintln(stdout, "Update available. Run `bridge update --yes` to install.")
		return 0
	}

	if !*yes {
		// To stderr (like the restart prompt below): `bridge update >file` would
		// otherwise hide this prompt in the redirected stream and block on
		// invisible input.
		fmt.Fprint(stderr, "Install update? [y/N] ")
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
	// Resolve symlinks so a symlinked install (e.g. /usr/local/bin/bridge
	// → /opt/1-bit-bridge/bridge) swaps the real binary, not the link.
	// Mirrors the binary resolution in init.go's service install.
	if resolved, lerr := filepath.EvalSymlinks(binaryPath); lerr == nil {
		binaryPath = resolved
	}

	// CLI install: pass nil sessions tracker (no running HTTP server
	// in this process) and Force=true (no inflight downloads to gate
	// on by construction).
	st, err = upd.Install(ctx, updater.InstallOptions{
		DataDir:            cfg.DataDir,
		BinaryPath:         binaryPath,
		Sessions:           nil,
		Force:              true,
		OverrideCompatGate: *overrideClientFloor,
		ConfigPath:         cfgPath,
	})
	if err != nil {
		reportInstallFailure(stderr, err, cfgPath)
		return 1
	}

	if st.InstallWarning != "" {
		fmt.Fprintf(stderr, "warning: %s\n", st.InstallWarning)
	}
	fmt.Fprintf(stdout, "Installed %s.\n", st.LatestVersion)
	// What is checked (backlog B660): the version installed is confirmed
	// only once one start of it has answered as ready and stayed up, the
	// backup kept until then; a crash loop is rolled back by its count, but
	// not past a migration of the store; and a restart that comes up as
	// another version restores the backup within the boot window.
	fmt.Fprintf(stdout, "A backup of the previous binary is at %s.bak. %s is confirmed once one start of it has answered as ready "+
		"(its API listener, /v1/health and its store) and stayed up for %s, and the backup is kept until then. After %d starts of it "+
		"that do not end cleanly, the next start puts the backup back, unless a migration of the store has begun, which `bridge doctor` "+
		"then reports; and a restart that does not come up as version %s within %s puts the backup back too.\n",
		binaryPath, st.LatestVersion, updater.ProbationWindow, updater.ProbationFailedStarts, st.LatestVersion, updater.RecencyWindow())

	// Post-install restart hand-off. Three branches:
	//   --yes / non-interactive (CI / scripts / piped stdin):
	//       restart immediately if a service unit is installed,
	//       otherwise print the manual hint and exit. Non-zero
	//       interactivity rules out the prompt.
	//   interactive on a TTY with a service unit installed:
	//       prompt "Restart now? [Y/n]" — Enter or 'y' restarts.
	//   no service unit installed:
	//       can't restart from here (operator is running serve in
	//       a terminal); print the manual hint as before.
	kind, _ := packaging.InstalledKind()
	hasService := kind != packaging.KindNone
	stdinIsTTY := isStdinTTY(stdin)

	if hasService && (*yes || stdinIsTTY) {
		shouldRestart := *yes
		if !shouldRestart {
			// Write the prompt to STDERR, not stdout. `bridge
			// update >file` from a terminal would otherwise hide
			// the question in the redirected stream and block
			// for input with no visible cue (CodeRabbit Major
			// post-merge on PR #82). Same convention as
			// `cert rotate` and `restore` — confirmation prompts
			// always go to stderr.
			fmt.Fprint(stderr, "\nRestart the service now to apply? [Y/n] ")
			var resp string
			fmt.Fscanln(stdin, &resp)
			r := strings.ToLower(strings.TrimSpace(resp))
			shouldRestart = r == "" || r == "y" || r == "yes"
		}
		if shouldRestart {
			if err := packaging.Restart(); err != nil {
				fmt.Fprintf(stderr, "restart: %v\n", err)
				printManualRestartHint(stderr, kind)
				return 1
			}
			fmt.Fprintln(stdout, "bridge: service restart requested.")
			return 0
		}
	}

	fmt.Fprintln(stdout, "\nRestart the bridge to load the new binary:")
	printManualRestartHint(stdout, kind)
	return 0
}

// refuseUpdateChecksOff says why `bridge update` asks nothing and installs
// nothing while update checks are off (backlog B761), naming what turned them
// off, the variable in this shell's environment or the file, and how to turn
// them on; the console's rollback is not refused.
func refuseUpdateChecksOff(stderr io.Writer, cfgPath string) {
	source := fmt.Sprintf("update.checkEnabled is false in %s", cfgPath)
	if _, set := config.UpdateCheckEnabledFromEnv(); set {
		source = "BRIDGE_UPDATE_CHECK_ENABLED in this environment says off"
	}
	fmt.Fprintf(stderr, "bridge update: update checks are off (%s), so this command neither asks GitHub for a "+
		"release nor installs one.\n", source)
	fmt.Fprintln(stderr, "Turn them on in the console's Settings → Updates, or set update.checkEnabled: true and unset "+
		"BRIDGE_UPDATE_CHECK_ENABLED, then run it again. A rollback, in the console, works with them off.")
}

// reportInstallFailure prints why `bridge update` installed nothing, and what
// to do: for a version that refuses the settings, each reason it gave by
// line, and the file to fix (backlog B613).
func reportInstallFailure(stderr io.Writer, err error, cfgPath string) {
	fmt.Fprintf(stderr, "Install failed: %v\n", err)
	if errors.Is(err, updater.ErrConfigRefused) {
		fmt.Fprintf(stderr, "The running bridge is unchanged. Fix %s, then run `bridge update` again.\n", cfgPath)
		return
	}
	if errors.Is(err, updater.ErrMarkerLockUnavailable) {
		// The error says why and what to do (backlog B692).
		fmt.Fprintln(stderr, "The running bridge is unchanged.")
		return
	}
	if errors.Is(err, updater.ErrInstallNotSupported) {
		fmt.Fprintln(stderr, "On Windows, stop the bridge service, replace bridge.exe, and start it back up.")
	}
	if errors.Is(err, updater.ErrCompatGateRefused) {
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "The candidate release would orphan a still-paired older iOS device.")
		fmt.Fprintln(stderr, "Either update the device to a supported version, or rerun with")
		fmt.Fprintln(stderr, "  bridge update --yes --override-client-floor")
		fmt.Fprintln(stderr, "to install anyway. Older devices will refuse to authenticate after the swap.")
	}
}

// printManualRestartHint emits the service-restart commands
// appropriate for the installed unit kind. Used on the install-
// succeeded-but-restart-skipped path AND as a fallback when the
// auto-restart attempt fails (service-manager glitch, permission
// flap).
//
// Per-platform output (CodeRabbit Major post-merge on PR #82):
// pre-fix the function emitted launchd + systemd guidance
// regardless of platform, so Windows SCM operators were told to
// run launchctl. Now we branch on kind and only emit the line
// for the actually-installed unit type.
func printManualRestartHint(w io.Writer, kind packaging.ServiceKind) {
	switch kind {
	case packaging.KindLaunchdUser:
		// User-domain LaunchAgent — kickstart against the
		// per-uid `gui/$UID` domain. No sudo.
		fmt.Fprintln(w, "  - launchctl kickstart -k gui/$UID/com.acoseac.1-bit-bridge")
	case packaging.KindLaunchdSystem:
		// System-domain LaunchDaemon — different launchctl
		// domain (`system/`), and root privilege required.
		// CodeRabbit Major post-merge on PR #85 flagged the
		// previous shared `gui/$UID` line as wrong for
		// system-installed bridges.
		fmt.Fprintln(w, "  - sudo launchctl kickstart -k system/com.acoseac.1-bit-bridge")
	case packaging.KindSystemdUser:
		fmt.Fprintln(w, "  - systemctl --user restart com.acoseac.1-bit-bridge")
	case packaging.KindSystemdSystem:
		fmt.Fprintln(w, "  - sudo systemctl restart com.acoseac.1-bit-bridge")
	case packaging.KindWindowsSCM:
		fmt.Fprintln(w, "  - net stop 1-bit-bridge && net start 1-bit-bridge")
		fmt.Fprintln(w, "    (or use Services.msc → 1-bit-bridge → Restart)")
	case packaging.KindWindowsStartup:
		fmt.Fprintln(w, "  - end the current bridge.exe in Task Manager;")
		fmt.Fprintln(w, "    the Startup-folder launcher will spawn a fresh one on next logon")
		fmt.Fprintln(w, "    (or run `bridge serve` from a terminal now to come up immediately)")
	default:
		fmt.Fprintln(w, "  - no service unit installed; run `bridge serve` to start in the foreground")
	}
	fmt.Fprintln(w, "  - or hit \"Restart\" in the admin console.")
}

// isStdinTTY classifies the passed reader as interactive only when
// it's actually os.Stdin AND that file descriptor refers to a
// terminal. Pipes / redirects / tests fall through to non-
// interactive (matches the convention init / restore use to gate
// their own prompts).
func isStdinTTY(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
