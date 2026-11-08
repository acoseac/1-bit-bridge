package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
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
// path as its config gave it and where that led, and since B741 its identity,
// which a marker in the data dir holds too, and how each folder on its way
// compared names (dataDirRecord, serve_data_dir_identity.go). Serve's is
// beside the file its boot read of the config was of (serveDataDirRecord),
// for its check and its write alike, never beside the file the config's name
// opens by then.
//
// Serve writes it once its database is open and its lock file is there
// (recordServeDataDir), and, before it prepares the data dir, refuses one the
// record names that is not the one it served from (checkServeDataDirRecord):
// one that holds neither its database nor its lock file, which is what a
// volume not mounted looks like, and since B741 one that does not hold the
// identity the record holds, and a record or a marker that does not read. The
// refusal exits 1, and the service manager starts serve again (systemd's
// Restart=always, launchd's KeepAlive, the SCM's recovery actions, a
// container's restart policy) until the volume is mounted; serve never waits
// for it.
//
// A first run has no record: a fresh install, and bridge init, which makes
// the data dir and its TLS pair and no database, start as they did, and the
// first serve writes the record. A database removed on purpose with the lock
// file kept (the local fixture's reset) is the data dir serve served from,
// its identity kept, and is made again as before. A data dir removed whole is
// refused, saying to remove the record; one moved without the config being
// told, saying to set dataDir where it is now, which the record does not name
// (onPurpose). A config directory serve cannot write keeps no record, said
// once a start, and serve goes on as before. The record is plain file I/O,
// the same on Windows. In a container whose config and data dir are on one
// volume (the image's /data), the record is on that volume too, and a missing
// volume is a missing config.
//
// Every other command that makes, opens or writes the data dir asks the same
// check first (refuseDataDirAway, backlog B727): a store open makes the data
// dir and an empty database, and a command that takes the data dir's lock its
// lock file, and either has the next serve take the bare mount point for the
// data dir it served from.
const dataDirRecordSuffix = ".datadir"

// dataDirRecordMax is the most of a record readDataDirRecord reads: past the
// record of any path an OS takes, both spellings at Windows' 32,767 UTF-16
// units included, each escaped as JSON escapes it at most (B741's round 2: at
// 64 KiB, a record keeping each folder's whole path outgrew it at a depth an
// ordinary path reaches, and one letter a folder no longer can).
const dataDirRecordMax = 2 << 20

// errLinkToNothing is why a record that is a link whose target is not there
// does not read (readDataDirRecord): never fs.ErrNotExist, the absence of a
// record, which it is not.
var errLinkToNothing = errors.New("a link to nothing")

// dataDirRecord is what the record holds: the data dir serve opened its
// database in, in both of its spellings (B651's fifth round, CodeRabbit on
// #1397). config.ResolvePath cleans a path and resolves no link, so the
// config's spelling can run through a link on the data dir's own volume, which
// nothing resolves once the volume is away; where it led is kept beside it,
// read while the volume was there. A record written before the fifth round
// holds DataDir alone, and reads as that one spelling.
type dataDirRecord struct {
	// DataDir is the data dir as its config gave it.
	DataDir string `json:"dataDir"`
	// Resolved is where DataDir led, through its links, when serve wrote the
	// record (fsutil.ResolveLinks); "" where it did not resolve.
	Resolved string `json:"resolved,omitempty"`
	// ID is the data dir's identity, which the marker in it holds too
	// (backlog B741): minted by the first start of B741 that opened its
	// database there, and kept by every start after it. "" in a record
	// written before B741, or by a start that could not write the marker.
	ID string `json:"id,omitempty"`
	// DataDirFolding and ResolvedFolding are how each folder on the way of
	// DataDir and of Resolved compared the names in it when serve wrote the
	// record (fsutil.FoldingOnTheWay), one letter a folder from the volume's
	// root down (fsutil.FormatFolding), so that the record grows with its
	// paths alone, however deep (backlog B741's gap 3, and its round 2): once
	// the volume is away, the folders below its mount point are gone, and a
	// probe answers for the folder left at the mount point instead, on the
	// volume above. A record written before B741 holds neither.
	DataDirFolding  string `json:"dataDirFolding,omitempty"`
	ResolvedFolding string `json:"resolvedFolding,omitempty"`
}

// names reports whether the record names dataDir, by either of its
// spellings: each is compared with dataDir as the directory they name, never
// as a string alone. A record serve wrote since B741 is compared by how its
// folders compared names (fsutil.SamePathAsRecorded: what resolves on either
// side resolved, and a spelling in another case taken for one where its
// folder was not read as case-sensitive, or where the record holds no folding
// of that spelling's folders), probing nothing; only a record written before
// B741, which holds neither an identity nor any folding, by a probe of what is
// there now (fsutil.SamePath), which with the volume away answers for the
// folder at the mount point.
func (r dataDirRecord) names(dataDir string) bool {
	byProbe := r.ID == "" && r.DataDirFolding == "" && r.ResolvedFolding == ""
	for _, s := range [...]struct{ spelling, folding string }{{r.DataDir, r.DataDirFolding}, {r.Resolved, r.ResolvedFolding}} {
		switch {
		case s.spelling == "":
		case byProbe && fsutil.SamePath(s.spelling, dataDir):
			return true
		case !byProbe && fsutil.SamePathAsRecorded(s.spelling, dataDir, fsutil.ParseFolding(s.folding)):
			return true
		}
	}
	return false
}

// errRecordTooLong is marshalDataDirRecord's refusal of a record longer than
// its reader takes, which no path an OS takes makes (dataDirRecordMax).
var errRecordTooLong = errors.New("longer than a record is read")

// marshalDataDirRecord is the record r as its file holds it, a line of JSON,
// never one its reader refuses for its size (readDataDirRecord), which would
// refuse every start after it. One whose folding takes it past that is
// written with each spelling's folding the one letter u, which the
// comparison reads as no folding of any folder, every one CaseUnknown: two
// spellings in another case taken for one, the direction that refuses, and
// nothing probed; and one longer than that without it is refused
// (errRecordTooLong), never written. Neither is a record of a path an OS
// takes: one letter a folder is less than half a path's bytes, and the paths
// themselves are under the reader's bound (dataDirRecordMax).
func marshalDataDirRecord(r dataDirRecord) ([]byte, error) {
	b, err := json.Marshal(r)
	if err == nil && len(b)+1 > dataDirRecordMax {
		r.DataDirFolding, r.ResolvedFolding = unknownFolding(r.DataDirFolding), unknownFolding(r.ResolvedFolding)
		b, err = json.Marshal(r)
	}
	switch {
	case err != nil:
		return nil, err
	case len(b)+1 > dataDirRecordMax:
		return nil, errRecordTooLong
	}
	return append(b, '\n'), nil
}

// unknownFolding is the one letter u in place of the folding letters, which
// says nothing of any folder, and "" for none.
func unknownFolding(letters string) string {
	if letters == "" {
		return ""
	}
	return fsutil.FormatFolding([]fsutil.CaseFolding{fsutil.CaseUnknown})
}

// same reports whether r holds exactly what o holds.
func (r dataDirRecord) same(o dataDirRecord) bool {
	return r == o
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

// readDataDirRecord reads the record at path: ok false where nothing is
// there, and an error for one that is there and does not read as one (the
// open's or the read's error, or errDoesNotRead: not JSON, no data dir, an
// identity out of shape), which is its own state (backlog B741), never the
// absence of a record a fresh install has: an identity it holds is not to be
// replaced unread. Until B741 such a record named nothing.
//
// Nothing is there only where the record's entry is not (os.Lstat): the open
// answers a link to nothing as it answers no file, and a link left beside the
// config to a record kept on a volume that is away is there and does not read
// (B741's round 2, Codex on #1441: it read as no record, so serve made the
// data dir in the bare mount point and renamed a fresh record over the link).
func readDataDirRecord(path string) (dataDirRecord, bool, error) {
	b, err := readSmallFile(path, dataDirRecordMax)
	if errors.Is(err, fs.ErrNotExist) {
		switch fi, lerr := os.Lstat(path); {
		case errors.Is(lerr, fs.ErrNotExist):
			return dataDirRecord{}, false, nil
		case lerr != nil:
			err = lerr
		case fi.Mode()&fs.ModeSymlink != 0:
			err = &fs.PathError{Op: "open", Path: path, Err: errLinkToNothing}
		}
	}
	if err != nil {
		return dataDirRecord{}, false, err
	}
	var r dataDirRecord
	if json.Unmarshal(b, &r) != nil || r.DataDir == "" || r.ID != "" && !isDataDirIdentity(r.ID) {
		return dataDirRecord{}, false, errDoesNotRead
	}
	return r, true, nil
}

// dataDirCause is why the check refuses the data dir its record names
// (dataDirGoneError), each said in words of its own.
type dataDirCause int

const (
	// dataDirNotThere: the data dir holds neither its database nor its lock
	// file, nor an identity (B651's refusal).
	dataDirNotThere dataDirCause = iota
	// noIdentity: it holds its database or its lock file, and the record holds
	// an identity its data dir holds no marker of (B741's gap 2).
	noIdentity
	// otherIdentity: it holds a marker of another identity than the record's.
	otherIdentity
	// identityUnreadable: it holds a marker that does not read.
	identityUnreadable
	// recordUnreadable: the record does not read, so no data dir is known to
	// be the one serve served from.
	recordUnreadable
)

// String is the cause's name in the note that serve said its refusal
// (dataDirRefusalNote.Cause).
func (c dataDirCause) String() string {
	switch c {
	case noIdentity:
		return "no-identity"
	case otherIdentity:
		return "other-identity"
	case identityUnreadable:
		return "identity-unreadable"
	case recordUnreadable:
		return "record-unreadable"
	}
	return "not-there"
}

// dataDirGoneError is serve's refusal of a data dir its record names that is
// not the one serve served from, or of every data dir where the record does
// not read (checkServeDataDirRecord), for its cause. B651 named it for its
// one cause then, a data dir gone with its volume; every cause since is said
// through it, so that bridge doctor and every command that asks the check
// (dataDirAway) refuse each alike.
type dataDirGoneError struct {
	// dataDir is the data dir as the config gives it, never as the record or
	// a resolution spells it: what bridge.yaml says, which is what the
	// operator looks for. record is the record that names it, where it is:
	// beside the file the config's name opens, through whatever links lead
	// there.
	dataDir, record string
	cause           dataDirCause
	// identity is the identity the record holds, "" for none: the note that
	// the refusal was said keeps it, and the data dir back is the one holding
	// it (dataDirBack).
	identity string
	// held names what the data dir holds without its identity (noIdentity).
	held []string
	// err is why the record or the marker does not read (recordUnreadable,
	// identityUnreadable).
	err error
}

func (e *dataDirGoneError) Error() string {
	switch e.cause {
	case noIdentity:
		return e.what() + ": when it is on a volume that is mounted at boot, the volume is not mounted yet, and what it holds was made " +
			"in the bare mount point. bridge serve does not take it for its data directory, and its service manager starts it again; " +
			e.onPurpose("serve", "start bridge serve again")
	case otherIdentity:
		return e.what() + ": when it is on a volume that is mounted at boot, another volume is mounted there. " +
			"bridge serve does not take it for its data directory, and its service manager starts it again; " +
			e.onPurpose("serve", "start bridge serve again")
	case identityUnreadable, recordUnreadable:
		return e.what() + ": bridge serve cannot tell it from another data directory, and does not start on it; " +
			e.onPurpose("serve", "start bridge serve again")
	}
	return e.what() + ": when it is on a volume that is mounted at boot, the volume is not mounted yet. " +
		"bridge serve does not make an empty data directory in its place, and its service manager starts it again; " +
		e.onPurpose("serve", "start bridge serve again")
}

// onPurpose is how to go on by hand (recovery), said to bridge cmd, then
// doing what then says.
func (e *dataDirGoneError) onPurpose(cmd, then string) string {
	return e.recovery(cmd) + " and " + then
}

// recovery is how to go on by hand from the refusal, ending in the record to
// remove. A data dir moved on purpose is named where it is now, which the
// record does not name, so nothing is refused and the next serve records it;
// one removed takes the record with it (backlog B727). Removing the record of
// a data dir that was moved would have the next command make an empty one in
// its place. A data dir holding no identity or another's is the one to serve
// from where the operator says so, by removing the record: the next serve
// takes up its identity, or gives it one (backlog B741). A marker that does
// not read is moved aside with it, and a record that does not read is removed
// with the data dir there, which serve records again; one this user may not
// read is read first as the service's user (readable), said to bridge cmd.
func (e *dataDirGoneError) recovery(cmd string) string {
	const moved = "if the data directory was moved on purpose, set dataDir in bridge.yaml to where it is now; "
	switch e.cause {
	case noIdentity:
		return moved + "if it was removed on purpose, or it is the data directory to serve from, remove " + e.record
	case otherIdentity:
		return moved + "if it was removed on purpose, or it is the data directory to serve from now, remove " + e.record
	case identityUnreadable:
		return e.readable(cmd) + moved + "if it was removed on purpose, or it is the data directory to serve from, move " + dataDirMarkerName +
			" aside and remove " + e.record
	case recordUnreadable:
		return e.readable(cmd) + "where the data directory is there, its volume mounted, remove " + e.record
	}
	return moved + "if it was removed on purpose, remove " + e.record
}

// readable is the advice for a record or a marker this user may not read,
// said to bridge cmd: serve, which its service manager runs as the service's
// user, is to have it made readable by that user; any other command, bridge
// doctor included, is to run as that user (B741's round 2: the advice to make
// readable what the service's user already reads told a command run by
// another user to change nothing), and to make it readable by that user if
// it is not, which this user cannot tell.
func (e *dataDirGoneError) readable(cmd string) string {
	switch {
	case !errors.Is(e.err, fs.ErrPermission):
		return ""
	case cmd == "serve":
		return "make it readable by the user the service runs as; "
	}
	return "run bridge " + cmd + " as the user the service runs as, and make it readable by that user if it is not; "
}

// unreadableHere reports whether the refusal is of a record or a marker this
// user may not read, which the user the service runs as may: bridge doctor
// says it is not checked (dataDirCheck), as for a data dir it may not
// inspect (B659).
func (e *dataDirGoneError) unreadableHere() bool {
	return (e.cause == recordUnreadable || e.cause == identityUnreadable) && errors.Is(e.err, fs.ErrPermission)
}

// doctorLine is bridge doctor's data-dir line on the refusal (dataDirCheck):
// a fail in serve's refusal's words with its fix, or, for a record or a
// marker this user may not read, not checked, a warn.
func (e *dataDirGoneError) doctorLine() (status doctor.Status, summary, hint string) {
	if e.unreadableHere() {
		return doctor.Warn, "not checked: " + e.what(), e.recovery("doctor")
	}
	return doctor.Fail, "bridge serve will not start: " + e.what(), e.fix()
}

// what is the refusal's first sentence, which bridge doctor's data-dir line
// says too (dataDirCheck).
func (e *dataDirGoneError) what() string {
	switch e.cause {
	case noIdentity:
		return "the data directory " + e.dataDir + " holds " + strings.Join(e.held, " and ") +
			" but not the identity of the data directory this bridge served from (" + dataDirMarkerName + ")"
	case otherIdentity:
		return "the data directory " + e.dataDir + " holds the identity of another data directory than the one this bridge served from (" +
			dataDirMarkerName + ")"
	case identityUnreadable:
		return "the data directory " + e.dataDir + " holds an identity (" + dataDirMarkerName + ") that " + e.why()
	case recordUnreadable:
		return "the data directory " + e.dataDir + " cannot be checked against the one this bridge served from: its record (" +
			e.record + ") " + e.why()
	}
	return fmt.Sprintf("the data directory %s held this bridge's database, and holds neither %s nor %s now",
		e.dataDir, filepath.Base(manifest.DefaultDBPath(e.dataDir)), serveLockFileName)
}

// why is what kept the record or the marker from reading, naming no path.
func (e *dataDirGoneError) why() string {
	if errors.Is(e.err, errDoesNotRead) {
		return "does not read as one"
	}
	return "cannot be read (" + fsutil.ErrorWithoutPath(e.err) + ")"
}

// fix is the data-dir line's hint for the refusal.
func (e *dataDirGoneError) fix() string {
	switch e.cause {
	case dataDirNotThere, noIdentity:
		return "mount the volume it is on (serve's service manager starts it again until it is there); " + e.recovery("doctor")
	case otherIdentity:
		return "mount the volume this bridge served from (serve's service manager starts it again until it is there); " + e.recovery("doctor")
	}
	return e.recovery("doctor")
}

// commandRefusal is the refusal of bridge cmd, a command other than serve, of
// the data dir its record names (dataDirAway): serve's first sentence, and how
// to go on.
func (e *dataDirGoneError) commandRefusal(cmd string) string {
	again := "run bridge " + cmd + " again"
	switch e.cause {
	case noIdentity:
		return e.what() + ": when it is on a volume that is mounted at boot, the volume is not mounted yet, and what it holds was made " +
			"in the bare mount point. bridge " + cmd + " makes and writes nothing there: mount the volume and run it again; " + e.onPurpose(cmd, again)
	case otherIdentity:
		return e.what() + ": when it is on a volume that is mounted at boot, another volume is mounted there. bridge " + cmd +
			" makes and writes nothing there: mount the volume this bridge served from and run it again; " + e.onPurpose(cmd, again)
	case identityUnreadable, recordUnreadable:
		return e.what() + ": bridge " + cmd + " cannot tell it from another data directory, and makes and writes nothing there; " +
			e.onPurpose(cmd, again)
	}
	return e.what() + ": when it is on a volume that is mounted at boot, the volume is not mounted yet. " +
		"bridge " + cmd + " makes and writes nothing there: mount the volume and run it again; " +
		e.onPurpose(cmd, again)
}

// dataDirAway is the check serve makes before it prepares its data dir
// (checkServeDataDirRecord), for a command other than serve, which asks it
// before anything that makes, opens or writes the data dir (backlog B727's
// second item): every command that writes the data dir, and every one whose
// open of the store would make it (manifest.OpenStore makes the directory
// and an empty database, which serve's own check then takes for the data dir
// it served from). It answers the refusal of the data dir dataDir, which the
// config at cfgFile names, where the record beside the file that name opens
// (dataDirRecordPath, as bridge doctor reads it) refuses it, for any of its
// causes, and nil otherwise: no record (a fresh install, init's first run), a
// record naming another data dir (dataDir moved on purpose and set where it
// is now), the data dir the one serve served from, or a config name that does
// not resolve, which bridge doctor's data-dir line does not refuse either.
func dataDirAway(cfgFile, dataDir string) *dataDirGoneError {
	record, err := dataDirRecordPath(cfgFile)
	if err != nil {
		return nil
	}
	var gone *dataDirGoneError
	if errors.As(checkServeDataDirRecord(record, dataDir), &gone) {
		return gone
	}
	return nil
}

// refuseDataDirAway answers true, having said on stderr why bridge cmd stops
// and how to go on, when dataDirAway refuses the data dir dataDir of the
// config at cfgFile: the command then makes, opens and writes nothing there.
// A diagnostic that only reads (bridge doctor, status, logs, token list, cert
// info) does not ask it.
func refuseDataDirAway(cmd, cfgFile, dataDir string, stderr io.Writer) bool {
	gone := dataDirAway(cfgFile, dataDir)
	if gone == nil {
		return false
	}
	fmt.Fprintf(stderr, "%s: %s\n", cmd, gone.commandRefusal(cmd))
	return true
}

// checkServeDataDirRecord refuses, with a *dataDirGoneError, the data dir
// dataDir that the record at path names where it is not the data dir serve
// served from, and every data dir where the record does not read. Asked
// before serve prepares the data dir, so a data dir that is not there is
// refused and never made. The record and the config may spell one data dir
// two ways (through a link, or in another case on a volume that folds case),
// so the record names dataDir by either of its spellings, each compared as
// the directory it names (dataDirRecord.names), never as a string alone.
//
// Of a data dir the record names: one holding a marker of the identity the
// record holds is the one serve served from, whatever else it holds (its
// database removed on purpose, its lock file too); one holding a marker of
// another identity is refused (otherIdentity), and so is one holding a marker
// that does not read (identityUnreadable). Where it holds no marker: one
// holding neither its database nor its lock file is refused, as B651 refused
// it (dataDirNotThere: neither is ever removed by the bridge, and a serve
// that opened its database there took its lock first); and one holding either
// is refused while the record holds an identity (noIdentity: a lock file a
// start made on the bare mount point and a database a command made there read
// as the data dir before B741), and taken up while it holds none, a record
// written before B741, whose next start gives it one (backlog B741).
//
// Anything else is no refusal: no record (path "" included), one naming
// another data dir (the config's dataDir changed), and an error other than an
// entry's absence on the way to what the data dir holds, which serve's
// preparation, its lock and its store answer.
func checkServeDataDirRecord(path, dataDir string) error {
	if path == "" {
		return nil
	}
	rec, ok, err := readDataDirRecord(path)
	if err != nil {
		return &dataDirGoneError{dataDir: dataDir, record: path, cause: recordUnreadable, err: err}
	}
	if !ok || !rec.names(dataDir) {
		return nil
	}
	f := inspectDataDir(dataDir)
	gone := &dataDirGoneError{dataDir: dataDir, record: path, identity: rec.ID}
	switch {
	case f.uninspected != nil:
		return nil
	case f.unreadable != nil:
		gone.cause, gone.err = identityUnreadable, f.unreadable
	case f.identity != "" && rec.ID != "" && f.identity != rec.ID:
		gone.cause = otherIdentity
	case f.identity != "":
		return nil
	case len(f.held) == 0:
		gone.cause = dataDirNotThere
	case rec.ID != "":
		gone.cause, gone.held = noIdentity, f.held
	default:
		return nil
	}
	return gone
}

// dataDirRecording is what recordServeDataDir writes and reads: the marker
// in the data dir (writeDataDirMarker) and the steps that make it durable
// (markerDurability), the record beside the config, and how a folder on the
// data dir's way compares the names in it (fsutil.FoldingIn). A nil field is
// the one of its own; a boot test fails a write or a step, as a start
// interrupted there leaves them, records the steps in their order, and
// answers for a folder as a volume this host does not have would
// (serveOpts.dataDirRecording).
type dataDirRecording struct {
	marker     func(dataDir, id string) error
	durability markerDurability
	record     func(path string, data []byte) error
	folding    func(dir string) fsutil.CaseFolding
}

func (w dataDirRecording) writeMarker(dataDir, id string) error {
	if w.marker != nil {
		return w.marker(dataDir, id)
	}
	return writeDataDirMarker(dataDir, id, w.durability)
}

// syncMarker makes the marker the data dir dataDir holds durable where it
// is (markerDurability.syncInPlace), before the record takes up its identity.
func (w dataDirRecording) syncMarker(dataDir string) error {
	return w.durability.syncInPlace(filepath.Join(dataDir, dataDirMarkerName))
}

func (w dataDirRecording) writeRecord(path string, data []byte) error {
	if w.record != nil {
		return w.record(path, data)
	}
	return atomicwrite.WriteBytes(path, data, ".bridge-datadir-*.tmp")
}

// foldingOnTheWay is how each folder on the way of each of spellings
// compares the names in it, one string of letters a spelling
// (fsutil.FormatFolding of fsutil.FoldingOnTheWay; dataDirRecord's
// DataDirFolding and ResolvedFolding), "" for a spelling that is "": a folder
// on the way of both is asked once.
func (w dataDirRecording) foldingOnTheWay(spellings ...string) []string {
	probe := w.folding
	if probe == nil {
		probe = fsutil.FoldingIn
	}
	asked := map[string]fsutil.CaseFolding{}
	letters := make([]string, len(spellings))
	for i, s := range spellings {
		if s != "" {
			letters[i] = fsutil.FormatFolding(fsutil.FoldingOnTheWay(s, probe, asked))
		}
	}
	return letters
}

// recordServeDataDir writes the record at path naming dataDir, once serve's
// database is open there and its lock file is too: dataDir as its config gave
// it and where it leads now, through its links, and how each folder on the
// way of both compares the names in it, while the volume it is on is there
// to resolve them and to ask (dataDirRecord), and the data dir's identity
// (backlog B741), which its marker holds durably before the record does. The
// identity is the marker's where the data dir holds one, taken up once the
// marker is synced where it is (a marker a start before left, whose record's
// write or whose barrier after its rename failed); else the record's own
// where it names this data dir (never a second one minted beside it), else a
// new one, and the marker is written first, durably (writeDataDirMarker). It
// writes the record only when it does not hold exactly what it is to hold
// already, so a record that names the data dir in another spelling, or in one
// alone (a record written before B651's fifth round), or with no identity or
// folding (one written before B741), is written again. It never writes one
// its reader would refuse for its size (marshalDataDirRecord).
//
// Nothing it cannot write refuses the start, which goes on, and each is said
// at Warn: no path (a boot read that resolved no file) or a record serve
// cannot write (a config directory it may not write), where a later start
// cannot tell that data dir's volume missing; and a marker it cannot write,
// or make durable, where the record holds no identity, and a later start
// tells the data dir by its database and its lock file alone, as before B741.
// It writes no record the next start's check would read otherwise than this
// start's did, and none without, or with another than, the identity the
// record holds of this data dir (B741's round 2, the Claude API on #1441: a
// marker that did not read at the write, whose record it wrote without one, had
// the next start refuse that data dir, or with the volume away take up the
// bare mount point's lock file for it): a record that does not read, and a
// marker that does not read or cannot be inspected, leave the record as it
// was; and so does a marker of this data dir's identity that is gone and
// cannot be written again, which the next start refuses as it would have
// refused this one.
func recordServeDataDir(path, dataDir string, w dataDirRecording) {
	if path == "" {
		logger.Warn("could not record the data directory beside the config: serve's boot read resolved no file for the config's name, so a later start cannot tell the data directory's volume missing from a fresh install",
			"dataDir", dataDir)
		return
	}
	for _, p := range []string{manifest.DefaultDBPath(dataDir), filepath.Join(dataDir, serveLockFileName)} {
		if _, err := os.Lstat(p); err != nil {
			// No record for a data dir whose lock file could not be made: the
			// refusal reads its absence as a data dir serve never served from.
			return
		}
	}
	had, ok, err := readDataDirRecord(path)
	if err != nil {
		logger.Warn("did not record the data directory beside the config: its record does not read, and serve does not replace what it cannot read; the next start refuses it until it reads or is removed",
			"record", path, "dataDir", dataDir, "err", fsutil.ErrorWithoutPath(err))
		return
	}
	// kept is the identity the record holds of this data dir, which no write
	// of the record drops or replaces.
	kept := ""
	if ok && had.names(dataDir) {
		kept = had.ID
	}
	id := ""
	switch f := inspectDataDir(dataDir); {
	case f.unreadable != nil || f.uninspected != nil:
		reason := f.unreadable
		if reason == nil {
			reason = f.uninspected
		}
		logger.Warn("did not record the data directory beside the config: its marker cannot be read, and serve does not replace it unread; the record is left as it was",
			"dataDir", dataDir, "marker", dataDirMarkerName, "err", fsutil.ErrorWithoutPath(reason))
		return
	case f.identity != "" && kept != "" && f.identity != kept:
		logger.Warn("did not record the data directory beside the config: its marker holds another identity than the record, and serve replaces neither; the record is left as it was, and the next start refuses the data directory for it",
			"dataDir", dataDir, "marker", dataDirMarkerName)
		return
	case f.identity != "":
		id = f.identity
		if id != kept {
			if err := w.syncMarker(dataDir); err != nil {
				logger.Warn("did not record the data directory's identity beside the config: its marker may not outlive a crash, and the record takes up its identity only once it is durable; the next start tries again",
					"dataDir", dataDir, "marker", dataDirMarkerName, "err", fsutil.ErrorWithoutPath(err))
				id = ""
			}
		}
	default:
		id = kept
		if id == "" {
			id = mintDataDirIdentity()
		}
		if err := w.writeMarker(dataDir, id); err != nil {
			if kept != "" {
				logger.Warn("could not record the data directory's identity in it again: the record is left as it was, holding it, and the next start refuses the data directory for holding no marker of it",
					"dataDir", dataDir, "marker", dataDirMarkerName, "err", fsutil.ErrorWithoutPath(err))
				return
			}
			logger.Warn("could not record the data directory's identity in it: a later start tells the data directory by its database and its lock file alone",
				"dataDir", dataDir, "marker", dataDirMarkerName, "err", fsutil.ErrorWithoutPath(err))
			id = ""
		}
	}
	want := dataDirRecord{DataDir: dataDir, ID: id}
	if resolved, err := fsutil.ResolveLinks(dataDir); err == nil {
		want.Resolved = resolved
	}
	folding := w.foldingOnTheWay(want.DataDir, want.Resolved)
	want.DataDirFolding, want.ResolvedFolding = folding[0], folding[1]
	if ok && had.same(want) {
		return
	}
	b, err := marshalDataDirRecord(want)
	if err == nil {
		err = w.writeRecord(path, b)
	}
	if err != nil {
		logger.Warn("could not record the data directory beside the config: a later start cannot tell the data directory's volume missing from a fresh install",
			"record", path, "dataDir", dataDir, "err", err)
	}
}
