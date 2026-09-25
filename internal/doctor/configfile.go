package doctor

import (
	"context"
	"errors"
	"io/fs"
	"strings"
)

const checkNameConfigFile = "config-file"

// ConfigFile is what the caller found when it looked for bridge.yaml, so
// the report can say which config its other checks graded, and refuse one
// that is there but will not load.
type ConfigFile struct {
	// Path is the config the caller found, as an absolute path. Empty
	// when none was found.
	Path string
	// Tried lists every location the caller consulted, for the line that
	// says none was found.
	Tried []string
	// LoadErr is why Path could not be graded: a named file that is not
	// there, one this user cannot reach or read, or one that does not
	// load. Nil when it loaded, and when there was no Path.
	//
	// The port checks read it too. A config that did not load set no
	// ports and no pid file, so they report that they were not checked
	// rather than grade the defaults the caller seeded in its place (see
	// ungradedConfigPortCheck). So does config-dir, for two of the three:
	// a named config that is not there leaves it nothing to vouch for,
	// and one this user cannot read says this run is not by the user the
	// bridge runs as, whom its probes would have to answer for (see
	// checkConfigDir).
	LoadErr error
}

// configProblem is why a config that was named or found could not be
// graded. checkConfigFile turns it into that line's verdict, and the port
// checks and config-dir into their "not checked" reasons, so the lines
// cannot disagree about what went wrong.
type configProblem int

const (
	// noConfigProblem: the config loaded, or none was named or found (the
	// pre-setup state), or there was no lookup.
	noConfigProblem configProblem = iota
	// configUnreadable: this user may not reach or read it.
	configUnreadable
	// configNotThere: a path the caller named that does not exist.
	configNotThere
	// configDoesNotLoad: it is there, and config.Load refuses it.
	configDoesNotLoad
)

// problem classifies c's LoadErr, a permission failure first: that is a
// fact about who ran doctor, whatever else the error says. A nil c has
// none.
func (c *ConfigFile) problem() configProblem {
	switch {
	case c == nil || c.LoadErr == nil:
		return noConfigProblem
	case errors.Is(c.LoadErr, fs.ErrPermission):
		return configUnreadable
	case errors.Is(c.LoadErr, fs.ErrNotExist):
		return configNotThere
	default:
		return configDoesNotLoad
	}
}

// ranWithoutIt ends the hint on a config that could not be graded. The
// checks after config-file ran without it: the ones with a default on the
// default, and the port checks not at all, since a default port is a
// guess at the one the config sets.
const ranWithoutIt = "the checks below ran without it, on defaults or not at all"

// checkConfigFile names the bridge.yaml the report graded, and FAILS on
// one that was named or found but cannot be graded.
//
// A config that nobody named and nothing found is not an error: doctor
// runs before `bridge init` has written one, so "none found" is ok, and
// it names where the caller looked. A config that EXISTS and does not
// load is a different fact. Every command that reads it refuses it,
// `bridge serve` will not start with it, and the checks beside this one
// ran without it. Until this check the load error was dropped without a
// word: a typo'd key read "all clear", exit 0, graded against ports the
// file does not name, while `bridge status` exited 2 on the same file.
// A path the caller NAMED that is not there fails for the same reason: an
// "all clear" graded on defaults would answer for a different config
// than the one asked about.
//
// A permission failure is a WARN, not a fail. It is a fact about the
// doctor run rather than the file: on the public-mode layout the operator
// is not the service user, which is also why the cert checks do not grade
// a key they cannot read, and why config-dir does not probe a directory
// for a user who is not the bridge's.
func checkConfigFile(_ context.Context, d Deps) Check {
	c := d.ConfigFile
	switch c.problem() {
	case configUnreadable:
		return warn(checkNameConfigFile,
			c.Path+" is not readable by this user: "+oneLine(c.LoadErr.Error()),
			"run `bridge doctor` as the user the bridge runs as to grade this install; "+ranWithoutIt)
	case configNotThere:
		return fail(checkNameConfigFile,
			c.Path+" does not exist",
			"the config named for this run is not there, so nothing was graded against it; check the path ("+ranWithoutIt+"). Before `bridge init`, run `bridge doctor` without --config")
	case configDoesNotLoad:
		return fail(checkNameConfigFile,
			c.Path+" does not load: "+oneLine(c.LoadErr.Error()),
			"every command that reads this file refuses it, and `bridge serve` will not start with it; fix it and re-run `bridge doctor` ("+ranWithoutIt+")")
	}
	switch {
	case c == nil:
		return ok(checkNameConfigFile, "no config lookup — check skipped")
	case c.Path != "":
		return ok(checkNameConfigFile, c.Path)
	case len(c.Tried) > 0:
		return ok(checkNameConfigFile,
			"none found (looked at "+strings.Join(c.Tried, ", ")+"); the checks below use defaults")
	default:
		return ok(checkNameConfigFile, "none found; the checks below use defaults")
	}
}

// oneLine collapses every run of whitespace, newlines included, to a
// single space. A YAML decode error spans lines, and the report prints
// one line per check.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
