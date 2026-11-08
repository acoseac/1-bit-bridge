package main

import (
	"context"
	"errors"
	"io/fs"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// dataDirCheckName is the doctor's line on the data directory as bridge serve
// prepares it (B659).
const dataDirCheckName = "data-dir"

// The fixes for an entry in the data directory's way, the data-dir line's and
// the player-storage line's: one that is no link, and a link, to an entry
// that is no directory or to nothing.
const (
	moveItAsideHint   = "move it aside, or set dataDir in bridge.yaml to a directory serve can make or use"
	linkToNothingHint = "point the link at a directory, or set dataDir in bridge.yaml to one"
)

// doctorLine makes one of the doctor's lines of one name, true for a line
// made: the data-dir line's maker, and playerStorageCheck's, whose false is
// no line.
type doctorLine func(status doctor.Status, summary, hint string) (doctor.Check, bool)

// doctorDataDir is doctor.Deps.DataDirPreparation for a config at cfgFile
// that loaded, whose data directory is dir: the one source bridge doctor and
// the console's doctor read (buildDoctorDepsFor).
func doctorDataDir(cfgFile, dir string) func(context.Context) doctor.Check {
	return func(context.Context) doctor.Check {
		c, _ := dataDirCheck(cfgFile, dir)
		return c
	}
}

// doctorDataDirNotChecked is doctor.Deps.DataDirPreparation for a config the
// lookup found, or was named, and could not load (lookup.LoadErr): no data
// directory it names is known, so the line is "not checked", a warn, saying
// why in config-file's terms (doctor.ConfigFile.NotGradedReason), never a
// verdict on a data directory it guessed (Codex's check on the brief).
func doctorDataDirNotChecked(lookup *doctor.ConfigFile) func(context.Context) doctor.Check {
	return func(context.Context) doctor.Check {
		return doctor.Check{Name: dataDirCheckName, Status: doctor.Warn,
			Summary: "not checked: " + lookup.NotGradedReason("the data directory"),
			Hint:    "config-file above says why; the data directory is checked once the config loads"}
	}
}

// dataDirCheck is the data-dir line on the data directory dir: whether bridge
// serve's preparation (prepareServeDataDir) can make or use it, from what
// stands on its path, which os.Lstat and os.Stat read and nothing writes
// (fsutil.BlockingEntry, the walk serve's refusal names the entry in its way
// from). Each shape serve refuses, a regular file, a link to one or a link to
// nothing at the data directory or above it, is a fail saying bridge serve
// will not start, naming the entry in the way, with its fix
// (dataDirBlocked). A directory there, or a link to one, is ok, and so is a
// data directory not made yet below one, which serve makes. A walk that
// cannot be read for another reason than an entry's absence (a permission
// error, a link cycle) is not checked, a warn (dataDirNotChecked), never ok
// nor a fail: the service's user may read what this one cannot, and serve's
// refusal of such a path is no shape it names. Whether the data directory can
// be written is not asked, as serve's preparation does not ask it.
//
// Before all of that, the check serve makes ahead of its preparation
// (checkServeDataDirRecord, backlog B651): a data directory the record beside
// the file the config's name at cfgFile opens (dataDirRecordPath) names that
// is not the one serve served from, whether it is there or not (it holds
// neither its database nor its lock file, or since B741 not the identity the
// record holds), and a record or a marker that does not read, is a fail in
// the refusal's words; but a record or a marker this user may not read, which
// the user the service runs as may, is not checked, a warn, as a data
// directory it may not inspect is.
func dataDirCheck(cfgFile, dir string) (doctor.Check, bool) {
	line := func(status doctor.Status, summary, hint string) (doctor.Check, bool) {
		return doctor.Check{Name: dataDirCheckName, Status: status, Summary: summary, Hint: hint}, true
	}
	var gone *dataDirGoneError
	if record, err := dataDirRecordPath(cfgFile); err == nil && errors.As(checkServeDataDirRecord(record, dir), &gone) {
		return line(gone.doctorLine())
	}
	nearest, inTheWay, err := fsutil.BlockingEntry(dir)
	switch {
	case err != nil:
		return dataDirNotChecked(dir, err, line)
	case inTheWay != nil:
		return dataDirBlocked(dir, inTheWay, line)
	case nearest != dir:
		return line(doctor.OK, dir+" does not exist yet; bridge serve makes it", "")
	}
	return line(doctor.OK, dir+" is a directory", "")
}

// dataDirBlocked is the fail for the data directory dir that inTheWay, the
// entry of its path that is there and is no directory, stands in the way of:
// bridge serve will not start, which serve, refusing a data directory it
// cannot prepare, makes true (B606). The data directory itself is named as
// such, a link to nothing as one serve cannot make the data directory
// through, an entry above it that is a link as one serve cannot make it
// through, and one that is no link as one it cannot make it below; a link's
// fix points the link at a directory, any other entry's moves it aside.
func dataDirBlocked(dir string, inTheWay *fsutil.EntryInTheWay, line doctorLine) (doctor.Check, bool) {
	const wont = "bridge serve will not start: "
	switch {
	case inTheWay.Link && inTheWay.Kind == "":
		return line(doctor.Fail, wont+inTheWay.String()+", and serve cannot make "+dir+" through it", linkToNothingHint)
	case inTheWay.Entry == dir && inTheWay.Link:
		return line(doctor.Fail, wont+"the data directory "+inTheWay.String(), linkToNothingHint)
	case inTheWay.Entry == dir:
		return line(doctor.Fail, wont+"the data directory "+inTheWay.String(), moveItAsideHint)
	case inTheWay.Link:
		return line(doctor.Fail, wont+inTheWay.String()+", and serve cannot make "+dir+" through it", linkToNothingHint)
	}
	return line(doctor.Fail, wont+inTheWay.String()+", and serve cannot make "+dir+" below it", moveItAsideHint)
}

// dataDirNotChecked is the line for the data directory dir whose inspection,
// or the walk up its path, failed for another reason than an entry's absence:
// not checked, a warn naming dir and the error, never ok. A permission error is
// this user's, and the service's user may read what this one cannot; any
// other, a link cycle (which filepath.EvalSymlinks answers as too many links)
// or an entry on the way that cannot be read, stands in the way of the path.
func dataDirNotChecked(dir string, err error, line doctorLine) (doctor.Check, bool) {
	summary := "not checked: the data directory " + dir + " cannot be inspected: " + err.Error()
	if errors.Is(err, fs.ErrPermission) {
		return line(doctor.Warn, summary, "run bridge doctor as the user the service runs as, who can read the data directory")
	}
	return line(doctor.Warn, summary, "mend what the error names on the way to the data directory, or set dataDir in bridge.yaml to a directory serve can make or use")
}
