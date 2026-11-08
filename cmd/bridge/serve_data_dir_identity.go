package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The data dir's identity (backlog B741). B651's record named the data dir
// serve served from, and serve took the data dir at that path for it
// wherever it held its database or its lock file. A lock file is made before
// the database opens, so a start on the bare mount point that failed between
// the two, before any record was written, left one there, which every start
// after the volume went took for the data dir (gap 2); and a database a
// command made there before B727 read the same way. The record now holds an
// identity too, a random id minted once, which a marker in the data dir holds
// (data-dir-id.json), and the check refuses a data dir the record names whose
// marker is not there while the record holds an identity, holds another, or
// does not read.
//
// Only serve writes the marker, once its database is open (recordServeDataDir),
// and before the record gains the identity, durably: a record holding an
// identity over a data dir whose marker a crash took would be refused at every
// start after it. A command that opens the store makes no marker: its data dir
// gains one at the next serve. A marker or a record that cannot be written
// never refuses the start that wrote it: it goes on, says so, and the record
// holds no identity, as before B741.
//
// A record written before B741 holds none, and the first start of B741 takes
// up the data dir it finds at its path, whatever it holds, so that a database
// removed on purpose with its lock file kept (the local fixture's reset) still
// starts: gap 2 is closed from the first marker on. That start trusts the
// database it finds, which may be the bare mount point's where a start before
// the volume was mounted made one there (gap 1), and nothing outside the volume
// can tell an install whose volume is away from a fresh one, so the real
// volume, once mounted, is refused for holding no identity, in words that say
// so and how to go on.

// dataDirMarkerName is the marker's name in the data dir.
const dataDirMarkerName = "data-dir-id.json"

// dataDirMarkerMax is the most of a marker readDataDirMarker reads.
const dataDirMarkerMax = 4 << 10

// dataDirMarker is what the marker holds.
type dataDirMarker struct {
	// ID is the data dir's identity (isDataDirIdentity).
	ID string `json:"id"`
}

// errDoesNotRead is why a record or a marker that is there is refused when
// its bytes do not read as one: not JSON, a field missing or out of shape,
// or more of it than one ever is.
var errDoesNotRead = errors.New("does not read as one")

// mintDataDirIdentity is a new identity: 16 random bytes in lower-case hex.
// crypto/rand's Read never fails.
func mintDataDirIdentity() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// isDataDirIdentity reports whether id is an identity as mintDataDirIdentity
// makes one.
func isDataDirIdentity(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// readSmallFile reads the file at path, opened as a file (fsutil.OpenAsFile:
// a named pipe is refused without waiting), whole, refusing one of more than
// max bytes with errDoesNotRead. Its errors are the open's or the read's.
func readSmallFile(path string, max int64) ([]byte, error) {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	switch {
	case err != nil:
		return nil, err
	case int64(len(b)) > max:
		return nil, errDoesNotRead
	}
	return b, nil
}

// readDataDirMarker reads the marker at path, which is there: its identity,
// or why it does not read as a marker (the open's or the read's error, or
// errDoesNotRead).
func readDataDirMarker(path string) (string, error) {
	b, err := readSmallFile(path, dataDirMarkerMax)
	if err != nil {
		return "", err
	}
	var m dataDirMarker
	if json.Unmarshal(b, &m) != nil || !isDataDirIdentity(m.ID) {
		return "", errDoesNotRead
	}
	return m.ID, nil
}

// dataDirFindings is what the check of the record asks of the data dir it
// names, by lookups that write nothing: its identity, and which of its
// database and its lock file it holds.
type dataDirFindings struct {
	// identity is the identity its marker holds, "" where it holds no marker;
	// unreadable is why a marker it holds does not read as one.
	identity   string
	unreadable error
	// held names, of bridge.db and server.lock, each it holds.
	held []string
	// uninspected is an error other than an entry's absence met on the way to
	// any of them (a data dir this user may not search, a regular file where
	// a directory goes): what the data dir holds is not known, and serve's
	// preparation, its lock and its store answer it, as before B741.
	uninspected error
}

// inspectDataDir is what the data dir dataDir holds, by an Lstat of each of
// its database, its lock file and its marker, and a read of a marker there.
func inspectDataDir(dataDir string) dataDirFindings {
	var f dataDirFindings
	for _, name := range []string{filepath.Base(manifest.DefaultDBPath(dataDir)), serveLockFileName} {
		switch _, err := os.Lstat(filepath.Join(dataDir, name)); {
		case err == nil:
			f.held = append(f.held, name)
		case !errors.Is(err, fs.ErrNotExist):
			f.uninspected = err
		}
	}
	marker := filepath.Join(dataDir, dataDirMarkerName)
	switch _, err := os.Lstat(marker); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		f.uninspected = err
	default:
		f.identity, f.unreadable = readDataDirMarker(marker)
	}
	return f
}

// dataDirEvidence is what the data dir held before serve's preparation and
// its lock made anything there, which the end of a refusal reads
// (dataDirBack): whether it held its database or its lock file, and the
// identity its marker held, "" for none. The marker is no file either makes.
type dataDirEvidence struct {
	held     bool
	identity string
}

// dataDirEvidenceOf is what the data dir dataDir holds now.
func dataDirEvidenceOf(dataDir string) dataDirEvidence {
	f := inspectDataDir(dataDir)
	return dataDirEvidence{held: len(f.held) > 0, identity: f.identity}
}

// dataDirMarkerMode is the marker's mode: it holds no secret, and whoever may
// read the data dir's database may read it, so that a command or bridge
// doctor run by a member of the service's group reads the identity of a
// healthy data dir (B741's round 2, the Claude API on #1441: it was the
// owner's alone, and every such run took the data dir for one whose identity
// does not read).
const dataDirMarkerMode = 0o644

// markerDurability is how writeDataDirMarker makes the marker durable before
// the record gains its identity, each step one a boot test stands in for to
// record it or fail it (dataDirRecording.durability): the staged marker's
// bytes synced, the rename that puts it in place, and the marker synced where
// it is, its own bytes and metadata and then its directory's entries
// (syncInPlace), which the adoption of a marker a start before left runs too
// (recordServeDataDir). A nil step is its own.
type markerDurability struct {
	// syncStaged syncs the staged marker's bytes, before the rename.
	syncStaged func(f *os.File) error
	// rename puts the staged marker in place: the rename and its retries
	// alone (atomicwrite.RenameWithRetryUnsynced), the barrier syncInPlace's.
	rename func(from, to string) error
	// syncFile syncs the marker at path where it is, through a handle of its
	// own (syncMarkerFile).
	syncFile func(path string) error
	// syncDir syncs the entries of the directory that holds path
	// (fsutil.SyncParentDir).
	syncDir func(path string) error
}

// steps is d with each nil step its own.
func (d markerDurability) steps() markerDurability {
	if d.syncStaged == nil {
		d.syncStaged = (*os.File).Sync
	}
	if d.rename == nil {
		d.rename = atomicwrite.RenameWithRetryUnsynced
	}
	if d.syncFile == nil {
		d.syncFile = syncMarkerFile
	}
	if d.syncDir == nil {
		d.syncDir = fsutil.SyncParentDir
	}
	return d
}

// syncInPlace makes the marker at path durable where it is, once renamed
// there: its own bytes and metadata (syncFile), then its directory's entries
// (syncDir; nothing on Windows, where a directory cannot be synced), a
// directory fsync the filesystem cannot make (EINVAL, ENOTSUP, EOPNOTSUPP),
// as for the updater's marker, taken as made.
func (d markerDurability) syncInPlace(path string) error {
	d = d.steps()
	if err := d.syncFile(path); err != nil {
		return err
	}
	if err := d.syncDir(path); err != nil && !cannotSyncADirectory(err) {
		return err
	}
	return nil
}

// writeDataDirMarker writes the marker in the data dir dataDir holding id,
// durably before it answers (markerDurability's steps): staged beside it,
// readable by its group and others (dataDirMarkerMode), its bytes synced,
// renamed into place, and synced where it is, the barrier's failure the
// write's, so that the record beside the config, which gains the identity
// after it, never outlives a crash that took the marker. A failure after the
// rename leaves the marker in place, and the record without its identity: the
// next start takes it up (recordServeDataDir), once its own barrier has
// answered. It makes no directory: the data dir is there, its database open.
// Run as root, the marker takes the data dir's owner (fsutil.KeepOwner), so
// the service can read it.
func writeDataDirMarker(dataDir, id string, d markerDurability) (err error) {
	d = d.steps()
	b, err := json.Marshal(dataDirMarker{ID: id})
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, dataDirMarkerName)
	tmp, err := os.CreateTemp(dataDir, ".data-dir-id-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// LIFO: the file is closed before the Remove above, which Windows needs.
	defer func() { _ = tmp.Close() }()
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return err
	}
	if err := tmp.Chmod(dataDirMarkerMode); err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := d.syncStaged(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := d.rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	if err := d.syncInPlace(path); err != nil {
		return fmt.Errorf("the marker is in place and may not outlive a crash: %w", err)
	}
	return nil
}

// syncMarkerFile syncs the marker at path where it is, its bytes and its own
// metadata, through a handle of its own, opened for writing since on Windows
// Sync is FlushFileBuffers, which needs that, and which commits the file's
// metadata, the rename that put it there among it, to NTFS's journal before
// it answers (B741's round 2, the Claude API on #1441: the rename alone left
// that to the journal's own flush, so a record on another volume could reach
// its disk first).
func syncMarkerFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// cannotSyncADirectory reports whether err is a directory fsync's answer that
// the filesystem cannot make one (internal/updater's barrier reads the same).
func cannotSyncADirectory(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
