package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// playerStorageCheckName is the doctor's line on the storage under the data
// directory of the bridge's own player.
const playerStorageCheckName = "player-storage"

// The two things the player-storage line reads of the host, as seams so a
// test stands in for a Linux box and decides what its storage answers: the
// host's platform, and the medium check the player's strict writer makes
// (atomicwrite.InspectMedium). Production never changes them, and a test
// that does must not call t.Parallel.
var (
	playerStorageGOOS    = runtime.GOOS
	inspectPlayerStorage = atomicwrite.InspectMedium
)

// doctorPlayerStorage is doctor.Deps.PlayerStorage for cfg, the one source
// both bridge doctor and the console's doctor read (buildDoctorDepsFor): its
// line, or none where cfg does not ask for the player.
func doctorPlayerStorage(cfg *config.Config) func(context.Context) []doctor.Check {
	return func(context.Context) []doctor.Check {
		if c, ok := playerStorageCheck(cfg, playerStorageGOOS, inspectPlayerStorage); ok {
			return []doctor.Check{c}
		}
		return nil
	}
}

// playerStorageCheck is the doctor's word on the storage under the data
// directory, where the bridge's own player keeps its identity and the
// reservations of its IDs and tokens (B603): the medium check the player's
// strict writer makes as the player starts, its own verdict
// (atomicwrite.InspectMedium), with the storage it names, Volumio's data
// partition on Volumio, ok; or that the player will not start, and why, a
// fail. A medium it takes is no word that the player will start, which the
// line never says: serve checks the directories' syncs, the DAC's binding
// and its other gates as it starts it. A player cfg leaves off has no line.
// Off Linux, where the player never runs, a player cfg asks for is not
// checked, ok; on Linux, one cfg's own refusal turns off is not checked, a
// warn, naming that refusal (which names no value); a data directory the
// inspection finds no entry at is judged by the nearest entry of its path
// that is there (dataDirNotThere): a link to nothing there is a fail, since
// serve cannot make the directory through it, and anything else is not
// checked, ok, since serve makes it; and one the inspection cannot read is
// not checked, a warn (dataDirNotChecked), never ok.
func playerStorageCheck(cfg *config.Config, goos string, inspect func(string) (atomicwrite.Medium, error)) (doctor.Check, bool) {
	line := func(status doctor.Status, summary, hint string) (doctor.Check, bool) {
		return doctor.Check{Name: playerStorageCheckName, Status: status, Summary: summary, Hint: hint}, true
	}
	on, why := cfg.PlayerEnabled(goos)
	switch {
	case goos != "linux":
		if !cfg.Output.Player.Enabled {
			return doctor.Check{}, false
		}
		return line(doctor.OK, "not checked: the bridge's own player runs on Linux alone, and this is "+goos, "")
	case !on && why == nil:
		return doctor.Check{}, false
	case !on:
		return line(doctor.Warn, "not checked: the player is off: "+why.Error(),
			"fix output.player in bridge.yaml, and its storage is checked once the config runs the player")
	}
	dir := cfg.DataDir
	m, err := inspect(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dataDirNotThere(dir, line)
	case err != nil:
		return dataDirNotChecked(dir, err, line)
	case errors.Is(m.Err, atomicwrite.ErrOverlayBind):
		return line(doctor.Fail, "the player will not start: "+m.Err.Error(),
			"put the data directory (dataDir in bridge.yaml) on the overlay's own mount, or on a filesystem that is not an overlay")
	case m.Err != nil:
		return line(doctor.Fail, "the player will not start: "+m.Err.Error(),
			"put the data directory (dataDir in bridge.yaml) on storage that keeps what is written to it through a reboot")
	}
	storage := m.Storage
	if storage == "" {
		storage = "storage in which the check finds no volatile layer"
	}
	return line(doctor.OK, dir+" is on "+storage+"; serve checks the rest as it starts the player", "")
}

// storageLine is playerStorageCheck's maker of its line.
type storageLine func(status doctor.Status, summary, hint string) (doctor.Check, bool)

// dataDirNotThere is the line for a data directory the inspection finds no
// entry at (fs.ErrNotExist), which its resolution of every link in the path
// answers for a link that leads to nothing as well (CodeRabbit on #1368).
// From dir up, the nearest entry of its path that is there, by os.Lstat (the
// kernel resolving the entries above it), is judged. A symbolic link there
// that leads to nothing (os.Stat answers fs.ErrNotExist), however many links
// it goes through, is a fail naming it: serve's MkdirAll stops at that entry
// (EEXIST), so serve takes no lock on the data directory and starts no
// player. Anything else there leaves only the components below it to make,
// which serve's MkdirAll makes: "not there yet", ok. An Lstat or a Stat on
// the way that fails for another reason than the entry's absence is not
// checked (dataDirNotChecked), never ok, and so is a path whose root is not
// there.
func dataDirNotThere(dir string, line storageLine) (doctor.Check, bool) {
	for p := dir; ; {
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if up := filepath.Dir(p); up != p {
				p = up
				continue
			}
			return dataDirNotChecked(dir, err, line)
		case err != nil:
			return dataDirNotChecked(dir, err, line)
		case info.Mode()&fs.ModeSymlink != 0:
			switch _, err := os.Stat(p); {
			case errors.Is(err, fs.ErrNotExist):
				return line(doctor.Fail, "the player will not start: "+p+" is a symbolic link to nothing, and serve cannot make "+dir+" through it",
					"point the link at a directory, or set dataDir in bridge.yaml to one")
			case err != nil:
				return dataDirNotChecked(dir, err, line)
			}
		}
		return line(doctor.OK, "not checked: the data directory "+dir+" does not exist yet; bridge serve makes it, and checks its storage as it starts the player", "")
	}
}

// dataDirNotChecked is the line for the data directory dir whose
// inspection, or the walk up its path, failed for another reason than an
// entry's absence: not checked, a warn naming dir and the error, never ok,
// since filepath.EvalSymlinks answers an entry on the way that is no
// directory with a bare ENOTDIR, which names nothing. A permission error is
// this user's, and the service's user may read what this one cannot; any
// other, a link cycle (which EvalSymlinks answers as too many links) or an
// entry on the way that is no directory, stands in the way of the path.
func dataDirNotChecked(dir string, err error, line storageLine) (doctor.Check, bool) {
	summary := "not checked: the data directory " + dir + " cannot be inspected: " + err.Error()
	if errors.Is(err, fs.ErrPermission) {
		return line(doctor.Warn, summary, "run bridge doctor as the user the service runs as, who can read the data directory")
	}
	return line(doctor.Warn, summary, "mend what the error names on the way to the data directory, or set dataDir in bridge.yaml to a directory serve can make or use")
}
