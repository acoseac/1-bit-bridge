package backup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A snapshot's verification (backlog B653). Snapshot records, in the
// manifest it publishes, each file's size and SHA-256 as it writes it (the
// database's read once, right after VACUUM INTO writes it), and publishes
// the manifest with its verification record already PENDING, so no bundle
// this bridge writes is without one. VerifySnapshot then checks the bundle
// against those baselines, never replacing them with what it finds, and
// records two verdicts: the database's (its baseline, then a read-only open
// that runs SQLite's own check and reads the schema its version requires)
// and the other files' (presence, size and SHA-256). Pruning, the startup
// snapshot's skip and "the newest verified" read the database's verdict
// alone; the doctor and the console show both, reading the manifests and
// nothing else (Status). verify_db.go is the database's check,
// verify_record.go the record's publication under the backups folder's lock
// (verify_lock.go), and finish.go the one path every snapshot takes.

// State is one verdict of a snapshot's verification record: the database's
// or the other files'.
type State string

const (
	// StatePending is a check not ended: Snapshot writes it, and a
	// verification replaces it. A reader takes one older than PendingBound
	// for INCOMPLETE (ReasonInterrupted).
	StatePending State = "PENDING"
	// StateVerified is a check that passed.
	StateVerified State = "VERIFIED"
	// StateFailed is a check that found the bundle not as captured, or not a
	// database this bridge can recover from.
	StateFailed State = "FAILED"
	// StateIncomplete is a check that did not end: stopped, past its
	// deadline, or read so by a reader (an interrupted PENDING, or a verdict
	// whose write did not become durable).
	StateIncomplete State = "INCOMPLETE"
	// StateUnverified is a snapshot with nothing to verify: one written
	// before verification, which recorded no baseline (a reader's word, never
	// written), a database a snapshot did not capture (written, with
	// ReasonNoDatabase), or a record this bridge cannot read. Never verified
	// and never corrupt.
	StateUnverified State = "UNVERIFIED"
)

// Reason is a verdict's reason code: a fixed word, from a closed list
// (Reasons), that names no path.
type Reason string

const (
	// ReasonNoBaseline: the snapshot recorded no size and SHA-256 for the
	// file (written before verification).
	ReasonNoBaseline Reason = "no-baseline"
	// ReasonNoDatabase: the snapshot holds no database (the install had none
	// when it was taken).
	ReasonNoDatabase Reason = "no-database"
	// ReasonMissing: a file the snapshot lists is not there.
	ReasonMissing Reason = "missing"
	// ReasonNotRegular: a file the snapshot lists is a link, a folder or
	// another kind of entry, never opened.
	ReasonNotRegular Reason = "not-regular"
	// ReasonBadName: a name the snapshot lists is not a file of the bundle
	// (a separator, "." or "..").
	ReasonBadName Reason = "bad-name"
	// ReasonSidecar: a -wal, -shm or -journal beside the database, which a
	// snapshot never holds.
	ReasonSidecar Reason = "sidecar"
	// ReasonSize and ReasonHash: the file is not the size, or its SHA-256 is
	// not the one, the snapshot recorded.
	ReasonSize Reason = "size-mismatch"
	ReasonHash Reason = "hash-mismatch"
	// ReasonUnreadable: a read of the file failed.
	ReasonUnreadable Reason = "unreadable"
	// ReasonOpenFailed: SQLite would not open the database.
	ReasonOpenFailed Reason = "open-failed"
	// ReasonQuickCheck, ReasonIntegrityCheck, ReasonForeignKeyCheck: SQLite's
	// own check did not answer exactly one "ok" (quick_check, integrity_check),
	// or foreign_key_check found a row.
	ReasonQuickCheck      Reason = "quick-check"
	ReasonIntegrityCheck  Reason = "integrity-check"
	ReasonForeignKeyCheck Reason = "foreign-key-check"
	// ReasonSchemaVersion: a user_version below the migration ladder's first
	// step; ReasonSchemaNewer: one past this bridge's ladder.
	ReasonSchemaVersion Reason = "schema-version"
	ReasonSchemaNewer   Reason = "schema-newer"
	// ReasonSchemaTable and ReasonSchemaColumn: a table, or a column, the
	// database's user_version requires is not there.
	ReasonSchemaTable  Reason = "schema-table"
	ReasonSchemaColumn Reason = "schema-column"
	// ReasonBundleChanged: the bundle's files changed while it was checked.
	ReasonBundleChanged Reason = "bundle-changed"
	// ReasonCancelled and ReasonDeadline: the check was stopped (a shutdown,
	// a cancelled command) or ran past its deadline.
	ReasonCancelled Reason = "cancelled"
	ReasonDeadline  Reason = "deadline"
	// ReasonInterrupted: a reader's word for a PENDING older than
	// PendingBound, whose check no one is finishing.
	ReasonInterrupted Reason = "interrupted"
	// ReasonUnsynced: a reader's word for a verdict whose write did not
	// become durable (the bundle holds the verdict's veto).
	ReasonUnsynced Reason = "unsynced"
	// ReasonUnknownRecord: a reader's word for a record this bridge cannot
	// read (a newer format, a state it does not know).
	ReasonUnknownRecord Reason = "unknown-record"
)

// Reasons is every reason code a verdict can carry, for a surface that words
// them (the doctor, the console).
func Reasons() []Reason {
	return []Reason{
		ReasonNoBaseline, ReasonNoDatabase, ReasonMissing, ReasonNotRegular, ReasonBadName, ReasonSidecar,
		ReasonSize, ReasonHash, ReasonUnreadable, ReasonOpenFailed, ReasonQuickCheck, ReasonIntegrityCheck,
		ReasonForeignKeyCheck, ReasonSchemaVersion, ReasonSchemaNewer, ReasonSchemaTable, ReasonSchemaColumn,
		ReasonBundleChanged, ReasonCancelled, ReasonDeadline, ReasonInterrupted, ReasonUnsynced, ReasonUnknownRecord,
	}
}

// CheckLevel is how far the database's check goes: CheckQuick (SQLite's
// quick_check: every page well formed, every row's constraints, and no check
// that an index matches its table) or CheckFull (integrity_check, which
// does, and foreign_key_check), which bridge backup --verify --full runs.
type CheckLevel string

const (
	CheckQuick CheckLevel = "quick"
	CheckFull  CheckLevel = "full"
)

// rank orders the levels: a verdict never replaces one of a higher level
// unless it is worse (mergeVerdict).
func (l CheckLevel) rank() int {
	if l == CheckFull {
		return 2
	}
	if l == CheckQuick {
		return 1
	}
	return 0
}

// Baseline is a captured file's size and SHA-256 (hex), recorded as Snapshot
// writes it. Verification compares against it and never replaces it.
type Baseline struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// FileFault is a file of the bundle that failed the files' check, by its
// name in the bundle (one of the names a snapshot captures, never a path)
// and its reason.
type FileFault struct {
	Name   string `json:"name"`
	Reason Reason `json:"reason"`
}

// verificationFormat is the record's format; a reader reads a newer one's
// states only where it knows them.
const verificationFormat = 1

// Verification is a snapshot's verification record: the database's verdict
// (DatabaseState, its reasons, the check's level and time, its duration, the
// user_version read and the core tables' row counts, which are information,
// 0 a valid count) and the other files' (FilesState, its reasons, the files
// that failed). PendingSince is when the PENDING it holds was written.
type Verification struct {
	Format          int              `json:"format"`
	DatabaseState   State            `json:"databaseState"`
	DatabaseReasons []Reason         `json:"databaseReasons,omitempty"`
	Level           CheckLevel       `json:"level,omitempty"`
	CheckedAt       time.Time        `json:"checkedAt,omitzero"`
	DurationMS      int64            `json:"durationMs,omitempty"`
	UserVersion     int              `json:"userVersion,omitempty"`
	Counts          map[string]int64 `json:"counts,omitempty"`
	FilesState      State            `json:"filesState"`
	FilesReasons    []Reason         `json:"filesReasons,omitempty"`
	FilesFailed     []FileFault      `json:"filesFailed,omitempty"`
	FilesCheckedAt  time.Time        `json:"filesCheckedAt,omitzero"`
	PendingSince    time.Time        `json:"pendingSince,omitzero"`
}

// pendingRecord is the record Snapshot publishes with a bundle.
func pendingRecord(now time.Time, hasDatabase bool) *Verification {
	v := &Verification{Format: verificationFormat, DatabaseState: StatePending, FilesState: StatePending, PendingSince: now}
	if !hasDatabase {
		v.DatabaseState, v.DatabaseReasons = StateUnverified, []Reason{ReasonNoDatabase}
	}
	return v
}

// coreTables are the tables whose row counts a verification records:
// the library, its folders, and the playlists and their entries.
var coreTables = []string{"tracks", "folders", "playlists", "playlist_items"}

// capturedNames is every file a snapshot captures, by its name in the
// bundle: a FileFault names one of these, or none.
var capturedNames = []string{
	ManifestDBFileName, "tokens.json", "server.crt", "server.key", "bridge.yaml", AdminAuthFileName,
	AtlasHarvestFileName, PlayerUUIDFileName, engine.ReservationFileName, PlayerDSDFileName,
}

// plainName reports whether name is a file directly in the bundle: no
// separator, not "." or "..", not empty.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) &&
		filepath.Base(name) == name && !filepath.IsAbs(name) && filepath.VolumeName(name) == ""
}

// PendingBound is how long a PENDING stands before a reader takes it for an
// INCOMPLETE whose check no one is finishing (ReasonInterrupted): longer than
// a verification's deadline (VerifyDeadline) and the wait for the backups
// folder's lock that comes first (AutoLockWait), with ten minutes to spare,
// so a check under way, or one waiting its turn behind another process's, is
// never read so. A PENDING younger than it is never pruned.
const PendingBound = VerifyDeadline + AutoLockWait + 10*time.Minute

// VerifyDeadline is how long a verification may run: past it the check
// stops, INCOMPLETE (ReasonDeadline).
const VerifyDeadline = 30 * time.Minute

// vetoFileName is the file a verdict's write puts in the bundle before it
// publishes a VERIFIED state and removes once the write is durable
// (verify_record.go): while it stands, a reader reads that state as
// INCOMPLETE (ReasonUnsynced), so a verification that returned an error
// never leaves a VERIFIED any reader reads.
const vetoFileName = ".verdict-unsynced"

// readVerdict is what a reader reads in m's record at now: the record as
// written, but a snapshot with no record (written before verification) reads
// UNVERIFIED (ReasonNoBaseline), a record whose format or states this bridge
// does not know reads UNVERIFIED (ReasonUnknownRecord), a PENDING older than
// PendingBound reads INCOMPLETE (ReasonInterrupted), and a VERIFIED beside the
// bundle's veto (vetoed) reads INCOMPLETE (ReasonUnsynced). It is the one
// reading the pruner, the startup skip, the doctor and the console take.
func readVerdict(m Manifest, now time.Time, vetoed bool) Verification {
	if m.Verification == nil {
		return Verification{
			Format: verificationFormat, DatabaseState: StateUnverified, DatabaseReasons: []Reason{ReasonNoBaseline},
			FilesState: StateUnverified, FilesReasons: []Reason{ReasonNoBaseline},
		}
	}
	v := *m.Verification
	v.DatabaseReasons = slices.Clone(v.DatabaseReasons)
	v.FilesReasons = slices.Clone(v.FilesReasons)
	v.FilesFailed = slices.Clone(v.FilesFailed)
	if v.Format > verificationFormat || v.Format < 1 {
		v.DatabaseState, v.DatabaseReasons = StateUnverified, []Reason{ReasonUnknownRecord}
		v.FilesState, v.FilesReasons = StateUnverified, []Reason{ReasonUnknownRecord}
		return v
	}
	// A PENDING with no time, or one further than the bound from now either
	// way (a clock stepped back since it was written), is no check under way.
	age := now.Sub(v.PendingSince)
	stale := v.PendingSince.IsZero() || age >= PendingBound || -age >= PendingBound
	read := func(s State, reasons []Reason) (State, []Reason) {
		switch s {
		case StatePending:
			if stale {
				return StateIncomplete, []Reason{ReasonInterrupted}
			}
			return s, reasons
		case StateVerified:
			if vetoed {
				return StateIncomplete, []Reason{ReasonUnsynced}
			}
			return s, reasons
		case StateFailed, StateIncomplete, StateUnverified:
			return s, reasons
		}
		return StateUnverified, []Reason{ReasonUnknownRecord}
	}
	v.DatabaseState, v.DatabaseReasons = read(v.DatabaseState, v.DatabaseReasons)
	v.FilesState, v.FilesReasons = read(v.FilesState, v.FilesReasons)
	return v
}

// vetoedAt reports whether the bundle at dir holds a verdict's veto: any
// entry by that name, or one Lstat cannot tell is not there, reads as one.
func vetoedAt(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, vetoFileName))
	return !errors.Is(err, os.ErrNotExist)
}

// legacy reports whether m is a snapshot written before verification: one
// that recorded no baseline. The pruner keeps the newest of them while no
// snapshot's database is verified.
func (m Manifest) legacy() bool { return len(m.Baselines) == 0 && m.Verification == nil }

// baseline is the recorded baseline of name, if any.
func (m Manifest) baseline(name string) (Baseline, bool) {
	for _, b := range m.Baselines {
		if b.Name == name {
			return b, true
		}
	}
	return Baseline{}, false
}
