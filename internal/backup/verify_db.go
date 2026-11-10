package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"modernc.org/sqlite/vfs"
)

// The database's check opens the snapshot's bridge.db read-only on a
// connection of its own, never through manifest.OpenStore (which creates the
// file, takes the ladder's lock and migrates), with mode=ro&immutable=1:
// SQLite then reads the file as it is and opens nothing beside it, where
// mode=ro alone, over a database whose header is in WAL mode, creates a -shm
// and a -wal in the bundle (measured 2026-11-08 with the module's SQLite). It
// opens it through modernc's read-only VFS over a filesystem that serves that
// one file (dbFS), so no other name can be opened or created, every read is
// paced and stops once the check's context ends, and the bytes SQLite reads
// are the file the baseline was compared with, the same open. The functions
// the expression indexes call (unicode_lower, mix_week_hash) are registered
// by internal/manifest, which this package imports, for every connection.

// sidecarSuffixes are the files SQLite keeps beside a database it writes; a
// snapshot (VACUUM INTO's single file) holds none.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// dbCheck is the database's verdict, with the facts the record keeps.
type dbCheck struct {
	state       State
	reasons     []Reason
	userVersion int
	counts      map[string]int64
}

// failed is a dbCheck that FAILED for reasons.
func failed(reasons ...Reason) dbCheck { return dbCheck{state: StateFailed, reasons: reasons} }

// stopped is the INCOMPLETE ctx's end gives, or ok false while it has not
// ended.
func stopped(ctx context.Context) (Reason, bool) {
	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonDeadline, true
	case err != nil:
		return ReasonCancelled, true
	}
	return "", false
}

// entryState lists the bundle's entries, each with its kind, size and
// modification time: what a check compares before it opens the database and
// after it closes it.
func entryState(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out[e.Name()] = fmt.Sprintf("%v/%d/%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
	}
	return out, nil
}

// checkDatabase is the database's check of the bundle at dir, whose
// manifest is m, at level, its reads through p.
func checkDatabase(ctx context.Context, dir string, m Manifest, level CheckLevel, p *pacer) dbCheck {
	if !slices.Contains(m.Files, ManifestDBFileName) {
		return dbCheck{state: StateUnverified, reasons: []Reason{ReasonNoDatabase}}
	}
	path := filepath.Join(dir, ManifestDBFileName)
	// What stands beside the database, of any kind, before anything is
	// opened: a -wal holds commits the file does not, and SQLite would read
	// it, or create one, beside a database it opens otherwise.
	for _, suffix := range sidecarSuffixes {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, fs.ErrNotExist) {
			return failed(ReasonSidecar)
		}
	}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return failed(ReasonMissing)
	case err != nil:
		return failed(ReasonUnreadable)
	case !info.Mode().IsRegular():
		return failed(ReasonNotRegular)
	}
	base, ok := m.baseline(ManifestDBFileName)
	if !ok {
		return failed(ReasonNoBaseline)
	}
	if info.Size() != base.Size {
		return failed(ReasonSize)
	}
	before, err := entryState(dir)
	if err != nil {
		return failed(ReasonUnreadable)
	}
	f, err := os.Open(path)
	if err != nil {
		return failed(ReasonUnreadable)
	}
	defer func() { _ = f.Close() }()
	// The open is the file Lstat saw: a regular file put in a link's place
	// between the two would be another.
	if opened, err := f.Stat(); err != nil || !os.SameFile(opened, info) || !opened.Mode().IsRegular() {
		return failed(ReasonBundleChanged)
	}
	size, sum, err := hashPaced(p, f)
	if err != nil {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		return failed(ReasonUnreadable)
	}
	if size != base.Size {
		return failed(ReasonSize)
	}
	if sum != base.SHA256 {
		return failed(ReasonHash)
	}
	got := checkSQLite(ctx, f, level, p)
	if got.state == StateIncomplete {
		return got
	}
	after, err := entryState(dir)
	if err != nil {
		return failed(ReasonUnreadable)
	}
	if !maps.Equal(before, after) {
		return failed(ReasonBundleChanged)
	}
	return got
}

// onDatabaseOpen, nil in production, runs just before SQLite opens a
// snapshot's database: a test proves a refusal came before it.
var onDatabaseOpen func()

// checkSQLite runs SQLite's check over the opened database f at level, then
// reads its user_version, the schema that version requires and the core
// tables' row counts, and closes the database and its VFS before it answers.
func checkSQLite(ctx context.Context, f *os.File, level CheckLevel, p *pacer) dbCheck {
	if onDatabaseOpen != nil {
		onDatabaseOpen()
	}
	name, fsys, err := vfs.New(dbFS{f: f, p: p})
	if err != nil {
		return failed(ReasonOpenFailed)
	}
	db, err := sql.Open("sqlite", "file:"+dbFSName+"?vfs="+name+"&mode=ro&immutable=1&_pragma=temp_store(2)")
	if err != nil {
		_ = fsys.Close()
		return failed(ReasonOpenFailed)
	}
	db.SetMaxOpenConns(1)
	got := runChecks(ctx, db, level)
	// Every connection closed before the VFS is: its Close refuses while one
	// is open, and SQLite keeps a pointer to it in each.
	_ = db.Close()
	_ = fsys.Close()
	if got.state != StateIncomplete {
		if why, ok := stopped(ctx); ok && got.state == StateFailed {
			// A read the stop refused fails the statement it was in: that is
			// the stop, not the database.
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
	}
	return got
}

// maxCheckRows bounds how many rows of quick_check's or integrity_check's
// answer a check reads: one "ok" passes, and anything else fails however
// many there are.
const maxCheckRows = 8

// runChecks is checkSQLite's statements over db.
func runChecks(ctx context.Context, db *sql.DB, level CheckLevel) dbCheck {
	conn, err := db.Conn(ctx)
	if err != nil {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		return failed(ReasonOpenFailed)
	}
	defer func() { _ = conn.Close() }()
	stmt, reason := "PRAGMA quick_check", ReasonQuickCheck
	if level == CheckFull {
		stmt, reason = "PRAGMA integrity_check", ReasonIntegrityCheck
	}
	rows, err := firstRows(ctx, conn, stmt)
	if why, ok := stopped(ctx); ok {
		return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
	}
	// A check that errors out (SQLite's SQLITE_CORRUPT on a page it cannot
	// read) has failed as surely as one that answers what it found.
	if err != nil || len(rows) != 1 || rows[0] != "ok" {
		return failed(reason)
	}
	if level == CheckFull {
		violations, err := firstRows(ctx, conn, "PRAGMA foreign_key_check")
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		if err != nil || len(violations) != 0 {
			return failed(ReasonForeignKeyCheck)
		}
	}
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		return failed(ReasonOpenFailed)
	}
	got := dbCheck{state: StateVerified, userVersion: version}
	switch {
	case version < 1:
		got.state, got.reasons = StateFailed, []Reason{ReasonSchemaVersion}
		return got
	case version > manifest.LadderTop():
		got.state, got.reasons = StateFailed, []Reason{ReasonSchemaNewer}
		return got
	}
	required, _ := manifest.RequiredSchema(version)
	present, err := tablesOf(ctx, conn)
	if err != nil {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		return failed(ReasonOpenFailed)
	}
	missing, err := schemaMisses(ctx, conn, required, present)
	if err != nil {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		return failed(ReasonOpenFailed)
	}
	if len(missing) > 0 {
		if why, ok := stopped(ctx); ok {
			return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
		}
		got.state, got.reasons = StateFailed, missing
		return got
	}
	got.counts = make(map[string]int64)
	for _, table := range coreTables {
		if !present[table] {
			continue
		}
		var n int64
		// The name is one of coreTables, a constant, never input.
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+table+`"`).Scan(&n); err != nil {
			if why, ok := stopped(ctx); ok {
				return dbCheck{state: StateIncomplete, reasons: []Reason{why}}
			}
			return failed(ReasonOpenFailed)
		}
		got.counts[table] = n
	}
	return got
}

// firstRows is the first column of stmt's rows, at most maxCheckRows+1 of
// them.
func firstRows(ctx context.Context, conn *sql.Conn, stmt string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() && len(out) <= maxCheckRows {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			// foreign_key_check's first column is the table, a string; a
			// row of any shape is a row.
			out = append(out, "?")
			continue
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// tablesOf is every table the database holds, its own SQLite tables aside.
func tablesOf(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// columnsOf collects column names, keeping query, scan and iteration errors
// separate from a schema that actually lacks a required column.
func columnsOf(ctx context.Context, conn *sql.Conn, table string) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// schemaMisses is why the database does not hold what its version requires:
// ReasonSchemaTable for a table that is not there, ReasonSchemaColumn for a
// column of one that is, each once, or none. Read errors are returned
// separately and never interpreted as missing schema.
func schemaMisses(ctx context.Context, conn *sql.Conn, required map[string][]string, present map[string]bool) ([]Reason, error) {
	var tableMissing, columnMissing bool
	names := make([]string, 0, len(required))
	for t := range required {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, table := range names {
		if !present[table] {
			tableMissing = true
			continue
		}
		have, err := columnsOf(ctx, conn, table)
		if err != nil {
			return nil, err
		}
		for _, c := range required[table] {
			if !have[c] {
				columnMissing = true
				break
			}
		}
	}
	var out []Reason
	if tableMissing {
		out = append(out, ReasonSchemaTable)
	}
	if columnMissing {
		out = append(out, ReasonSchemaColumn)
	}
	return out, nil
}

// filesCheck is the other files' verdict.
type filesCheck struct {
	state   State
	reasons []Reason
	failed  []FileFault
}

// checkFiles is the other files' check of the bundle at dir: each file m
// lists but the database present, a regular file, and the size and SHA-256
// its baseline records, read through p. A snapshot written before
// verification (legacy: no baseline and no record) is checked for presence
// alone, and reads UNVERIFIED; one with a record and no baseline for a file
// it lists has lost it, and fails (ReasonNoBaseline).
func checkFiles(ctx context.Context, dir string, m Manifest, p *pacer) filesCheck {
	legacy := m.legacy()
	var out filesCheck
	reasons := map[Reason]bool{}
	fault := func(name string, r Reason) {
		reasons[r] = true
		if !slices.Contains(capturedNames, name) {
			name = ""
		}
		out.failed = append(out.failed, FileFault{Name: name, Reason: r})
	}
	for _, name := range m.Files {
		if name == ManifestDBFileName {
			continue
		}
		if !plainName(name) {
			fault("", ReasonBadName)
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fault(name, ReasonMissing)
			continue
		case err != nil:
			fault(name, ReasonUnreadable)
			continue
		case !info.Mode().IsRegular():
			fault(name, ReasonNotRegular)
			continue
		}
		if legacy {
			continue
		}
		base, ok := m.baseline(name)
		if !ok {
			fault(name, ReasonNoBaseline)
			continue
		}
		if info.Size() != base.Size {
			fault(name, ReasonSize)
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			fault(name, ReasonUnreadable)
			continue
		}
		opened, statErr := f.Stat()
		var size int64
		var sum string
		if statErr == nil && os.SameFile(opened, info) {
			size, sum, err = hashPaced(p, f)
		} else {
			err = errors.New("the file changed as it was opened")
		}
		_ = f.Close()
		if err != nil {
			if why, ok := stopped(ctx); ok {
				return filesCheck{state: StateIncomplete, reasons: []Reason{why}}
			}
			fault(name, ReasonUnreadable)
			continue
		}
		if size != base.Size {
			fault(name, ReasonSize)
		} else if sum != base.SHA256 {
			fault(name, ReasonHash)
		}
	}
	if why, ok := stopped(ctx); ok {
		return filesCheck{state: StateIncomplete, reasons: []Reason{why}}
	}
	for _, r := range Reasons() {
		if reasons[r] {
			out.reasons = append(out.reasons, r)
		}
	}
	switch {
	case legacy:
		out.state = StateUnverified
		out.reasons = append([]Reason{ReasonNoBaseline}, out.reasons...)
	case len(out.failed) > 0:
		out.state = StateFailed
	default:
		out.state = StateVerified
	}
	return out
}

// checkBundle is a verification's check of the bundle at dir, whose manifest
// is m, at level, its reads paced and stopped by ctx: the database's verdict
// and the other files'. A snapshot written before verification is checked
// for presence alone and reads UNVERIFIED, the database never opened.
func checkBundle(ctx context.Context, dir string, m Manifest, level CheckLevel) Verification {
	start := time.Now()
	p := newPacer(ctx)
	v := Verification{Format: verificationFormat}
	if m.legacy() {
		v.DatabaseState, v.DatabaseReasons = StateUnverified, []Reason{ReasonNoBaseline}
		if slices.Contains(m.Files, ManifestDBFileName) {
			if _, err := os.Lstat(filepath.Join(dir, ManifestDBFileName)); errors.Is(err, fs.ErrNotExist) {
				v.DatabaseReasons = append(v.DatabaseReasons, ReasonMissing)
			}
		} else {
			v.DatabaseReasons = []Reason{ReasonNoDatabase}
		}
	} else {
		db := checkDatabase(ctx, dir, m, level, p)
		v.DatabaseState, v.DatabaseReasons = db.state, db.reasons
		v.UserVersion, v.Counts = db.userVersion, db.counts
		v.Level = level
	}
	if beforeFilesCheck != nil {
		beforeFilesCheck(dir)
	}
	files := checkFiles(ctx, dir, m, p)
	v.FilesState, v.FilesReasons, v.FilesFailed = files.state, files.reasons, files.failed
	now := time.Now().UTC()
	v.CheckedAt, v.FilesCheckedAt = now, now
	v.DurationMS = time.Since(start).Milliseconds()
	return v
}
