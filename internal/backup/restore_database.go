package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The database-only restore (backlog B653 PR 2b1): `bridge restore
// --database-only`, an offline, consented restore of the library database
// alone that keeps the files it replaces. It stands beside the full restore
// (RestoreWithOptions), never through it, and restores nothing else of the
// snapshot: the tokens, the TLS pair, bridge.yaml, the console account, the
// player's files and the library are left as they are, and the harvest's
// sync position alone is rewound, since the restored library is a new
// history.
//
// Phase B prepares a copy and changes no original: the snapshot's database,
// VERIFIED as every reader reads it, is copied into the data dir beside the
// live one (manifest.RestoreStagingPath), its bytes held to the baseline its
// verification checked, migrated through the store's own open, given a new
// backup epoch with the deletion journal's coverage restarted
// (manifest.PrepareRestoredDatabase), checkpointed, checked read-only, and
// hashed. Phase C swaps it in under a journal (restore_journal.go): every
// original hashed and synced (C0), a folder of its own made for them under
// restore-originals/ and its record moved in by the no-replace rename (C1),
// the journal published (C2), the sidecars and then the database moved there
// (C3, C4), the prepared copy renamed in and checked by its content (C5),
// `installed` recorded (C5′), and the harvest's sync position rewound
// durably before the journal goes (C6). Every rename is the platform's own
// no-replace rename, and a filesystem that refuses to sync a directory is
// refused before anything moves. A failure in C3, C4 or C5's rename puts the
// originals back (putBack), the one routine `bridge restore --database-only`
// runs with a journal standing.
//
// Its locks, in order (§4.7): the data dir's (its caller's), the store's
// lifetime lock exclusively (counted on its caller's), the snapshot folder's
// (holdSource); for the admission before the copy, the install marker's and
// then bridge.yaml's, both let go before the preparation, whose own open
// takes the staged copy's lifetime and ladder locks and, for a pending step,
// the marker's; then the marker's again, held to C5′, and bridge.yaml's
// again, held to C5′ too, under which the record is made and the swap runs.

// DatabaseOptions are RestoreDatabase's.
type DatabaseOptions struct {
	// Context stops the restore until its swap begins (C1), and nothing after.
	Context context.Context
	// LockWait is the wait for the snapshot folder's lock: VerifyCmdLockWait
	// where 0.
	LockWait time.Duration
	// Full checks the prepared copy with integrity_check and
	// foreign_key_check, quick_check otherwise.
	Full bool
	// ConfigPath is the live bridge.yaml as its caller resolved it, whose
	// turn the restore takes; required.
	ConfigPath string
	// ConfigAsRead is its bytes as the caller read them before its prompt.
	ConfigAsRead []byte
	// Warn says what the restore says beside its answer.
	Warn func(string)
	// HarvestState is the harvest state the restore rewinds:
	// <dataDir>/atlas-harvest.json where empty.
	HarvestState string
}

// DatabaseResult is what a database-only restore did.
type DatabaseResult struct {
	// Archive is the run's folder of originals, absolute.
	Archive string
	// Originals is what was moved there: each original's name, presence,
	// size and SHA-256.
	Originals []FileFact
	// Verdict is the snapshot's record as read under its folder's lock: the
	// database's VERIFIED, the files' whatever it is (reported, never
	// required).
	Verdict Verification
}

// ErrDatabaseNotVerified is the database-only restore's refusal of a
// snapshot whose database is not VERIFIED as every reader reads it.
var ErrDatabaseNotVerified = errors.New("the snapshot's database is not a verified recovery point")

// RestoreDatabase restores the library database alone from the snapshot at
// snapshotDir into dataDir, keeping the files it replaces (see above). A
// refusal before the swap restores nothing and leaves every original as it
// was; a failure in the swap puts them back; a stop names what stopped it
// and leaves the journal for `bridge restore --database-only`.
func RestoreDatabase(snapshotDir, dataDir string, opts DatabaseOptions) (res DatabaseResult, err error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	wait := opts.LockWait
	if wait == 0 {
		wait = VerifyCmdLockWait
	}
	warn := opts.Warn
	if warn == nil {
		warn = func(s string) { warnRestore(RestoreOptions{}, s) }
	}
	if opts.ConfigPath == "" {
		return res, errors.New("the database-only restore needs the bridge's config, to compare its library form; nothing was restored")
	}
	harvest := opts.HarvestState
	if harvest == "" {
		harvest = harvestStateIn(dataDir)
	}
	dbPath := manifest.DefaultDBPath(dataDir)
	storeHold, err := manifest.HoldStoreExclusive(dbPath)
	if err != nil {
		return res, fmt.Errorf("%w; nothing was restored", err)
	}
	defer storeHold.Release()
	if err := manifest.CheckRestoreJournal(dbPath); err != nil {
		return res, fmt.Errorf("%w; nothing was restored", err)
	}
	// A filesystem that refuses to sync a directory is refused before
	// anything is staged or moved: the journal's order before the renames it
	// guards rests on the barrier.
	if err := probeRestoreBarrier(dataDir); err != nil {
		return res, err
	}
	src, err := holdSource(ctx, snapshotDir, wait)
	if err != nil {
		return res, err
	}
	defer src.letGo()
	defer func() { err = src.asGiven(err) }()
	snap, m := src.snap, src.m
	base, ok := m.baseline(ManifestDBFileName)
	if !slices.Contains(m.Files, ManifestDBFileName) || !ok {
		return res, fmt.Errorf("the snapshot %s lists no database with a baseline, so it cannot be restored alone; nothing was restored", snapshotDir)
	}
	if res.Verdict, err = admitDatabaseRestore(snap, m, dbPath, opts); err != nil {
		return res, err
	}

	// B6: what an earlier run left at the staged names, by name; from here
	// until the journal stands, a failure removes what this run staged.
	if err := removeStaged(dataDir); err != nil {
		return res, fmt.Errorf("remove what an earlier restore staged in the data directory: %w; nothing was restored", err)
	}
	journaled := false
	defer func() {
		if !journaled {
			if rerr := removeStaged(dataDir); rerr != nil {
				warn(fmt.Sprintf("restore: a staged file was left in the data directory (%v), which the next restore removes", rerr))
			}
		}
	}()
	stage := manifest.RestoreStagingPath(dataDir)
	if err := stageSnapshotDatabase(ctx, filepath.Join(snap, ManifestDBFileName), dataDir, base); err != nil {
		return res, err
	}
	switch v, err := snapshotUserVersion(stage); {
	case err != nil:
		return res, fmt.Errorf("read the staged database's schema: %w; nothing was restored", err)
	case v < 1 || v > manifest.LadderTop():
		return res, fmt.Errorf("the snapshot's database is at schema %d, which this bridge does not take (1 to %d); nothing was restored", v, manifest.LadderTop())
	}
	hist, err := manifest.PrepareRestoredDatabase(ctx, dataDir)
	if err != nil {
		return res, fmt.Errorf("prepare the staged database: %w; nothing was restored", err)
	}
	if err := dropStagedSidecars(stage); err != nil {
		return res, err
	}
	step("B9")

	// B11a: the marker's admission again, held to C5′; B10 under it; then
	// bridge.yaml's turn again (B11b), the config and the root form compared
	// again, and the store change recorded.
	marker, err := holdRestoreMarker(dbPath)
	if err != nil {
		return res, err
	}
	releaseMarker := marker.Release
	defer releaseMarker()
	prepared, err := checkPrepared(ctx, stage, dataDir, opts.Full, hist)
	if err != nil {
		return res, err
	}
	turn, err := config.LockFile(opts.ConfigPath)
	if err != nil {
		return res, fmt.Errorf("take bridge.yaml's turn: %w; nothing was restored", err)
	}
	releaseConfig := turn.Unlock
	defer releaseConfig()
	if err := configStillAsRead(turn, opts.ConfigAsRead); err != nil {
		return res, err
	}
	if err := rootFormOfTurn(snap, m, turn); err != nil {
		return res, err
	}
	if err := recordRestoredStore(marker); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, fmt.Errorf("the restore was stopped before it moved anything: %w; nothing was restored", err)
	}
	j := &RestoreJournal{
		Format: journalFormat, StartedAt: time.Now().UTC(), BridgeVersion: version.ServerVersion, Phase: PhaseForward,
		Prepared: prepared,
		Snapshot: SnapshotNote{Given: src.given, Abs: snap, Folder: filepath.Base(snap)},
	}
	if j.ID, err = randomID(); err != nil {
		return res, err
	}
	return swapDatabase(dataDir, j, harvest, releaseConfig, releaseMarker, warn, res, &journaled)
}

// admitDatabaseRestore is the admission before anything is copied (B5, then
// B2 to B4): the install marker's lock held and its admission made, writing
// nothing; under it bridge.yaml's turn, the config compared with the bytes
// its caller read and with what the turn found; the snapshot's database
// VERIFIED as every reader reads it (readVerdict, its veto and its folder
// asked), its files' verdict reported and never required; and the library's
// form the snapshot's verified settings give compared with the live one's.
// Both locks are let go before it answers, the turn first: neither may span
// the preparation, whose open can take the marker's lock itself.
func admitDatabaseRestore(snap string, m Manifest, dbPath string, opts DatabaseOptions) (Verification, error) {
	marker, err := holdRestoreMarker(dbPath)
	if err != nil {
		return Verification{}, err
	}
	defer marker.Release()
	turn, err := config.LockFile(opts.ConfigPath)
	if err != nil {
		return Verification{}, fmt.Errorf("take bridge.yaml's turn: %w; nothing was restored", err)
	}
	defer turn.Unlock()
	if err := configStillAsRead(turn, opts.ConfigAsRead); err != nil {
		return Verification{}, err
	}
	v := snapshotVerdict(snap, m)
	if v.DatabaseState != StateVerified {
		return v, fmt.Errorf("%w: its record reads %s (%s); restore it whole, or choose a snapshot whose database is verified; nothing was restored",
			ErrDatabaseNotVerified, v.DatabaseState, reasonWords(v.DatabaseReasons))
	}
	if err := rootFormOfTurn(snap, m, turn); err != nil {
		return v, err
	}
	return v, nil
}

// snapshotVerdict is the snapshot at snap's record as every reader reads it
// (readVerdict): its veto asked, its owner probed where the record holds a
// PENDING, and bound to the folder's name.
func snapshotVerdict(snap string, m Manifest) Verification {
	own := OwnerNone
	if m.hasPending() {
		own = probeOwner(snap)
	}
	return readVerdict(m, time.Now(), vetoedAt(snap), own, filepath.Base(snap))
}

// reasonWords is reasons as one list of codes.
func reasonWords(reasons []Reason) string {
	if len(reasons) == 0 {
		return "no reason recorded"
	}
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += ", "
		}
		out += string(r)
	}
	return out
}

// rootFormOfTurn is the root form's check (checkRootForm) against the live
// config as the turn found it.
func rootFormOfTurn(snap string, m Manifest, turn *config.FileLock) error {
	found, ok := turn.Found()
	if !ok {
		return fmt.Errorf("the bridge's config %s is not there to compare the library's form with; nothing was restored", turn.Path())
	}
	return checkRootForm(snap, m, turn.Path(), found)
}

// stageSnapshotDatabase is B7: the snapshot's database at srcPath copied into
// a temporary file of its own in dataDir, the context asked between chunks
// and the bytes hashed as they stream, given the live database's owner and
// mode (else the data dir's owner, and 0644), required to be the bytes its
// verification checked (base), synced, and renamed onto the staging name by
// the no-replace rename, where a platform that has none refuses before any
// original moves.
func stageSnapshotDatabase(ctx context.Context, srcPath, dataDir string, base Baseline) error {
	stage := manifest.RestoreStagingPath(dataDir)
	live := manifest.DefaultDBPath(dataDir)
	mode := os.FileMode(0o644)
	if fi, err := os.Lstat(live); err == nil && fi.Mode().IsRegular() {
		mode = fi.Mode().Perm()
	}
	var in *os.File
	err := atomicwrite.ReadWithRetryCtx(ctx, func() (err error) {
		in, _, err = fsutil.OpenAsFile(srcPath)
		return err
	})
	if err != nil {
		return fmt.Errorf("open the snapshot's database: %w; nothing was restored", err)
	}
	defer in.Close()
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, filepath.Base(stage)+".tmp-"+id)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	defer func() { _ = out.Close() }()
	if err := fsutil.KeepOwner(out, live); err != nil {
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("the restore was stopped while it copied the snapshot's database: %w; nothing was restored", err)
		}
		r, rerr := in.Read(buf)
		if r > 0 {
			if _, err := out.Write(buf[:r]); err != nil {
				return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
			}
			h.Write(buf[:r])
			n += int64(r)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read the snapshot's database: %w; nothing was restored", rerr)
		}
	}
	if n != base.Size || hex.EncodeToString(h.Sum(nil)) != base.SHA256 {
		return fmt.Errorf("the snapshot's database is not the one its verification checked (%d bytes, its baseline %d): it changed since; nothing was restored", n, base.Size)
	}
	if err := out.Chmod(mode); err != nil {
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	if err := fsutil.RenameNoReplace(tmp, stage); err != nil {
		if errors.Is(err, fsutil.ErrNoReplaceUnsupported) {
			return fmt.Errorf("this filesystem has no rename that never replaces, which the restore moves every file by (%w); nothing was restored", err)
		}
		return fmt.Errorf("stage the snapshot's database: %w; nothing was restored", err)
	}
	committed = true
	return restoreBarrier(dataDir, dataDir)
}

// dropStagedSidecars requires, once the staged copy is prepared and closed,
// its WAL absent or empty, the prepared copy being its main file alone, and
// removes an empty WAL and the -shm; a WAL that holds frames refuses, the
// staged set removed whole, so the WAL is never taken from its database.
func dropStagedSidecars(stage string) error {
	wal := stage + "-wal"
	fi, err := os.Lstat(wal)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read the staged database's WAL: %w; nothing was restored", err)
	case !fi.Mode().IsRegular() || fi.Size() != 0:
		return fmt.Errorf("the staged database's WAL still holds what its preparation wrote; nothing was restored")
	default:
		if err := os.Remove(wal); err != nil {
			return fmt.Errorf("remove the staged database's empty WAL: %w; nothing was restored", err)
		}
	}
	if err := os.Remove(stage + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the staged database's -shm: %w; nothing was restored", err)
	}
	return nil
}

// checkPrepared is B10: the prepared copy checked read-only through the
// snapshot reader (checkSQLite: quick_check, or integrity_check and
// foreign_key_check with full; the schema this bridge's ladder top requires),
// its epoch and coverage read back and compared with what the preparation
// wrote, then its size and SHA-256 hashed through one handle, which syncs
// it, and the data dir synced. It opens no store.
func checkPrepared(ctx context.Context, stage, dataDir string, full bool, hist manifest.RestoredHistory) (PreparedFact, error) {
	level := CheckQuick
	if full {
		level = CheckFull
	}
	f, _, err := fsutil.OpenAsFile(stage)
	if err != nil {
		return PreparedFact{}, fmt.Errorf("open the prepared database: %w; nothing was restored", err)
	}
	got := checkSQLite(ctx, f, level, &pacer{ctx: ctx})
	if got.state != StateVerified || got.userVersion != manifest.LadderTop() {
		_ = f.Close()
		return PreparedFact{}, fmt.Errorf("the prepared database did not pass its check (%s, %s, schema %d); nothing was restored",
			got.state, reasonWords(got.reasons), got.userVersion)
	}
	db, closeReader, err := openSnapshotReader(f, &pacer{ctx: ctx})
	if err != nil {
		_ = f.Close()
		return PreparedFact{}, fmt.Errorf("open the prepared database: %w; nothing was restored", err)
	}
	back, err := manifest.ReadRestoredHistory(ctx, db)
	closeReader()
	_ = f.Close()
	if err != nil {
		return PreparedFact{}, fmt.Errorf("read the prepared database's history back: %w; nothing was restored", err)
	}
	if back != hist {
		return PreparedFact{}, errors.New("the prepared database does not hold the history its preparation wrote; nothing was restored")
	}
	at, err := readAndSyncOriginal(stage)
	if err != nil {
		return PreparedFact{}, fmt.Errorf("hash the prepared database: %w; nothing was restored", err)
	}
	if err := restoreBarrier(dataDir, dataDir); err != nil {
		return PreparedFact{}, fmt.Errorf("%w; nothing was restored", err)
	}
	return PreparedFact{Size: at.size, SHA256: at.sha256}, nil
}

// swapDatabase is Phase C (§4.3): from its first step on the context stops
// nothing. A refusal before the journal is published (C0, C1, a publication
// that did not land) moves nothing, and its caller removes the staged copy;
// once the journal stands (journaled), the staged copy is the journal's: a
// failure of a rename in C3, C4 or C5 puts the originals back, the config's
// turn and the marker's lock let go first, and a stop keeps the journal.
func swapDatabase(dataDir string, j *RestoreJournal, harvest string, releaseConfig, releaseMarker func(), warn func(string), res DatabaseResult,
	journaled *bool) (DatabaseResult, error) {
	// C0: every original read whole, hashed and synced where it stands.
	for _, name := range originalNames() {
		at, err := readAndSyncOriginal(filepath.Join(dataDir, name))
		if err != nil {
			return res, fmt.Errorf("%w; nothing was restored", err)
		}
		f := FileFact{Name: name}
		if at.kind == entryFile {
			f.Present, f.Size, f.SHA256 = true, at.size, at.sha256
		}
		j.Originals = append(j.Originals, f)
	}
	step("C0")
	// C1: the run's folder of originals, and its record moved in.
	archive, err := makeRunFolder(dataDir)
	if err != nil {
		return res, err
	}
	j.Archive = OriginalsDirName + "/" + filepath.Base(archive)
	res.Archive, res.Originals = archive, j.Originals
	record := *j
	record.Phase = ""
	staged, err := writeStagedJSON(dataDir, filepath.Join(archive, originalsRecordName), &record)
	if err == nil {
		if err = fsutil.RenameNoReplace(staged, filepath.Join(archive, originalsRecordName)); err != nil {
			_ = os.Remove(staged)
		}
	}
	if err == nil {
		err = restoreBarrier(dataDir, archive)
	}
	if err != nil {
		_ = os.Remove(filepath.Join(archive, originalsRecordName))
		_ = os.Remove(archive)
		if errors.Is(err, fsutil.ErrNoReplaceUnsupported) {
			return res, fmt.Errorf("this filesystem has no rename that never replaces between the data directory and %s (%w); nothing was restored", OriginalsDirName, err)
		}
		return res, fmt.Errorf("record the originals in %s: %w; nothing was restored", archive, err)
	}
	step("C1")
	// C2: the journal, from which every opener of the live database refuses.
	if err := publishJournal(dataDir, j); err != nil {
		if errors.Is(err, os.ErrNotExist) || !journalStands(dataDir) {
			_ = os.Remove(filepath.Join(archive, originalsRecordName))
			_ = os.Remove(archive)
			return res, fmt.Errorf("%w; nothing was restored", err)
		}
		*journaled = true
		return res, &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
	}
	*journaled = true
	step("C2")
	putBackAfter := func(cause error) (DatabaseResult, error) {
		releaseConfig()
		releaseMarker()
		if err := putBack(dataDir, j, warn); err != nil {
			return res, fmt.Errorf("the restore of the library database failed (%v), and putting its originals back stopped: %w", cause, err)
		}
		return res, fmt.Errorf("the restore of the library database failed (%w); its originals were put back, and nothing was restored", cause)
	}
	// C3, then C4: the sidecars, then the database, into the run's folder.
	for _, group := range [][]string{sidecarOriginalNames(), {originalNames()[0]}} {
		moved := false
		for _, name := range group {
			if !j.Original(name).Present {
				continue
			}
			if err := swapRename(filepath.Join(dataDir, name), filepath.Join(archive, name)); err != nil {
				return putBackAfter(fmt.Errorf("move %s into %s: %w", name, archive, err))
			}
			moved = true
			step(swapStepOf(name))
		}
		if !moved {
			continue
		}
		if err := restoreBarrier(dataDir, archive); err != nil {
			return putBackAfter(err)
		}
		if err := restoreBarrier(dataDir, dataDir); err != nil {
			return putBackAfter(err)
		}
		step(swapStepOf(group[0]) + ":synced")
	}
	// C5: the prepared copy renamed in, no sidecar beside it before or after.
	dbName := originalNames()[0]
	live := filepath.Join(dataDir, dbName)
	stage := manifest.RestoreStagingPath(dataDir)
	if side := liveSidecar(dataDir); side != "" {
		return res, &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf("%s appeared at its name while the originals were moved out; remove it, then run `bridge restore --database-only` again", side)}
	}
	if err := swapRename(stage, live); err != nil {
		if _, lerr := os.Lstat(live); lerr == nil {
			return res, &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf("an entry appeared at %s while its original was moved out, and the prepared copy was not renamed over it: %v", dbName, err)}
		}
		return putBackAfter(fmt.Errorf("rename the prepared copy in: %w", err))
	}
	if err := restoreBarrier(dataDir, dataDir); err != nil {
		return res, &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
	}
	step("C5")
	if side := liveSidecar(dataDir); side != "" {
		if err := fsutil.RenameNoReplace(live, stage); err != nil {
			return res, &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf("%s appeared beside the installed copy, and sending the copy back to %s failed (%v); remove %s and whatever stands at %s, then run `bridge restore --database-only` again",
				side, filepath.Base(stage), err, side, filepath.Base(stage))}
		}
		_ = restoreBarrier(dataDir, dataDir)
		return res, &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf("%s appeared beside the installed copy, which went back to %s; remove %s, then run `bridge restore --database-only` again", side, filepath.Base(stage), side)}
	}
	if at, err := readEntry(live); err != nil || !at.matches(j.Prepared.Size, j.Prepared.SHA256) {
		return res, &RestoreStoppedError{Phase: j.Phase, Why: fmt.Sprintf("%s does not hold the prepared copy once renamed in", dbName)}
	}
	// C5′: installed, durably; the config's turn, then the marker's lock, go.
	if err := setJournalPhase(dataDir, j, PhaseInstalled); err != nil {
		return res, &RestoreStoppedError{Phase: j.Phase, Why: err.Error()}
	}
	step("C5'")
	releaseConfig()
	releaseMarker()
	if err := completeRestore(dataDir, j, harvest, warn); err != nil {
		return res, err
	}
	return res, nil
}

// swapStepOf names the step at which the original name moved out: C4 for
// the database, C3 and the name for a sidecar.
func swapStepOf(name string) string {
	if name == originalNames()[0] {
		return "C4"
	}
	return "C3:" + name
}

// swapFault, nil in production, answers before each rename of the swap that
// moves an original out or the prepared copy in (C3 to C5), and an error it
// returns is that rename's (SetSwapFaultForTest): a failure there puts the
// originals back.
var swapFault atomic.Pointer[func(from, to string) error]

// swapRename is the swap's rename: the no-replace rename, unless a test's
// fault answers first.
func swapRename(from, to string) error {
	if fn := swapFault.Load(); fn != nil {
		if err := (*fn)(from, to); err != nil {
			return err
		}
	}
	return fsutil.RenameNoReplace(from, to)
}

// SetSwapFaultForTest installs fn as the swap's fault, and returns what puts
// the one before it back. Process-global: a test that uses it must not call
// t.Parallel().
func SetSwapFaultForTest(fn func(from, to string) error) (restore func()) {
	var p *func(from, to string) error
	if fn != nil {
		p = &fn
	}
	prev := swapFault.Swap(p)
	return func() { swapFault.Store(prev) }
}

// journalStands reports whether anything stands at the journal's name.
func journalStands(dataDir string) bool {
	_, err := os.Lstat(journalPath(dataDir))
	return !errors.Is(err, os.ErrNotExist)
}

// liveSidecar names the first sidecar that stands at its live name, "" where
// none does: an entry there of any kind, or one Lstat cannot tell is not.
func liveSidecar(dataDir string) string {
	for _, name := range sidecarOriginalNames() {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			return name
		}
	}
	return ""
}

// makeRunFolder makes the run's folder of originals (C1): restore-originals/
// where it is not there (a link or another kind there refuses), its parent
// synced, then a folder named by the restore's UTC time, made exclusively,
// with a count where that name is taken, and restore-originals/ synced.
func makeRunFolder(dataDir string) (string, error) {
	root := filepath.Join(dataDir, OriginalsDirName)
	fi, err := os.Lstat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := fsutil.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("make %s: %w; nothing was restored", root, err)
		}
		if err := restoreBarrier(dataDir, dataDir); err != nil {
			return "", fmt.Errorf("%w; nothing was restored", err)
		}
		if fi, err = os.Lstat(root); err != nil {
			return "", fmt.Errorf("%w; nothing was restored", err)
		}
	case err != nil:
		return "", fmt.Errorf("read %s: %w; nothing was restored", root, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory (a link or a file stands there); move it aside, then restore again; nothing was restored", root)
	}
	stamp := time.Now().UTC().Format(timestampLayout)
	for n := 1; n < 10000; n++ {
		name := stamp
		if n > 1 {
			name = fmt.Sprintf("%s-%d", stamp, n)
		}
		dir := filepath.Join(root, name)
		err := fsutil.Mkdir(dir, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("make %s: %w; nothing was restored", dir, err)
		}
		if err := restoreBarrier(dataDir, root); err != nil {
			_ = os.Remove(dir)
			return "", fmt.Errorf("%w; nothing was restored", err)
		}
		return dir, nil
	}
	return "", fmt.Errorf("no free name for this restore's folder in %s; nothing was restored", root)
}

// SourceLook is bridge restore's look at a snapshot before its prompt
// (CheckRestoreSource): the manifest it read, and the record as every reader
// reads it.
type SourceLook struct {
	Manifest Manifest
	Verdict  Verification
}

// DatabaseRestoreRefusal is the database-only restore's refusal, before its
// prompt, of what the look found (§4.1 step 4): a schema this bridge does
// not take, a manifest that lists no database with a baseline, and a
// database whose record does not read VERIFIED (a VERIFIED beside its veto
// reads INCOMPLETE, unsynced). "" where it may go on to the prompt.
func DatabaseRestoreRefusal(look SourceLook) string {
	m := look.Manifest
	if m.SchemaVersion < 1 || m.SchemaVersion > SchemaVersion {
		return fmt.Sprintf("the snapshot's schema v%d is not one this bridge supports (v1 to v%d)", m.SchemaVersion, SchemaVersion)
	}
	if _, ok := m.baseline(ManifestDBFileName); !ok || !slices.Contains(m.Files, ManifestDBFileName) {
		return "the snapshot lists no database with a baseline, so it cannot be restored alone"
	}
	if v := look.Verdict; v.DatabaseState != StateVerified {
		return fmt.Sprintf("the snapshot's database is not a verified recovery point: its record reads %s (%s)", v.DatabaseState, reasonWords(v.DatabaseReasons))
	}
	return ""
}
