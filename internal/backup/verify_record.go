package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A verdict replaces the record a snapshot's manifest holds in one write,
// under the backups folder's lock, so the lock's holder is the record's one
// writer: the manifest is read again, the verdict merged into its record
// (mergeVerdict), and the manifest staged beside itself and renamed over
// itself through atomicwrite (RenameWithRetryUnsyncedCtx), then its folder
// synced (syncVerdictDir). That last sync can fail once the rename has
// landed, and then every reader already reads the new record while the write
// answers an error; and a write that fails before its rename leaves the
// record it would have replaced. Every rename attempt and veto removal
// checks the give-up immediately before its call (publication). A give-up
// between check and call still lets that operation run, and an operation
// already begun finishes afterward. A verdict's write is admitted only
// before its hold is given up (publishOf, hold.go).
//
// A veto disqualifies a VERIFIED that is in doubt (backlog B653,
// and its second to fourth rounds), by one veto, vetoFileName in the bundle,
// or beside it in the backups folder (rootVetoPath) where the bundle takes no
// file, which every reader asks (vetoedAt) and beside which it reads a
// VERIFIED as INCOMPLETE (ReasonUnsynced):
//   - before a verification checks a snapshot whose record holds a VERIFIED,
//     it puts the veto in place and syncs its folder (setAside), so that
//     neither a write that then fails, nor a crash at any point after, a
//     write's lost rename included, leaves a reader the VERIFIED this check
//     may disprove; where no veto can be put in place durably, the snapshot
//     is not checked again (ErrRecheckRefused), and its record, which no
//     check has disproved, stays as it was; and where no directory can be
//     synced at all (fsutil.SyncsParentDir false: Windows), the check runs
//     without a prior veto and its verdict is not recorded over the VERIFIED
//     (ErrRecheckNotRecorded). A completed check that disproves a previously
//     VERIFIED component leaves a veto before read-back, without a crash
//     durability promise. If neither location can be written the error says
//     so; the immediate prune is skipped, but a later process can still
//     trust the stale VERIFIED. An INCOMPLETE component disproves nothing;
//   - a write of a record that holds a VERIFIED, or that replaces one, needs
//     the veto too, and with none to be had writes nothing;
//   - the veto goes only once the record that replaces it is durable (the
//     folder's sync after the rename has answered), and only where a check
//     that ended earns it (earnsClear): never for an INCOMPLETE. Every
//     removal checks both the write and hold give-up before the call, with
//     the check/call gap and late completion described above. Removal of
//     the last effective veto exposes what the completed check found and
//     the write made durable; a veto that stood
//     before the verification began stays over a VERIFIED the record carries
//     over.
//
// A veto made whose owner could not be given (root_squash) is kept as it is:
// its presence alone is the veto, and the service may remove it. Each change
// of the veto, and each write, moves the backups folder's modification time
// (stampChange), so a reader that caches what the folder holds (the console's
// Jobs card) reads it again. A give-up skips putVeto's stamp and reports that
// the console may keep its cached reading until expiry or a folder change.
// Where no directory can be synced, a write is
// atomic and is not known to be durable.

// syncVerdictDir is the barrier a verdict's write runs once its rename has
// landed, given the manifest's path: fsutil.SyncParentDir. A test seam
// (SetSyncVerdictForTest).
var syncVerdictDir = fsutil.SyncParentDir

// syncVetoDir is the barrier that makes the veto of a snapshot about to be
// checked again durable, given the veto's path: fsutil.SyncParentDir. A test
// seam (SetSyncVetoForTest).
var syncVetoDir = fsutil.SyncParentDir

// syncsParentDir says whether syncVetoDir is a barrier at all here:
// fsutil.SyncsParentDir, false where fsutil.SyncParentDir is a no-op
// (Windows), and a test's stand-in (SetSyncsParentDirForTest).
var syncsParentDir = fsutil.SyncsParentDir

// failStage, failRename and failVeto, nil in production, answer the failure
// a test puts at a verdict's staging, its rename, or the making of a veto
// at the path given (SetVerdictWriteFailuresForTest).
var (
	failStage  func(dir string) error
	failRename func(path string) error
	failVeto   func(path string) error
)

// mergeVerdict is the record a verdict found leaves when it is written over
// stored: each verdict replaces stored's unless the stored one says more.
// A check that did not end (INCOMPLETE) replaces only a PENDING or an
// INCOMPLETE; a FAILED replaces anything; a database VERIFIED replaces
// anything but a completed check (VERIFIED or FAILED) of a higher level, so
// a quick check never undoes a full one unless it fails; the files' check,
// which is the same at every level, replaces a completed one with the
// latest. The record's PENDING time goes once nothing in it is PENDING. It
// answers too which of found's two verdicts replaced stored's (database,
// files): where both did, the record is wholly what the check found, and
// carries nothing over.
func mergeVerdict(stored *Verification, found Verification) (out Verification, database, files bool) {
	if stored == nil {
		out = found
		out.Format = verificationFormat
		return out, true, true
	}
	out = *stored
	out.Format = verificationFormat
	replaces := func(old, s State) bool {
		if s == StateIncomplete {
			return old == StatePending || old == StateIncomplete || !known(old)
		}
		return s != StatePending
	}
	database = replaces(stored.DatabaseState, found.DatabaseState) &&
		!(found.DatabaseState == StateVerified && completed(stored.DatabaseState) && stored.Level.rank() > found.Level.rank())
	if database {
		out.DatabaseState, out.DatabaseReasons = found.DatabaseState, found.DatabaseReasons
		out.Level, out.CheckedAt, out.DurationMS = found.Level, found.CheckedAt, found.DurationMS
		out.UserVersion, out.Counts = found.UserVersion, found.Counts
	}
	files = replaces(stored.FilesState, found.FilesState)
	if files {
		out.FilesState, out.FilesReasons, out.FilesFailed = found.FilesState, found.FilesReasons, found.FilesFailed
		out.FilesCheckedAt = found.FilesCheckedAt
	}
	if out.DatabaseState != StatePending && out.FilesState != StatePending {
		out.PendingSince = time.Time{}
	}
	return out, database, files
}

// completed reports whether s is the verdict of a check that ended.
func completed(s State) bool { return s == StateVerified || s == StateFailed }

// earnsClear says whether the verdict found, merged over base (database and
// files saying which of its two verdicts replaced base's: mergeVerdict),
// earns the removal of the veto beside the record (backlog B653's fifth
// round): never where found holds an INCOMPLETE, which no check ended, so a
// check stopped, past its deadline or that panicked leaves the veto it made
// before it; where both of found's verdicts replaced base's, the record is
// wholly what the check found; and where base's is carried over, only where
// no veto stood before the verification began (doubted false) and each
// verdict carried over is a VERIFIED that found matched with a VERIFIED of
// its own (a quick check over a full one), so a veto a failed write left
// stays over a VERIFIED a later verification carries over.
func earnsClear(found Verification, base *Verification, database, files, doubted bool) bool {
	if found.DatabaseState == StateIncomplete || found.FilesState == StateIncomplete {
		return false
	}
	if database && files {
		return true
	}
	matched := func(kept, s State) bool { return kept == StateVerified && s == StateVerified }
	return !doubted && base != nil &&
		(database || matched(base.DatabaseState, found.DatabaseState)) &&
		(files || matched(base.FilesState, found.FilesState))
}

// known reports whether s is a state a record holds.
func known(s State) bool {
	switch s {
	case StatePending, StateVerified, StateFailed, StateIncomplete, StateUnverified:
		return true
	}
	return false
}

// holdsVerified reports whether v holds a VERIFIED state in either verdict.
func holdsVerified(v *Verification) bool {
	return v != nil && (v.DatabaseState == StateVerified || v.FilesState == StateVerified)
}

// ErrVerdictNotDurable marks a verdict's write whose folder sync failed once
// its rename had landed: the record holds the verdict, behind its veto
// while it holds a VERIFIED, and whether it survives a crash is not known.
var ErrVerdictNotDurable = errors.New("the verdict was written and is not known to be durable")

// ErrRecheckRefused marks a verification that did not check its snapshot:
// the snapshot's record holds a VERIFIED, and no veto could be put in place
// durably beside it first (setAside), so a check that disproved it could not
// be kept from a reader. Nothing was checked or written; the record, which no
// check has disproved, is as it was.
var ErrRecheckRefused = errors.New("the snapshot was not checked again: its record holds a verified verdict, and no veto could be put in place durably before the check")

// ErrRecheckNotRecorded marks a verification of a snapshot whose record holds
// a VERIFIED where no directory can be synced (syncsParentDir false:
// Windows), so no veto can be made durable. The check runs and its verdict
// is not recorded. A completed finding that disproves a VERIFIED component
// vetoes the unchanged record before read-back; failure to make that veto
// is reported beside this error. An INCOMPLETE component alone leaves it alone.
var ErrRecheckNotRecorded = errors.New("no check of a snapshot whose record holds a verified verdict is recorded here: no directory can be synced, so no veto beside the record would be durable")

// setAside puts the veto of the snapshot in dir in place, durably, before a
// recheck: in the bundle first, then beside it if that fails. With neither
// veto durable it removes those made here and refuses (ErrRecheckRefused).
// Each creation attempt and cleanup removal checks p's step AND hold give-up
// immediately before the call, including when stepVeto registered after the
// hold was given up. These checks and filesystem calls are not one atomic
// admission: a give-up between a check and its call still lets that operation
// run. A veto creation or removal already begun when the give-up lands also
// finishes afterward. Successful removal of the last effective veto restores
// the earlier check's record as it stood before this recheck; a remaining
// veto (or failed removal) keeps the conservative reading. Read-back is only
// what the holder observed then, not a promise about a late operation.
func setAside(dir string, p publication) error {
	root := filepath.Dir(dir)
	var madeHere []string
	var causes []error
	for _, c := range []struct{ path, ref string }{{filepath.Join(dir, vetoFileName), dir}, {rootVetoPath(dir), root}} {
		if p.givenUp() {
			causes = append(causes, ErrCheckGivenUp)
			break
		}
		made, err := writeVeto(c.path, c.ref)
		if made {
			madeHere = append(madeHere, c.path)
			// A reader's VERIFIED reads INCOMPLETE from now on.
			stampChange(root)
		}
		if err != nil {
			causes = append(causes, err)
			continue
		}
		if err := syncVetoDir(c.path); err != nil {
			causes = append(causes, fmt.Errorf("sync the veto: %w", err))
			continue
		}
		return nil
	}
	removed := false
	for _, path := range madeHere {
		if p.givenUp() {
			// No next removal once this check observes the give-up.
			break
		}
		_ = removeVeto(path)
		removed = true
	}
	if removed {
		stampChange(root)
	}
	return fmt.Errorf("%w: %w", ErrRecheckRefused, errors.Join(causes...))
}

// publication carries the give-up checks for publishVerdict and setAside:
// given, the give-up of its step (stepOf, publishOf), which
// its rename asks before every attempt, and held, its hold's give-up
// (hold.gaveUp), which every removal of a veto asks beside given, since a
// step begun after its hold was given up (the write of an INCOMPLETE) is
// given a signal no earlier give-up ended. setAside asks both before each
// veto creation attempt too. Neither is ever the caller's stop.
type publication struct {
	given context.Context
	held  func() bool
	// recovery is the INCOMPLETE step registered after publication admission
	// was refused. Only its post-rename stamp uses the fresh step's signal
	// alone; every veto operation still checks both signals.
	recovery bool
}

// givenUp reports whether the write was given up, its step or its hold.
func (p publication) givenUp() bool {
	return p.given.Err() != nil || (p.held != nil && p.held())
}

// errVetoKept says a removal of a veto was not begun, its write given up
// (removeVetoes): the veto stays.
var errVetoKept = errors.New("the veto was kept: its write was given up")

// publishVerdict writes found into the record of the snapshot at dir, and
// answers the record as it now stands and whether it was written: found
// merged with prior, the record as it stood before its check (a
// verification's own, read under the same hold of the lock), or, for a nil
// prior, with the record its manifest holds now. Its caller holds the backups
// folder's lock. A merge that changes nothing writes nothing.
//
// p is what the write asks of its give-up (publication): its step before
// each rename attempt, and its step and hold before each veto removal,
// including after the unchanged record's veto lookup. Each check is directly
// before the call, without atomic admission: a give-up in that gap still
// lets the call run. A rename attempt or removal already begun when the
// give-up lands finishes afterward. Once the last effective veto is removed,
// the record reads what the completed check found and the write made durable.
// The caller's stop is no give-up: a stop still writes its INCOMPLETE.
//
// A veto goes, once the write is durable, only where the check earns it
// (earnsClear, backlog B653's fourth and fifth rounds): never for an
// INCOMPLETE; where the record is wholly what found says; or where no veto
// stood before this verification began (doubted false: the veto is this
// verification's own, made for its check alone) and what the record carries
// over is a VERIFIED found matched. A veto that stood before it, a write's
// that failed, stays over a VERIFIED the record carries over.
func publishVerdict(dir string, found Verification, prior *Verification, doubted bool, p publication) (_ Verification, written bool, resultErr error) {
	mpath := filepath.Join(dir, ManifestFile)
	m, err := readRecord(dir)
	if err != nil {
		return Verification{}, false, fmt.Errorf("read the snapshot's manifest: %w", err)
	}
	stored := m.Verification
	base := stored
	if prior != nil {
		base = prior
	}
	merged, database, files := mergeVerdict(base, found)
	merged.Snapshot = boundName(dir)
	clears := earnsClear(found, base, database, files, doubted)
	root := filepath.Dir(dir)
	if stored != nil && reflect.DeepEqual(*stored, merged) {
		// Nothing to write: a veto of this verification's own goes, the
		// record as every reader read it before, unless the write was given
		// up, asked again once the veto has been looked up (vetoedAt can wait
		// on the folder) and before each removal.
		if clears && !p.givenUp() && vetoedAt(dir) {
			_ = removeVetoes(dir, p.givenUp)
			stampChange(root)
		}
		return merged, false, nil
	}
	m.Verification = &merged
	// A record that holds a VERIFIED, or replaces one, is written behind its
	// veto, and with none to be had not at all.
	if holdsVerified(stored) || holdsVerified(&merged) {
		veto, err := putVeto(dir, p)
		if !veto.present {
			if veto.unconfirmed {
				return Verification{}, false, fmt.Errorf("the verdict was not written: the veto could not be confirmed: %w", err)
			}
			return Verification{}, false, fmt.Errorf("the verdict was not written: the record could not be vetoed: %w", err)
		}
		// A veto already made remains effective even if give-up skips its
		// cache stamp. Preserve that outcome beside any later write error.
		defer func() { resultErr = errors.Join(resultErr, veto.stampError()) }()
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Verification{}, false, err
	}
	if err := stageAndRename(p.given, mpath, data); err != nil {
		return Verification{}, false, fmt.Errorf("the verdict was not written: %w", err)
	}
	// The record readers read has changed, whatever follows. Recovery starts
	// after the hold's give-up by design; its fresh step may still stamp.
	// Ordinary publications retain both guards, as do all veto operations.
	defer func() {
		if p.given.Err() != nil || (!p.recovery && p.givenUp()) {
			resultErr = errors.Join(resultErr, errCacheStampSkipped)
		} else {
			stampChange(root)
		}
	}()
	if err := syncVerdictDir(mpath); err != nil {
		return merged, true, fmt.Errorf("%w: %w", ErrVerdictNotDurable, err)
	}
	if !clears {
		return merged, true, nil
	}
	switch err := removeVetoes(dir, p.givenUp); {
	case errors.Is(err, errVetoKept):
		// A write given up once its rename had landed keeps its veto.
		return merged, true, nil
	case err != nil:
		return merged, true, fmt.Errorf("%w: its veto could not be removed, so it reads INCOMPLETE: %w", ErrVerdictNotDurable, err)
	}
	return merged, true, nil
}

// boundName is the name the backups folder lists the snapshot in dir by: the
// entry of its parent that is that folder (os.SameFile), so a path spelled in
// another case, on a folder that does not tell case apart, binds the record
// to the name every reader lists; filepath.Base(dir) where none is found.
func boundName(dir string) string {
	base := filepath.Base(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return base
	}
	parent := filepath.Dir(dir)
	entries, err := readDir(parent)
	if err != nil {
		return base
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := os.Stat(filepath.Join(parent, e.Name())); err == nil && os.SameFile(fi, info) {
			return e.Name()
		}
	}
	return base
}

// vetoPlacement separates the veto's presence from its cache invalidation.
// An existing veto is just as effective as one this attempt created.
type vetoPlacement struct {
	present bool
	stamped bool
	// unconfirmed means give-up prevented trying every candidate location.
	unconfirmed bool
}

var errCacheStampSkipped = errors.New("the cache stamp was skipped after give-up; the console may retain its cached reading until its cache expires or the backups folder changes")

func (v vetoPlacement) stampError() error {
	if v.present && !v.stamped {
		return fmt.Errorf("the record is vetoed: %w", errCacheStampSkipped)
	}
	return nil
}

// afterVetoAttempt is a test seam after an attempt, before any next filesystem
// operation. Production leaves it nil.
var afterVetoAttempt func(path string, present bool)

// putVeto tries the bundle, then the root only if that attempt failed. Each
// attempt checks BOTH give-ups before even looking up the path (writeVeto).
// Presence is success even if give-up then prevents syncing or stamping it.
// These checks do not revoke an operation that already passed its guard.
func putVeto(dir string, p publication) (v vetoPlacement, err error) {
	root := filepath.Dir(dir)
	var causes []error
	for _, c := range []struct{ path, ref string }{{filepath.Join(dir, vetoFileName), dir}, {rootVetoPath(dir), root}} {
		if p.givenUp() {
			v.unconfirmed = true
			return v, errors.Join(append(causes, ErrCheckGivenUp)...)
		}
		made, attemptErr := writeVeto(c.path, c.ref)
		v.present = made || attemptErr == nil
		if afterVetoAttempt != nil {
			afterVetoAttempt(c.path, v.present)
		}
		if !v.present {
			causes = append(causes, attemptErr)
			continue
		}
		if !p.givenUp() {
			_ = fsutil.SyncParentDir(c.path)
		}
		if !p.givenUp() {
			stampChange(root)
			v.stamped = true
		}
		return v, nil
	}
	return v, errors.Join(causes...)
}

// vetoDisproved disqualifies a no-barrier recheck's completed disproof. A
// failed creation and a successful veto with a skipped cache stamp differ.
func vetoDisproved(dir string, p publication) error {
	v, err := putVeto(dir, p)
	if !v.present {
		if v.unconfirmed {
			return fmt.Errorf("the veto could not be confirmed; a later process may still trust the disproved VERIFIED verdict: %w", err)
		}
		return fmt.Errorf("the disproved record could not be vetoed; a later process may still trust its VERIFIED verdict: %w", err)
	}
	return v.stampError()
}

// lookupVetoForWrite is the existence check inside a guarded creation attempt.
var lookupVetoForWrite = os.Lstat

// writeVeto makes the veto at path, an empty file whose presence alone is
// the veto, with the owner of the folder ref (fsutil.KeepOwner), so that a
// verification run as root over a service's snapshot leaves the service a
// veto it can remove; one whose owner could not be given is kept as root's
// all the same, since its presence is all it says and its folder's owner may
// remove it. It answers whether it made one there (made, its error aside):
// one there already is kept, no error. Its caller syncs its folder as it
// needs: a veto before a write of a new VERIFIED protects every reader
// whether or not that sync answers, and one before a recheck is no veto until
// it has (setAside).
func writeVeto(path, ref string) (made bool, err error) {
	if _, err := lookupVetoForWrite(path); err == nil {
		return false, nil
	}
	if failVeto != nil {
		if err := failVeto(path); err != nil {
			return false, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = fsutil.KeepOwner(f, ref)
	return true, f.Close()
}

// removeVetoes removes the snapshot in dir's veto, in the bundle and beside
// it; one not there is no error. givenUp is asked immediately before each
// removal: once it answers true, no next removal begins (errVetoKept). The
// check and call are not atomic: a give-up between them still lets that
// removal run, and a removal already begun finishes after the give-up.
func removeVetoes(dir string, givenUp func() bool) error {
	var errs []error
	for _, p := range []string{filepath.Join(dir, vetoFileName), rootVetoPath(dir)} {
		if givenUp() {
			return errors.Join(append(errs, errVetoKept)...)
		}
		if err := removeVeto(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeVeto removes the veto at path: os.Remove, and a test's stand-in for a
// removal that does not answer (SetRemoveVetoForTest).
var removeVeto = os.Remove

// stageAndRename stages data beside path, with the owner of the file it
// replaces (a root verification over a service's snapshot), and renames it
// over path, with no barrier of its own: publishVerdict runs the one that
// decides. given is asked before every attempt at the rename, the first
// included (atomicwrite.RenameWithRetryUnsyncedCtx). A check that observes a
// give-up skips the attempt; a give-up between the check and call still lets
// it run, and a rename attempt already begun finishes after the give-up.
func stageAndRename(given context.Context, path string, data []byte) error {
	return stageAndRenameWithHook(given, path, data, nil)
}

// stageAndRenameWithHook exposes the staged pathname before the mode change
// to a test of replacement at that exact point; production passes nil.
func stageAndRenameWithHook(given context.Context, path string, data []byte, beforeMode func(string)) error {
	if failStage != nil {
		if err := failStage(filepath.Dir(path)); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-bak-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if beforeMode != nil {
		beforeMode(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if failRename != nil {
		if err := failRename(path); err != nil {
			return err
		}
	}
	if err := atomicwrite.RenameWithRetryUnsyncedCtx(given, tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// stampChange moves the backups folder's modification time to now, the
// change a reader that caches what the folder holds (the console's Jobs
// card) asks after: a snapshot made or removed moves it already, and a
// verdict or a veto, written inside a snapshot, does not. Best effort.
func stampChange(root string) {
	now := time.Now()
	_ = os.Chtimes(root, now, now)
}
