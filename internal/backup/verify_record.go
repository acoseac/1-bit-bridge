package backup

import (
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
// itself through atomicwrite (RenameWithRetryUnsynced), then its folder
// synced (syncVerdictDir). That last sync can fail once the rename has
// landed, and then every reader already reads the new record while the write
// answers an error; and a write that fails before its rename leaves the
// record it would have replaced. So a record written while either the record
// it replaces or the new one holds a VERIFIED state goes behind a veto
// (backlog B653, and its second round for a VERIFIED left behind): before
// the staging the write puts vetoFileName in the bundle, its folder synced,
// and only once the folder's sync after the rename has answered does it
// remove it; while the veto stands, every reader (readVerdict) reads a
// VERIFIED as INCOMPLETE (ReasonUnsynced). Each change of the veto moves the
// backups folder's modification time (stampChange), so a reader that caches
// what the folder holds (the console's Jobs card) reads it again.
//
// What a write that answered an error leaves, as built:
//   - no reader reads a VERIFIED, whichever step failed, the veto's own
//     removal included, while the veto could be made in the bundle or, where
//     it could not (a bundle this process may not write in), beside it in
//     the backups folder (rootVetoPath), which every reader asks too;
//   - where neither could be made, a new VERIFIED is not written at all
//     (the error says so), and a record that held a VERIFIED is still
//     written over, the error saying, if that write failed too, that the
//     record keeps the verdict it held: the one case a reader still reads
//     the VERIFIED a later check did not find;
//   - a veto made whose owner could not be given (root_squash) is kept as it
//     is: its presence alone is the veto, and the service may remove it.
//
// A crash meanwhile leaves the previous record behind its veto, or the new
// one, with or without it; each reads VERIFIED only from a check that
// passed. Where no directory can be synced (Windows, fsutil.SyncParentDir a
// no-op), the write is atomic and is not known to be durable: a crash can
// leave the previous record, PENDING among them, which reads INCOMPLETE once
// its owner is gone, and never a VERIFIED from a check that did not pass.

// syncVerdictDir is the barrier a verdict's write runs once its rename has
// landed, given the manifest's path: fsutil.SyncParentDir. A test seam
// (SetSyncVerdictForTest).
var syncVerdictDir = fsutil.SyncParentDir

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
// latest. The record's PENDING time goes once nothing in it is PENDING.
func mergeVerdict(stored *Verification, found Verification) Verification {
	if stored == nil {
		out := found
		out.Format = verificationFormat
		return out
	}
	out := *stored
	out.Format = verificationFormat
	completed := func(s State) bool { return s == StateVerified || s == StateFailed }
	replaces := func(old, s State) bool {
		if s == StateIncomplete {
			return old == StatePending || old == StateIncomplete || !known(old)
		}
		return s != StatePending
	}
	if replaces(stored.DatabaseState, found.DatabaseState) &&
		!(found.DatabaseState == StateVerified && completed(stored.DatabaseState) && stored.Level.rank() > found.Level.rank()) {
		out.DatabaseState, out.DatabaseReasons = found.DatabaseState, found.DatabaseReasons
		out.Level, out.CheckedAt, out.DurationMS = found.Level, found.CheckedAt, found.DurationMS
		out.UserVersion, out.Counts = found.UserVersion, found.Counts
	}
	if replaces(stored.FilesState, found.FilesState) {
		out.FilesState, out.FilesReasons, out.FilesFailed = found.FilesState, found.FilesReasons, found.FilesFailed
		out.FilesCheckedAt = found.FilesCheckedAt
	}
	if out.DatabaseState != StatePending && out.FilesState != StatePending {
		out.PendingSince = time.Time{}
	}
	return out
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

// ErrRecordKept marks a verdict's write that failed where no veto could be
// made beside the snapshot: its record keeps the VERIFIED it held, which a
// reader still reads, though the check just made did not find it.
var ErrRecordKept = errors.New("the snapshot's record keeps the verified verdict it held, which this check did not find")

// publishVerdict writes found into the record of the snapshot at dir, merged
// with the one its manifest holds now, and answers the record as written.
// Its caller holds the backups folder's lock. A merge that changes nothing
// writes nothing.
func publishVerdict(dir string, found Verification) (Verification, error) {
	mpath := filepath.Join(dir, ManifestFile)
	m, err := readRecord(dir)
	if err != nil {
		return Verification{}, fmt.Errorf("read the snapshot's manifest: %w", err)
	}
	merged := mergeVerdict(m.Verification, found)
	if m.Verification != nil && reflect.DeepEqual(*m.Verification, merged) && !vetoedAt(dir) {
		return merged, nil
	}
	stored := m.Verification
	m.Verification = &merged
	root := filepath.Dir(dir)
	// unvetoed is why no veto stands over a record that held a VERIFIED.
	var unvetoed error
	if holdsVerified(stored) || holdsVerified(&merged) {
		made, err := putVeto(dir)
		switch {
		case err != nil && holdsVerified(&merged):
			return Verification{}, fmt.Errorf("the verdict was not written: put its veto in place: %w", err)
		case err != nil:
			unvetoed = err
		case made:
			// A reader's VERIFIED reads INCOMPLETE from now on.
			stampChange(root)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Verification{}, err
	}
	if err := stageAndRename(mpath, data); err != nil {
		if unvetoed != nil {
			return Verification{}, fmt.Errorf("%w: the verdict was not written (%w), and no veto could be put in place (%w)", ErrRecordKept, err, unvetoed)
		}
		return Verification{}, fmt.Errorf("the verdict was not written: %w", err)
	}
	// The record readers read has changed, whatever follows.
	defer stampChange(root)
	if err := syncVerdictDir(mpath); err != nil {
		return merged, fmt.Errorf("%w: %w", ErrVerdictNotDurable, err)
	}
	if err := removeVetoes(dir); err != nil {
		return merged, fmt.Errorf("%w: its veto could not be removed, so it reads INCOMPLETE: %w", ErrVerdictNotDurable, err)
	}
	return merged, nil
}

// putVeto puts the veto of the snapshot in dir in place, and answers
// whether it made one: one there already, in the bundle or beside it, of
// any owner, is kept as it is (made false); otherwise it is made in the
// bundle, and where that cannot be (a bundle this process may not write in),
// beside it in the backups folder (rootVetoPath), each with its folder
// synced where it can be, so that a crash keeps it; neither made is the
// error.
func putVeto(dir string) (made bool, err error) {
	inBundle, beside := filepath.Join(dir, vetoFileName), rootVetoPath(dir)
	for _, p := range []string{inBundle, beside} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
	}
	bundleErr := writeVeto(inBundle, dir)
	if bundleErr == nil {
		return true, nil
	}
	besideErr := writeVeto(beside, filepath.Dir(dir))
	if besideErr == nil {
		return true, nil
	}
	return false, fmt.Errorf("in the snapshot: %w; beside it: %w", bundleErr, besideErr)
}

// writeVeto makes the veto at path, an empty file whose presence alone is
// the veto, with the owner of the folder ref (fsutil.KeepOwner), so that a
// verification run as root over a service's snapshot leaves the service a
// veto it can remove; one whose owner could not be given is kept as root's
// all the same, since its presence is all it says and its folder's owner
// may remove it. Its folder is synced, best effort: a veto there protects
// every reader whether or not that sync answers, and a folder that will not
// sync fails the verdict's own sync after its rename, which leaves the veto
// standing. One there already is no error.
func writeVeto(path, ref string) error {
	if failVeto != nil {
		if err := failVeto(path); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = fsutil.KeepOwner(f, ref)
	if err := f.Close(); err != nil {
		return err
	}
	_ = fsutil.SyncParentDir(path)
	return nil
}

// removeVetoes removes the snapshot in dir's veto, in the bundle and beside
// it; one not there is no error.
func removeVetoes(dir string) error {
	var errs []error
	for _, p := range []string{filepath.Join(dir, vetoFileName), rootVetoPath(dir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stageAndRename stages data beside path, with the owner of the file it
// replaces (a root verification over a service's snapshot), and renames it
// over path, with no barrier of its own: publishVerdict runs the one that
// decides.
func stageAndRename(path string, data []byte) error {
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
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if failRename != nil {
		if err := failRename(path); err != nil {
			return err
		}
	}
	if err := atomicwrite.RenameWithRetryUnsynced(tmpName, path); err != nil {
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
