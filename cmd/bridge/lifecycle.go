package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// startCmd asks the installed service manager to bring the bridge
// service up if it isn't already running. Wrapper around
// packaging.Start() so operators don't have to memorise launchctl /
// systemctl / sc.exe incantations.
func startCmd(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !ensureInstalled(stderr) {
		return 2
	}
	if err := packaging.Start(); err != nil {
		return reportLifecycleError(stderr, "start", err)
	}
	fmt.Fprintln(stderr, "bridge: service start requested")
	return 0
}

// stopCmd asks the service manager to stop the bridge service. The
// install stays in place; a follow-up `bridge start` (or the OS
// itself on next boot) brings it back.
func stopCmd(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !ensureInstalled(stderr) {
		return 2
	}
	if err := packaging.Stop(); err != nil {
		return reportLifecycleError(stderr, "stop", err)
	}
	fmt.Fprintln(stderr, "bridge: service stop requested")
	return 0
}

// restartCmd unconditionally bounces the installed service. Used
// when a setting requires a restart to apply (e.g. `bridge update`'s
// post-install prompt; an operator-edited bridge.yaml with
// non-hot-reloadable changes).
func restartCmd(args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !ensureInstalled(stderr) {
		return 2
	}
	if err := packaging.Restart(); err != nil {
		return reportLifecycleError(stderr, "restart", err)
	}
	fmt.Fprintln(stderr, "bridge: service restart requested")
	return 0
}

// ensureInstalled prints a friendly error and returns false when no
// service unit is detected. Lifecycle commands need an installed
// unit to act on — operators running `bridge start` against a
// non-installed binary almost certainly want to be told to run
// `bridge init` first rather than a silent no-op.
func ensureInstalled(stderr io.Writer) bool {
	kind, _ := packaging.InstalledKind()
	if kind == packaging.KindNone {
		fmt.Fprintln(stderr, "bridge: no service unit installed (run `bridge init` first, or `bridge serve` to run in the foreground)")
		return false
	}
	return true
}

// reportLifecycleError translates packaging errors into the right
// exit code + a human-readable message. ErrSystemInstallNeedsRoot
// gets a specific hint; everything else is a generic failure.
func reportLifecycleError(stderr io.Writer, action string, err error) int {
	if errors.Is(err, packaging.ErrSystemInstallNeedsRoot) {
		fmt.Fprintf(stderr, "bridge: %s failed: %v\n", action, err)
		fmt.Fprintln(stderr, "  re-run as root or convert to a user-context install.")
		return 1
	}
	fmt.Fprintf(stderr, "bridge: %s failed: %v\n", action, err)
	return 1
}
