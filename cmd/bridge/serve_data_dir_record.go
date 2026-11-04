package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The data dir's record (backlog B651). A NAS that starts the bridge before
// its volumes are mounted shows serve a data dir that is not there (a
// directory on the volume) or the bare mount point, empty. Serve then made a
// fresh data dir in its place (prepareServeDataDir's MkdirAll), minted a new
// TLS pair, and opened an empty token store and an empty database there, on
// the system disk under the mount point, which the mount then hid: every
// paired device's pin failed and its token was refused. Nothing in the data
// dir can say it was there before, since the whole of it may be on the volume
// that is missing, and serve may not write bridge.yaml (B506's writers), so
// the record is a file beside the config, in the directory a write of the
// config lands in (fsutil.ResolveParent, where the config's lock is), named
// after it: `.bridge.yaml.datadir` beside bridge.yaml, holding the data dir's
// path. Serve's is beside the file its boot read of the config was of
// (serveDataDirRecord), for its check and its write alike, never beside the
// file the config's name opens by then.
//
// Serve writes it once its database is open and its lock file is there
// (recordServeDataDir), and, before it prepares the data dir, refuses one the
// record names that holds neither its database nor its lock file
// (checkServeDataDirRecord): a data dir that is not the one serve served
// from, which is what a volume not mounted looks like. The refusal exits 1,
// and the service manager starts serve again (systemd's Restart=always,
// launchd's KeepAlive, the SCM's recovery actions, a container's restart
// policy) until the volume is mounted; serve never waits for it.
//
// A first run has no record: a fresh install, and bridge init, which makes
// the data dir and its TLS pair and no database, start as they did, and the
// first serve writes the record. A database removed on purpose with the lock
// file kept (the local fixture's reset) is the data dir serve served from,
// and is made again as before. A data dir removed whole, or moved without the
// config being told, is refused, saying to remove the record. A config
// directory serve cannot write keeps no record, said once a start, and serve
// goes on as before. The record is plain file I/O, the same on Windows. In a
// container whose config and data dir are on one volume (the image's /data),
// the record is on that volume too, and a missing volume is a missing config.
const dataDirRecordSuffix = ".datadir"

// dataDirRecordMax is the most of a record readDataDirRecord reads.
const dataDirRecordMax = 64 << 10

// dataDirRecord is what the record holds.
type dataDirRecord struct {
	// DataDir is the data dir serve opened its database in, as its config
	// resolved it.
	DataDir string `json:"dataDir"`
}

// dataDirRecordPath is where the record of the config at cfgFile is: beside
// the file the OS opens for the name now, as the config's lock is
// (config.LockFile), named after it. bridge doctor asks it as it reads the
// config; serve keeps to the file its boot read was of (serveDataDirRecord).
func dataDirRecordPath(cfgFile string) (string, error) {
	dir, name, err := fsutil.ResolveParent(cfgFile)
	if err != nil {
		return "", err
	}
	return dataDirRecordBeside(filepath.Join(dir, name)), nil
}

// dataDirRecordBeside is the record of the config file file: beside it, named
// after it.
func dataDirRecordBeside(file string) string {
	return filepath.Join(filepath.Dir(file), "."+filepath.Base(file)+dataDirRecordSuffix)
}

// serveDataDirRecord is serve's record: beside the file its boot read of the
// config was of (config.LoadFileAt), never the file the config's name opens by
// the time serve checks the record or writes it. A link at the name can be
// repointed in between (a dotfiles restow, a home-manager switch during
// serve's boot), and serve then checked another config's record for its data
// dir and wrote its data dir into that config's record (Codex on #1397). ""
// where the boot read resolved no file: serve keeps no record then.
func serveDataDirRecord(boot config.BootFile) string {
	file, ok := boot.File()
	if !ok {
		return ""
	}
	return dataDirRecordBeside(file)
}

// readDataDirRecord answers the data dir the record at path names, "" for no
// record, or one that does not read as one: such a record names nothing, and
// the next serve that opens its database writes it again.
func readDataDirRecord(path string) string {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, dataDirRecordMax))
	if err != nil {
		return ""
	}
	var r dataDirRecord
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.DataDir
}

// dataDirGoneError is serve's refusal of a data dir its record names and that
// holds neither its database nor its lock file (checkServeDataDirRecord).
type dataDirGoneError struct {
	// dataDir is the data dir, and record the record that names it.
	dataDir, record string
}

func (e *dataDirGoneError) Error() string {
	return e.what() + ": when it is on a volume that is mounted at boot, the volume is not mounted yet. " +
		"bridge serve does not make an empty data directory in its place, and its service manager starts it again; " +
		"if the data directory was removed or moved on purpose, remove " + e.record + " and start bridge serve again"
}

// what is the refusal's first sentence, which bridge doctor's data-dir line
// says too (dataDirCheck).
func (e *dataDirGoneError) what() string {
	return fmt.Sprintf("the data directory %s held this bridge's database, and holds neither %s nor %s now",
		e.dataDir, filepath.Base(manifest.DefaultDBPath(e.dataDir)), serveLockFileName)
}

// fix is the data-dir line's hint for the refusal.
func (e *dataDirGoneError) fix() string {
	return "mount the volume it is on (serve's service manager starts it again until it is there); " +
		"if the data directory was removed or moved on purpose, remove " + e.record
}

// checkServeDataDirRecord refuses the data dir dataDir, with a
// *dataDirGoneError, when the record at path names it and it holds neither
// its database nor its lock file: neither is ever removed by the bridge, and
// a serve that opened its database there took its lock first, so a data dir
// holding neither is not the one serve served from. Asked before serve
// prepares the data dir, so a data dir that is not there is refused and never
// made. Anything else is no refusal: no record (path "" included), or one
// that does not read or names another data dir (the config's dataDir
// changed), a database or a lock file there, and an error other than an
// entry's absence, which serve's preparation, its lock and its store answer.
func checkServeDataDirRecord(path, dataDir string) error {
	if path == "" {
		return nil
	}
	recorded := readDataDirRecord(path)
	if recorded == "" || filepath.Clean(recorded) != filepath.Clean(dataDir) {
		return nil
	}
	for _, p := range []string{manifest.DefaultDBPath(dataDir), filepath.Join(dataDir, serveLockFileName)} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	return &dataDirGoneError{dataDir: dataDir, record: path}
}

// recordServeDataDir writes the record at path naming dataDir, once serve's
// database is open there and its lock file is too, and only when the record
// does not name it already. No path (a boot read that resolved no file), or a
// record serve cannot write (a config directory it may not write), is said
// once, at Warn, and the start goes on: a later start cannot then tell that
// data dir's volume missing.
func recordServeDataDir(path, dataDir string) {
	if path == "" {
		logger.Warn("could not record the data directory beside the config: serve's boot read resolved no file for the config's name, so a later start cannot tell the data directory's volume missing from a fresh install",
			"dataDir", dataDir)
		return
	}
	if recorded := readDataDirRecord(path); recorded != "" && filepath.Clean(recorded) == filepath.Clean(dataDir) {
		return
	}
	for _, p := range []string{manifest.DefaultDBPath(dataDir), filepath.Join(dataDir, serveLockFileName)} {
		if _, err := os.Lstat(p); err != nil {
			// No record for a data dir whose lock file could not be made: the
			// refusal reads its absence as a data dir serve never served from.
			return
		}
	}
	b, err := json.Marshal(dataDirRecord{DataDir: dataDir})
	if err == nil {
		err = atomicwrite.WriteBytes(path, append(b, '\n'), ".bridge-datadir-*.tmp")
	}
	if err != nil {
		logger.Warn("could not record the data directory beside the config: a later start cannot tell the data directory's volume missing from a fresh install",
			"record", path, "dataDir", dataDir, "err", err)
	}
}
