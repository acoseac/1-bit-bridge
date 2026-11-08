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
// answers an error. So a record holding a VERIFIED state is written behind a
// veto: before the rename the write puts vetoFileName in the bundle, and
// only once the folder's sync has answered does it remove it; while the
// veto stands, every reader (readVerdict) reads that state as INCOMPLETE
// (ReasonUnsynced). A write that returned an error therefore never leaves a
// VERIFIED that any reader, the doctor, the console or the pruner, reads,
// whichever step failed, the veto's own removal included. A crash meanwhile
// leaves the previous record, or the new one, with or without the veto;
// each reads VERIFIED only from a check that passed. Where no directory can
// be synced (Windows, fsutil.SyncParentDir a no-op), the write is atomic
// and is not known to be durable: a crash can leave the previous record,
// PENDING among them, which reads INCOMPLETE once PendingBound has passed,
// and never a VERIFIED from a check that did not pass.

// syncVerdictDir is the barrier a verdict's write runs once its rename has
// landed, given the manifest's path: fsutil.SyncParentDir. A test seam
// (SetSyncVerdictForTest).
var syncVerdictDir = fsutil.SyncParentDir

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

// ErrVerdictNotDurable marks a verdict's write whose folder sync failed once
// its rename had landed: the record holds the verdict, behind its veto
// while it holds a VERIFIED, and whether it survives a crash is not known.
var ErrVerdictNotDurable = errors.New("the verdict was written and is not known to be durable")

// publishVerdict writes found into the record of the snapshot at dir, merged
// with the one its manifest holds now, and answers the record as written.
// Its caller holds the backups folder's lock. A merge that changes nothing
// writes nothing.
func publishVerdict(dir string, found Verification) (Verification, error) {
	mpath := filepath.Join(dir, ManifestFile)
	m, err := readManifest(mpath)
	if err != nil {
		return Verification{}, fmt.Errorf("read the snapshot's manifest: %w", err)
	}
	merged := mergeVerdict(m.Verification, found)
	if m.Verification != nil && reflect.DeepEqual(*m.Verification, merged) && !vetoedAt(dir) {
		return merged, nil
	}
	m.Verification = &merged
	veto := filepath.Join(dir, vetoFileName)
	if merged.DatabaseState == StateVerified || merged.FilesState == StateVerified {
		if err := writeVeto(veto); err != nil {
			return Verification{}, fmt.Errorf("the verdict was not written: put its veto in place: %w", err)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Verification{}, err
	}
	if err := stageAndRename(mpath, data); err != nil {
		return Verification{}, fmt.Errorf("the verdict was not written: %w", err)
	}
	// The record readers read has changed, whatever follows.
	defer stampChange(filepath.Dir(dir))
	if err := syncVerdictDir(mpath); err != nil {
		return merged, fmt.Errorf("%w: %w", ErrVerdictNotDurable, err)
	}
	if err := os.Remove(veto); err != nil && !errors.Is(err, os.ErrNotExist) {
		return merged, fmt.Errorf("%w: its veto could not be removed, so it reads INCOMPLETE: %w", ErrVerdictNotDurable, err)
	}
	return merged, nil
}

// writeVeto puts the veto at path: an empty file, made or truncated.
func writeVeto(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// stageAndRename stages data beside path, with the owner of the file it
// replaces (a root verification over a service's snapshot), and renames it
// over path, with no barrier of its own: publishVerdict runs the one that
// decides.
func stageAndRename(path string, data []byte) error {
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
	if err := atomicwrite.RenameWithRetryUnsynced(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// stampChange moves the backups folder's modification time to now, the
// change a reader that caches what the folder holds (the console's Jobs
// card) asks after: a snapshot made or removed moves it already, and a
// verdict, written inside a snapshot, does not. Best effort.
func stampChange(root string) {
	now := time.Now()
	_ = os.Chtimes(root, now, now)
}
