package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// serveRefusal is a refusal serve makes from its config alone, past Load's
// own: what serve prints, as it always has, and the same without a path or a
// value, for the settings check and the early rollback's record.
type serveRefusal struct {
	printed string
	reason  string
}

// serveConfigRefusal is every refusal serve makes from its config alone, past
// Load's own (the decode, the defaults, the environment, the paths,
// Normalize and Validate), and before it writes anything: two library roots
// sharing a folder name, and a Tailscale mode it does not know. runServe asks
// it right after the load, where the early rollback can still put the
// previous binary back, and the settings check (`bridge serve
// --validate-config`) asks the same, so a candidate's check refuses what its
// serve would. nil when serve takes the config.
//
// The Tailscale mode was refused after the stores had opened until backlog
// B613; nothing between here and there depends on it.
func serveConfigRefusal(cfg *config.Config) *serveRefusal {
	if err := bridgefs.ValidateRoots(cfg.LibraryRoots); err != nil {
		// The refusal names the roots, which serve prints for the
		// operator; the reason does not.
		return &serveRefusal{printed: "config: " + err.Error(),
			reason: "libraryRoots: two library roots have one folder name, compared case-insensitively; a library of several roots needs a different folder name for each"}
	}
	if _, err := cfg.Tailscale.EffectiveMode(); err != nil {
		return &serveRefusal{printed: "config: " + err.Error(), reason: err.Error()}
	}
	return nil
}

// checkServeConfig is `bridge serve --validate-config`: the settings check a
// candidate answers before the updater installs it (internal/updater,
// CheckConfig; backlog B613). It loads the config exactly as runServe does
// (resolveConfigPath, the boot read, Load's decode, defaults, environment,
// paths and validation) and asks serveConfigRefusal and, with --addr, the
// override's check, then exits: updater.ConfigCheckValidExit when serve
// would take the config, ConfigCheckRefusedExit when it would refuse it,
// each reason on its own stderr line after updater.ConfigRefusedPrefix,
// naming no path and no value (config.RefusedError's Reasons), and
// ConfigCheckFailedExit when the config cannot be read. It writes nothing,
// opens no store and binds nothing, and it never asks whether a library
// root can be reached (CheckLibraryRootsAccessible): that is the host's
// state at the moment, and the state of the user who runs the check, which
// `sudo bridge update` makes root; never the settings'.
//
// The codes and the prefix are a contract with every updater that runs this
// binary as a candidate, this release's and the earlier ones that have the
// check, so neither changes.
func checkServeConfig(explicit, addrOverride string, stdout, stderr io.Writer) int {
	resolved, found := resolveConfigPath(explicit)
	if abs, err := fsutil.AbsAsWritten(resolved); err == nil {
		resolved = abs
	}
	if !found {
		fmt.Fprintf(stderr, "config check: %v\n", errConfigNotFound(explicit))
		return updater.ConfigCheckFailedExit
	}
	cfg, _, _, _, err := loadServeConfigFile(resolved)
	if err != nil {
		var refused *config.RefusedError
		if !errors.As(err, &refused) {
			fmt.Fprintf(stderr, "config check: %v\n", err)
			return updater.ConfigCheckFailedExit
		}
		return refuseConfig(stderr, refused.Reasons()...)
	}
	if refusal := serveConfigRefusal(cfg); refusal != nil {
		return refuseConfig(stderr, refusal.reason)
	}
	if addrOverride != "" {
		if err := config.ValidateBindAddress("--addr", addrOverride); err != nil {
			return refuseConfig(stderr, err.Error())
		}
	}
	fmt.Fprintf(stdout, "config ok: 1-bit-bridge %s loads these settings\n", version.ServerVersion)
	return updater.ConfigCheckValidExit
}

// refuseConfig writes each reason after updater.ConfigRefusedPrefix, one a
// line, and returns the check's refusal.
func refuseConfig(stderr io.Writer, reasons ...string) int {
	if len(reasons) == 0 {
		reasons = []string{"the settings do not load"}
	}
	for _, r := range reasons {
		fmt.Fprintf(stderr, "%s%s\n", updater.ConfigRefusedPrefix, r)
	}
	return updater.ConfigCheckRefusedExit
}
