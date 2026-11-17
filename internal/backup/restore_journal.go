package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The database-only restore's journal (backlog B653 PR 2b1). It stands, at
// manifest.RestoreJournalPath beside the database, from the restore's first
// rename until the restore is finished or put back, and while it stands
// every opener of the live database refuses (manifest.CheckRestoreJournal),
// so a restore stopped anywhere between (a kill, a power loss, a sharing
// violation) leaves the bridge refusing to start rather than opening a
// database the restore was moving. It says which way the restore was going
// (its phase, written durably before the first rename of each direction),
// what the restore moved out and where to (each original's name, presence,
// size and SHA-256, and the archive folder), and the prepared copy's size and
// SHA-256: all a recovery needs, which never reads the snapshot again.
// `bridge restore --database-only`, with the journal standing and nothing
// else, is that recovery (FinishDatabaseRestore): an `installed` restore is
// completed, any other is put back.

// The journal's phases.
const (
	// PhaseForward is the restore moving the originals out and the prepared
	// copy in (C2 to C5).
	PhaseForward = "forward"
	// PhaseInstalled is the prepared copy in place, checked by its content
	// (C5′): what is left is the harvest's rewind and the journal's removal.
	PhaseInstalled = "installed"
	// PhasePuttingBack is the originals going back.
	PhasePuttingBack = "puttingBack"
)

// journalFormat is the journal's format; a journal of any other is refused.
const journalFormat = 1

// maxJournalBytes bounds the journal's read: a journal is a few hundred bytes.
const maxJournalBytes = 64 << 10

// OriginalsDirName is the folder in the data dir the database-only restore
// moves the original database files into, one folder a run, which nothing
// the bridge runs removes: a sibling of the backups folder, never inside
// it, so no prune and no orphan sweep of the backups folder reaches it.
const OriginalsDirName = "restore-originals"

// originalsRecordName is the record of a run's originals, in its folder.
const originalsRecordName = "restore.json"

// archiveStamp is the shape of a run's folder name: the restore's UTC time as
// a snapshot names its folder, and a count where that name was taken.
var archiveStamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}Z(-[0-9]{1,4})?$`)

// originalNames are the four names a database's files go by, the database
// first: what the restore moves out and the put-back moves back.
func originalNames() []string {
	db := filepath.Base(manifest.DefaultDBPath("."))
	return []string{db, db + "-wal", db + "-shm", db + "-journal"}
}

// sidecarOriginalNames are originalNames less the database, in the order the
// restore moves them out (C3).
func sidecarOriginalNames() []string {
	n := originalNames()
	return []string{n[3], n[1], n[2]}
}

// FileFact is what the journal knows of one original: whether it was there,
// and if so its size and SHA-256, read before it moved.
type FileFact struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

// PreparedFact is the prepared copy's size and SHA-256.
type PreparedFact struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// SnapshotNote names the snapshot a restore was of, for words alone: no
// recovery reads it.
type SnapshotNote struct {
	Given  string `json:"given"`
	Abs    string `json:"abs"`
	Folder string `json:"folder"`
}

// RestoreJournal is the journal, and, with no phase, the record a run keeps
// beside its originals (restore.json).
type RestoreJournal struct {
	Format        int          `json:"format"`
	ID            string       `json:"id"`
	StartedAt     time.Time    `json:"startedAt"`
	BridgeVersion string       `json:"bridgeVersion"`
	Phase         string       `json:"phase,omitempty"`
	Archive       string       `json:"archive"`
	Prepared      PreparedFact `json:"prepared"`
	Originals     []FileFact   `json:"originals"`
	Snapshot      SnapshotNote `json:"snapshot"`
}

// Original is the fact the journal holds for name.
func (j *RestoreJournal) Original(name string) FileFact {
	for _, o := range j.Originals {
		if o.Name == name {
			return o
		}
	}
	return FileFact{Name: name}
}

// ArchiveDir is the run's folder of originals in dataDir, the journal's
// archive validated (readRestoreJournal) before it is used.
func (j *RestoreJournal) ArchiveDir(dataDir string) string {
	return filepath.Join(dataDir, OriginalsDirName, strings.TrimPrefix(j.Archive, OriginalsDirName+"/"))
}

// ErrNoRestoreJournal is ReadRestoreJournal's answer where no journal stands.
var ErrNoRestoreJournal = errors.New("no unfinished restore of the library database stands")

// ErrRestoreJournalUnread is the refusal of a journal that stands and cannot
// be taken: not a regular file, too large, unreadable, of another format, or
// naming what no restore writes. Nothing is moved and the journal is kept.
var ErrRestoreJournalUnread = errors.New("the unfinished restore's journal cannot be read")

// journalPath is the journal of the database in dataDir.
func journalPath(dataDir string) string {
	return manifest.RestoreJournalPath(manifest.DefaultDBPath(dataDir))
}

// ReadRestoreJournal reads the journal of the database in dataDir: bounded,
// opening nothing but a regular file of at most maxJournalBytes through
// fsutil.OpenAsFile (a named pipe refused at once), and taking only a journal
// of journalFormat, a known phase, the four originals by their names, and an
// archive that names one folder of the run's shape under OriginalsDirName.
// ErrNoRestoreJournal where none stands; ErrRestoreJournalUnread, naming why,
// for one that stands and cannot be taken.
func ReadRestoreJournal(dataDir string) (*RestoreJournal, error) {
	path := journalPath(dataDir)
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNoRestoreJournal
	case err != nil:
		return nil, fmt.Errorf("%w: %s", ErrRestoreJournalUnread, fsutil.ErrorWithoutPath(err))
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRestoreJournalUnread, filepath.Base(path))
	case fi.Size() > maxJournalBytes:
		return nil, fmt.Errorf("%w: %s holds %d bytes, more than a journal does", ErrRestoreJournalUnread, filepath.Base(path), fi.Size())
	}
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRestoreJournalUnread, fsutil.ErrorWithoutPath(err))
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRestoreJournalUnread, fsutil.ErrorWithoutPath(err))
	}
	if len(data) > maxJournalBytes {
		return nil, fmt.Errorf("%w: %s holds more than a journal does", ErrRestoreJournalUnread, filepath.Base(path))
	}
	var j RestoreJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%w: %s does not decode", ErrRestoreJournalUnread, filepath.Base(path))
	}
	if why := j.invalid(); why != "" {
		return nil, fmt.Errorf("%w: %s %s", ErrRestoreJournalUnread, filepath.Base(path), why)
	}
	return &j, nil
}

// invalid says why j is no journal a database-only restore writes, "" where
// it is one.
func (j *RestoreJournal) invalid() string {
	if j.Format != journalFormat {
		return fmt.Sprintf("is of format %d, which this bridge does not read", j.Format)
	}
	switch j.Phase {
	case PhaseForward, PhaseInstalled, PhasePuttingBack:
	default:
		return "names no phase a restore writes"
	}
	if j.BridgeVersion == "" {
		return "names no bridge version"
	}
	stamp, ok := strings.CutPrefix(j.Archive, OriginalsDirName+"/")
	if !ok || !archiveStamp.MatchString(stamp) {
		return "names no folder of " + OriginalsDirName + " a restore makes"
	}
	if j.Prepared.Size <= 0 || !isSHA256(j.Prepared.SHA256) {
		return "records no prepared copy"
	}
	names := originalNames()
	if len(j.Originals) != len(names) {
		return "records another set of originals than a database's four files"
	}
	seen := map[string]bool{}
	for _, o := range j.Originals {
		if !containsName(names, o.Name) || seen[o.Name] {
			return "records another set of originals than a database's four files"
		}
		seen[o.Name] = true
		switch {
		case o.Present && (o.Size < 0 || !isSHA256(o.SHA256)):
			return "records an original with no size or SHA-256"
		case !o.Present && (o.Size != 0 || o.SHA256 != ""):
			return "records an absent original with a size or a SHA-256"
		}
	}
	return ""
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// isSHA256 reports whether s is a SHA-256 written as 64 lower-case hex digits.
func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// randomID is a short random identifier, for a journal and a staged name.
func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// stagedJSONPrefix names a journal or a record staged in the data dir before
// its rename: a leftover of one is removed by name by the next restore (B6).
func stagedJSONPrefix() string {
	return filepath.Base(manifest.RestoreJournalPath(manifest.DefaultDBPath("."))) + ".tmp-"
}

// writeStagedJSON writes v as JSON into a new file of stagedJSONPrefix's
// name in dataDir, synced and closed, and answers its path.
func writeStagedJSON(dataDir string, v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, stagedJSONPrefix()+id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(append(data, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", werr
	}
	return path, nil
}

// publishJournal writes j as the journal where none stands (C2): staged,
// synced, renamed onto its name by the no-replace rename, the data dir
// synced (restoreBarrier).
func publishJournal(dataDir string, j *RestoreJournal) error {
	staged, err := writeStagedJSON(dataDir, j)
	if err != nil {
		return fmt.Errorf("write the restore's journal: %w", err)
	}
	if err := fsutil.RenameNoReplace(staged, journalPath(dataDir)); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("publish the restore's journal: %w", err)
	}
	if err := restoreBarrier(dataDir, dataDir); err != nil {
		return fmt.Errorf("publish the restore's journal: %w", err)
	}
	return nil
}

// setJournalPhase rewrites the standing journal with phase: staged, synced,
// renamed over the journal alone, the data dir synced, before anything of the
// new phase moves.
func setJournalPhase(dataDir string, j *RestoreJournal, phase string) error {
	next := *j
	next.Phase = phase
	staged, err := writeStagedJSON(dataDir, &next)
	if err != nil {
		return fmt.Errorf("write the restore's journal: %w", err)
	}
	if err := atomicwrite.RenameWithRetry(staged, journalPath(dataDir)); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("write the restore's journal: %w", err)
	}
	if err := restoreBarrier(dataDir, dataDir); err != nil {
		return fmt.Errorf("write the restore's journal: %w", err)
	}
	j.Phase = phase
	return nil
}

// removeJournal removes the journal and syncs the data dir: the restore, or
// its put-back, is then finished, whatever is left to tidy.
func removeJournal(dataDir string) error {
	if err := os.Remove(journalPath(dataDir)); err != nil {
		return fmt.Errorf("remove the restore's journal: %w", err)
	}
	if err := restoreBarrier(dataDir, dataDir); err != nil {
		return fmt.Errorf("remove the restore's journal: %w", err)
	}
	return nil
}

// barrierName is the name in a directory the restore's barrier probes
// (atomicwrite.ProbeDurable), which syncs the directory that would hold it and
// every one above it up to the data dir; nothing is ever written at it.
const barrierName = ".bridge-restore-barrier"

// restoreBarrier syncs dir, the data dir or a folder of originals below it,
// and every directory between it and the data dir, through the strict
// writer's own barrier (atomicwrite.ProbeDurable): nil once synced, and where
// this platform syncs no directory at all (Windows, ErrNoDirectoryBarrier:
// each rename there is atomic and not known durable, backlog B920); any other
// answer is the barrier's failure, which stops the restore before its next
// rename.
func restoreBarrier(dataDir, dir string) error {
	if err := atomicwrite.ProbeDurable(dataDir, filepath.Join(dir, barrierName)); err != nil && !errors.Is(err, atomicwrite.ErrNoDirectoryBarrier) {
		return err
	}
	return nil
}

// probeRestoreBarrier is the restore's question before it moves anything
// (C1): whether a directory in dataDir can be synced. A filesystem that
// answers every directory sync with a failure (some network and FUSE mounts,
// atomicwrite.ErrBarrierFailed) cannot order the journal's publication before
// the renames it guards, and is refused, nothing moved; so is every other
// failure of the probe. A platform with no directory barrier at all
// (Windows) goes on, its renames atomic and not known durable (B920).
func probeRestoreBarrier(dataDir string) error {
	switch err := atomicwrite.ProbeDurable(dataDir, filepath.Join(dataDir, barrierName)); {
	case err == nil, errors.Is(err, atomicwrite.ErrNoDirectoryBarrier):
		return nil
	case errors.Is(err, atomicwrite.ErrBarrierFailed):
		return fmt.Errorf("the filesystem of the data directory refuses to sync a directory, so the restore could not order its journal before the files it moves (%w); "+
			"restore the database alone from a data directory on a local disk, or run the full restore; nothing was restored", err)
	default:
		return fmt.Errorf("whether the data directory's changes can be made durable could not be shown (%w); nothing was restored", err)
	}
}

// entryKind is what Lstat finds at a name.
type entryKind int

const (
	entryAbsent entryKind = iota
	entryFile
	entryOther
)

// entryFact is what stands at a name: nothing, a regular file and its
// size and SHA-256, or another kind.
type entryFact struct {
	kind   entryKind
	size   int64
	sha256 string
}

func (e entryFact) String() string {
	switch e.kind {
	case entryAbsent:
		return "nothing"
	case entryFile:
		return fmt.Sprintf("a file of %d bytes", e.size)
	}
	return "an entry that is not a regular file"
}

// matches reports whether e is a regular file of the size and SHA-256 given.
func (e entryFact) matches(size int64, sum string) bool {
	return e.kind == entryFile && e.size == size && e.sha256 == sum
}

// readEntry says what stands at path: by Lstat, and for a regular file its
// content hashed whole through one handle (fsutil.OpenAsFile), its size the
// bytes read. A read that does not finish is an error.
func readEntry(path string) (entryFact, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return entryFact{kind: entryAbsent}, nil
	case err != nil:
		return entryFact{}, err
	case !fi.Mode().IsRegular():
		return entryFact{kind: entryOther}, nil
	}
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return entryFact{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return entryFact{}, err
	}
	return entryFact{kind: entryFile, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// readAndSyncOriginal is C0's read of the original at path: absent, or a
// regular file opened once, hashed whole as it streams, synced through that
// handle and closed before anything moves it, its size the bytes read; any
// other kind is refused. Off Windows it is opened read-only, through
// fsutil.OpenAsFile; on Windows for writing, since File.Sync there is
// FlushFileBuffers, which needs it.
func readAndSyncOriginal(path string) (entryFact, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return entryFact{kind: entryAbsent}, nil
	case err != nil:
		return entryFact{}, err
	case !fi.Mode().IsRegular():
		return entryFact{}, fmt.Errorf("%s is not a regular file, and the restore takes only the database's own files", filepath.Base(path))
	}
	var f *os.File
	if runtime.GOOS == "windows" {
		f, err = os.OpenFile(path, os.O_RDWR, 0)
	} else {
		f, _, err = fsutil.OpenAsFile(path)
	}
	if err != nil {
		return entryFact{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err == nil {
		err = syncOriginal(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return entryFact{}, fmt.Errorf("read and sync %s: %w", filepath.Base(path), err)
	}
	return entryFact{kind: entryFile, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// originalSync is C0's sync of an original, (*os.File).Sync; a test's
// stand-in fails it (SetOriginalSyncForTest).
var originalSync atomic.Pointer[func(f *os.File) error]

func syncOriginal(f *os.File) error {
	if fn := originalSync.Load(); fn != nil {
		return (*fn)(f)
	}
	return f.Sync()
}

// SetOriginalSyncForTest makes C0's sync of each original fn's answer, and
// returns what puts the one before it back. Process-global: a test that uses
// it must not call t.Parallel().
func SetOriginalSyncForTest(fn func(f *os.File) error) (restore func()) {
	var p *func(*os.File) error
	if fn != nil {
		p = &fn
	}
	prev := originalSync.Swap(p)
	return func() { originalSync.Store(prev) }
}

// restoreStep, nil in production, runs at each step of the database-only
// restore and its put-back that a stop there would leave a layout of its own
// (SetRestoreStepForTest): the interruption tests stop the process there.
var restoreStep atomic.Pointer[func(step string)]

// step runs restoreStep's hook, if any.
func step(name string) {
	if fn := restoreStep.Load(); fn != nil {
		(*fn)(name)
	}
}

// SetRestoreStepForTest installs fn as the database-only restore's step
// hook, and returns what puts the one before it back. Process-global: a test
// that uses it must not call t.Parallel().
func SetRestoreStepForTest(fn func(step string)) (restore func()) {
	var p *func(string)
	if fn != nil {
		p = &fn
	}
	prev := restoreStep.Swap(p)
	return func() { restoreStep.Store(prev) }
}
