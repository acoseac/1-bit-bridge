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
	// LoadErr is why Path did not load. Nil when it loaded, and when
	// there was no Path.
	LoadErr error
}

// checkConfigFile names the bridge.yaml the report graded, and FAILS on
// one that is there but does not load.
//
// A MISSING config is not an error: doctor runs before `bridge init` has
// written one, so "none found" is ok, and it names where the caller
// looked. A config that EXISTS and does not load is a different fact.
// Every command that reads it refuses it, `bridge serve` will not start
// with it, and the checks beside this one ran on defaults. Until this
// check the load error was dropped without a word: a typo'd key read
// "all clear", exit 0, graded against ports the file does not name,
// while `bridge status` exited 2 on the same file.
//
// A permission failure is a WARN, not a fail. It is a fact about the
// doctor run rather than the file: on the public-mode layout the operator
// is not the service user, which is also why the cert checks do not grade
// a key they cannot read.
func checkConfigFile(_ context.Context, d Deps) Check {
	c := d.ConfigFile
	switch {
	case c == nil:
		return ok(checkNameConfigFile, "no config lookup — check skipped")
	case c.LoadErr != nil && errors.Is(c.LoadErr, fs.ErrPermission):
		return warn(checkNameConfigFile,
			c.Path+" is not readable by this user: "+oneLine(c.LoadErr.Error()),
			"run `bridge doctor` as the user the bridge runs as to grade this install; the checks below ran on defaults")
	case c.LoadErr != nil:
		return fail(checkNameConfigFile,
			c.Path+" does not load: "+oneLine(c.LoadErr.Error()),
			"every command that reads this file refuses it, and `bridge serve` will not start with it; fix it and re-run `bridge doctor` (the checks below ran on defaults)")
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
