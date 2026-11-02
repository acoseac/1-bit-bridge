package main

import (
	"context"
	"errors"
	"io/fs"
	"runtime"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
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
// warn, naming that refusal (which names no value). A data directory the
// inspection finds no directory at, or none on its way to (a root that is
// there and is no directory, atomicwrite.ErrRootNotADirectory; no entry
// there, fs.ErrNotExist; an entry on the way that is no directory, a bare
// ENOTDIR), is judged by the walk up its path the data-dir line takes
// (dataDirOnItsWay), in the same words: an entry in the way, a regular file,
// a link to one or a link to nothing, at the data directory or above it, is a
// fail saying bridge serve will not start, since serve refuses a data
// directory it cannot prepare (B606; an ancestor that is a regular file, or a
// link to one, since B659), and a data directory not made yet below a
// directory is not checked, ok, since serve makes it; and one the inspection
// or the walk cannot read is not checked, a warn (dataDirNotChecked), never
// ok, nor a fail, since the service's user may read it.
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
	case errors.Is(err, atomicwrite.ErrRootNotADirectory), errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return dataDirOnItsWay(dir, err, line)
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

// dataDirOnItsWay is the line for a data directory the inspection found no
// directory at, or none on its way to (inspectErr): the walk up its path the
// data-dir line takes (fsutil.BlockingEntry) judges it, so the two lines say
// one thing of it. The inspection resolves every link in the path, so it
// answers a data directory below a link to nothing as fs.ErrNotExist, as it
// does one not made yet (CodeRabbit on #1368), and one below a regular file
// or a link to one with a bare ENOTDIR that names no entry; the walk tells
// them apart. An entry in the way, a regular file, a link to one or a link to
// nothing, at the data directory or above it, is the data-dir line's fail
// (dataDirBlocked), which serve refuses (B606). A data directory not made yet
// below a directory or a link to one is "not there yet", ok, since serve's
// preparation (prepareServeDataDir) makes it. A walk that fails is not
// checked (dataDirNotChecked), and so is one that finds nothing in the way of
// what the inspection could not reach (the path changed in between), named by
// the inspection's error.
func dataDirOnItsWay(dir string, inspectErr error, line doctorLine) (doctor.Check, bool) {
	nearest, inTheWay, err := fsutil.BlockingEntry(dir)
	switch {
	case err != nil:
		return dataDirNotChecked(dir, err, line)
	case inTheWay != nil:
		return dataDirBlocked(dir, inTheWay, line)
	case nearest == dir || !errors.Is(inspectErr, fs.ErrNotExist):
		return dataDirNotChecked(dir, inspectErr, line)
	}
	return line(doctor.OK, "not checked: the data directory "+dir+" does not exist yet; bridge serve makes it, and checks its storage as it starts the player", "")
}
