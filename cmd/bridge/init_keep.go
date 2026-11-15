package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// keptConfigNotChanged closes each refusal of a run that keeps the config
// (prepareKeptDataDir): the install is as the run found it.
const keptConfigNotChanged = "the config was NOT changed, and no service was installed or started."

// prepareKeptDataDir is what a run of bridge init that keeps the config at
// cfgPath (--yes without --force, or a no at "Overwrite?") asks of the data
// directory, before its preflight and any service work (backlog B664). The run
// installs the service on that config and starts it, and the unit systemd and
// launchd run has the data directory for its working directory
// (internal/packaging's templates), so one that is not there fails the unit
// at its chdir, before serve can make it; the run answered as done all the
// same, and over a data directory, or a config, serve refuses too.
//
// The data directory is serve's: the config loaded as serve loads it
// (loadServeConfigFile: the file and the BRIDGE_ variables, BRIDGE_DATA_DIR
// among them, a relative or default dataDir resolved against the config's
// directory), never the one init gives a first install. Of it, the two steps
// serve takes before its lock, which holdDataDirForMutation takes for every
// command that writes the data dir: the check of the record beside the config
// (refuseDataDirAway), so that a data directory that is not there and may be
// a volume not mounted yet is refused and never made, and serve's preparation
// (prepareServeDataDir), which makes a data directory that is not there as
// serve would (fsutil.MkdirAll at 0700, which run as root gives each
// directory it makes its parent's owner) and requires a directory there. No
// rule of its own: whether the data directory can be written is not asked,
// as serve's preparation does not ask it (B659).
//
// It answers the data directory and true once both have taken it, having
// said so on stdout where it made it; and false where serve would not start,
// having said why on stderr and made and installed nothing: a config that
// does not load (never a data directory of init's own in its place), a data
// directory its record refuses (its volume away, its identity missing or
// another's, a record or a marker that does not read or that this user may
// not read), and one serve cannot make or use (an entry in its way, a parent
// this user may not write). The service init installs runs as the user who
// runs it (systemd's and launchd's user units, Windows' Startup launcher), so
// what this user may not read or make its serve may not either, and those
// refusals say to run init as the user the bridge runs as.
func prepareKeptDataDir(cfgPath string, stdout, stderr io.Writer) (string, bool) {
	cfg, _, _, _, err := loadServeConfigFile(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "init: bridge serve does not start on the config this run keeps: %v\n", err)
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintln(stderr, "this user cannot read it: run init as the user the bridge runs as.")
		} else {
			fmt.Fprintln(stderr, "fix it, or run init again with --force to rewrite it.")
		}
		fmt.Fprintln(stderr, keptConfigNotChanged)
		return "", false
	}
	if refuseDataDirAway("init", cfgPath, cfg.DataDir, stderr) {
		fmt.Fprintln(stderr, keptConfigNotChanged)
		return "", false
	}
	_, missing := os.Lstat(cfg.DataDir)
	if err := prepareServeDataDir(cfg.DataDir); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintf(stderr, "init: %v; this user cannot make it, and bridge serve starts only on a data directory it can "+
				"make or use: run init as the user the bridge runs as, or set dataDir in bridge.yaml to a directory that "+
				"user can make or use, then run init again.\n", err)
		} else {
			fmt.Fprintf(stderr, "init: %v; bridge serve starts only on a data directory it can make or use: move what "+
				"stands in its way, or set dataDir in bridge.yaml to a directory, then run init again.\n", err)
		}
		fmt.Fprintln(stderr, keptConfigNotChanged)
		return "", false
	}
	if errors.Is(missing, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "made the data directory %s, as bridge serve would as it starts\n", cfg.DataDir)
	}
	return cfg.DataDir, true
}
