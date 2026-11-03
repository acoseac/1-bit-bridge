package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// State is the on-disk record of the most recent install attempt.
// Lives at <dataDir>/update-state.json and is consulted at boot to
// decide whether the previous install succeeded, failed, or hasn't
// been verified yet.
//
// Lifecycle:
//
//  1. Install writes the marker (Status=installing, TargetVersion=X.Y.Z,
//     PreviousVersion=Y.Y.Y, AttemptedAt=now) BEFORE swapping the
//     binary and triggering restart.
//  2. On restart, the new binary's startup housekeeping reads the
//     marker and compares version.ServerVersion to TargetVersion:
//     - match → install succeeded → mark Status=installed (kept for
//     one more boot so a second botched startup can still see why
//     the .bak is there) and retain bridge.bak.
//     - mismatch → install failed (the new binary didn't actually
//     come up, or the swap landed something wrong) → restore .bak
//     and clear the marker. Service manager respawns into the
//     restored old binary.
//  3. On the boot AFTER a successful install (Status=installed,
//     installedAt > 0), startup deletes bridge.bak so disk doesn't
//     accumulate stale spares.
//
// The 6 h "recent attempt" window (recencyWindow) keeps a long-stale
// marker (e.g. an attempted install whose service-manager respawn
// never happened) from triggering rollback hours later when the user
// finally restarts. If the marker is older than the window AND no
// install/installed transition has been recorded, treat as abandoned
// and clear it.
type State struct {
	// Status is one of "" / "installing" / "installed". An empty
	// string means no marker is present; the helpers below normalise
	// a missing-file state to this.
	Status string `json:"status,omitempty"`

	// TargetVersion is the bridge version the install attempted to
	// reach. Compared against version.ServerVersion at boot to
	// decide success vs rollback.
	TargetVersion string `json:"targetVersion,omitempty"`

	// PreviousVersion is the bridge version the install was
	// upgrading FROM. Recorded for telemetry / rollback diagnostics
	// — not used for the success/failure decision.
	PreviousVersion string `json:"previousVersion,omitempty"`

	// AttemptedAt is the wall-clock time the install marker was
	// written. Used by the recency window to ignore stale markers.
	//
	// RE-STAMPED when a deferred auto-install restart finally fires
	// (restartWhenDrained): the poll cadence between the swap and the
	// restart can exceed recencyWindow — DefaultCheckInterval EQUALS it
	// at 6 h — and a marker stamped at swap time would then be read as
	// abandoned on the very boot it was armed for, skipping both the
	// success stamp and the .bak cleanup.
	AttemptedAt time.Time `json:"attemptedAt,omitempty"`

	// InstalledAt is set on the first successful boot after an
	// install, when Status transitions installing → installed. The
	// boot AFTER that uses InstalledAt to decide whether to delete
	// the now-stale .bak.
	InstalledAt time.Time `json:"installedAt,omitempty"`

	// SwapStarted records that the install reached its first
	// destructive filesystem operation — i.e. that .bak now holds the
	// binary we were running when the marker was armed, so restoring it
	// is a rollback rather than a downgrade.
	//
	// LOAD-BEARING for the boot-time restore. The marker is armed
	// BEFORE swapBinary (see Install's method doc), and on Windows
	// swapBinary's FIRST action is an SCM stop that can take up to
	// scmStopWait (15 s) before any rename happens. A kill in that
	// window — a Ctrl-C on `bridge update`, a power loss — leaves a
	// marker claiming an install that never touched the filesystem,
	// while .bak still holds the version BEFORE the one now running
	// (it is retained for exactly one boot after a successful install).
	// A blind BootInstallFailed restore there silently downgrades the
	// host by two versions. DecideBootAction refuses the restore when
	// this is false; see BootClearNotSwapped.
	//
	// The residual window between setting this and the rename actually
	// landing is microseconds and structurally irreducible without a
	// transactional filesystem.
	SwapStarted bool `json:"swapStarted,omitempty"`

	// RejectedVersion is a release the operator deliberately rolled
	// back. Written by Rollback INSTEAD of clearing the marker, and
	// consulted by the auto-installer so the next poll doesn't
	// immediately re-install the release that was just rejected.
	//
	// Without it a manual rollback could not be made to stick: the
	// rollback only takes effect on restart, and on that restart the
	// poller compares the now-running vN against the still-latest vN+1,
	// sets UpdateAvailable, and every auto-install gate passes again.
	//
	// A STRICTLY NEWER release installs normally — the operator
	// rejected one build, not all future ones — and writing a fresh
	// install marker drops the field (SaveState persists the whole
	// struct), so an install of that newer release clears the
	// rejection by construction. Carried on a marker whose Status is
	// "" so DecideBootAction treats it as BootNoop.
	RejectedVersion string `json:"rejectedVersion,omitempty"`

	// RejectedReason says, in words that name no path and no value, why
	// RejectedVersion is held back when the hold is not an operator's
	// rollback: the version refused this bridge's settings, at the check
	// before its install or at its first start, or it predates that check.
	// Empty for a rollback, whose wording the gate keeps (HeldBack).
	RejectedReason string `json:"rejectedReason,omitempty"`

	// RejectedConfigDigest is the ConfigDigest of the settings
	// RejectedVersion refused. The automatic install holds the version
	// back only while the settings still have this digest, so fixing them
	// lets it try again. Empty holds it back whatever the settings are (a
	// rollback, a candidate that predates the check).
	RejectedConfigDigest string `json:"rejectedConfigDigest,omitempty"`

	// DataDirOpenedBy is the version, other than PreviousVersion, that got
	// past its config at boot and went on to write the data dir while this
	// marker was armed. serve records it durably before its first write
	// there (disarmEarlyRollback in cmd/bridge), so from then on a restore
	// of .bak may be a downgrade past what that version's store migrations
	// wrote, and neither the early rollback (EarlyRollback) nor the boot
	// rollback (BootInstallFailed) restores it.
	DataDirOpenedBy string `json:"dataDirOpenedBy,omitempty"`

	// RollbackStarted says the early rollback (cmd/bridge) wrote this marker
	// just before it put the backup back: RejectedVersion's hold, beside the
	// install it was undoing (TargetVersion over PreviousVersion, at
	// AttemptedAt, SwapStarted). Status is "", so the binary the backup
	// restores, whatever its version, takes the marker for no install and
	// keeps the hold, where 0.2.1's and 0.2.2's boot clears an install
	// marker it cannot finish and the hold with it; and a start of
	// TargetVersion that finds it, stopped before the backup was back,
	// finishes the rollback (EarlyRollback).
	RollbackStarted bool `json:"rollbackStarted,omitempty"`

	// MigrationVeto names the version whose store migrations are refused
	// here (backlog B613's round 4): a store open of that version with a step
	// pending refuses (DisarmBeforeMigrating, MigrationVetoed). A rollback
	// that puts the backup back over that version's binary writes it before
	// its rename (the early rollback's RollbackRecord; the boot's
	// BootInstallFailed), since a command of that version may still be
	// running from its image, its store open waiting, and must not migrate
	// the store past the version put back. It outlives the rollback: every
	// clear keeps it (ClearInstall), as a refusal recorded since does
	// (recordRejection). Only that version's own serve, at its admission
	// once its settings loaded (AdmitBoot), lifts it, and a new install's
	// marker, a fresh one, carries none.
	MigrationVeto string `json:"migrationVeto,omitempty"`
}

// stateFileName is the basename under <dataDir>. Constant so tests
// can hardcode it; callers always go through StatePath.
const stateFileName = "update-state.json"

// recencyWindow is how long after AttemptedAt we still consider a
// marker actionable. A marker older than this is treated as
// abandoned and cleared at boot (no rollback). Generous because the
// failure mode we're guarding against is "install ran, service
// failed to restart, operator notices and restarts manually" — that
// could be hours.
const recencyWindow = 6 * time.Hour

// RecencyWindow is the exported view of `recencyWindow` for callers
// that need to surface the value in operator-facing copy (e.g. the
// CLI's post-install hint about when boot-time rollback would
// still fire). Returning a Duration keeps the formatting decision
// at the caller.
func RecencyWindow() time.Duration { return recencyWindow }

// StatePath returns the on-disk location of update-state.json
// inside the bridge's dataDir.
func StatePath(dataDir string) string {
	return filepath.Join(dataDir, stateFileName)
}

// LoadState reads the marker from dataDir. Returns a zero-value
// State + nil error when the file is missing — that's the normal
// no-install-pending state.
func LoadState(dataDir string) (State, error) {
	raw, err := os.ReadFile(StatePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read update-state: %w", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		// A malformed marker is treated as "no marker" — we'd rather
		// recover by ignoring it than block boot on a bad file. The
		// log line is what tells the operator something's off.
		logger.Warn("malformed update-state.json (treating as absent)", "err", err)
		return State{}, nil
	}
	return st, nil
}

// SaveState atomically writes State to dataDir/update-state.json
// (temp + rename). Caller is expected to validate the State before
// calling.
func SaveState(dataDir string, st State) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir dataDir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dataDir, ".update-state-*.json")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close (LIFO order — runs before Remove). See
	// internal/auth/auth.go for the rationale; pattern repeats here.
	defer func() { _ = tmp.Close() }()
	// A `sudo bridge update` beside a service install stages this marker
	// as root, and the service must read it at the boot that acts on it.
	if err := fsutil.KeepOwner(tmp, StatePath(dataDir)); err != nil {
		tmp.Close()
		return fmt.Errorf("keep the update marker's owner: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	// The marker's own publication: the rename and its retries
	// (RenameWithRetry's schedule, for Windows' scan-on-close), with no
	// barrier but the marker's own below. RenameWithRetry's tolerant barrier
	// would run first, log the data dir and the error, and be a second
	// barrier beside it (backlog B613's round 3).
	if err := atomicwrite.RenameWithRetryUnsynced(tmpName, StatePath(dataDir)); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = ""
	// fsync the parent directory so the rename's new dentry survives a
	// sudden power loss. os.Rename is atomic at the directory-entry
	// level, but POSIX doesn't guarantee the entry itself is durable
	// until the parent dir is fsynced — a crash in that window could
	// leave the marker missing and the boot-time rollback logic would
	// skip. Mirrors swapBinary in swap_unix.go, which is likewise
	// Unix-only. A no-op on Windows (markerBarrier), where
	// FlushFileBuffers on a directory handle always fails
	// (ERROR_INVALID_HANDLE) — NTFS journaling + the temp fsync above
	// already cover durability there (Gemini review).
	//
	// A barrier that fails is the write's error, as a failed fsync of the
	// file above is (backlog B613's round 2), but for a filesystem that
	// cannot fsync a directory at all (syncMarkerDir). The rename has landed
	// by then: the new marker is live, and may not outlive a power loss. A
	// caller told the write failed never trusts it later: the disarm runs
	// this barrier again before it short-cuts on what a marker says
	// (disarmHeld), refusing while it fails (round 3).
	if err := syncMarkerDir(dataDir); err != nil {
		return fmt.Errorf("sync the data dir after writing the update marker: %w", err)
	}
	return nil
}

// ClearState removes the marker. No-op when the file doesn't exist.
func ClearState(dataDir string) error {
	err := os.Remove(StatePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearStateDurably is ClearState, and off Windows an fsync of dataDir, so
// the removal outlives a power loss as SaveState's rename does: a marker that
// cannot be read is removed before the data dir is first written (AdmitBoot,
// DisarmBeforeMigrating), and it must not come back for a later start to act
// on. A barrier that fails is its error, as SaveState's is, and the barrier
// runs whether or not there was a marker to remove, so a second clear after a
// first whose barrier failed is not a no-op.
func ClearStateDurably(dataDir string) error {
	if err := ClearState(dataDir); err != nil {
		return err
	}
	if err := syncMarkerDir(dataDir); err != nil {
		return fmt.Errorf("sync the data dir after removing the update marker: %w", err)
	}
	return nil
}

// ClearInstall removes the install marker st, which is the one in dataDir,
// keeping a hold recorded beside it (RejectedVersion, its reason and its
// digest): the boot is done with the install, and the hold is about another
// version and the settings, so dropping it would cost one more download and
// check of a version that refuses them (backlog B613's round 2). It keeps
// the veto of a version's store migrations too (MigrationVeto), which is
// about that version's commands, not the install: the housekeeping of the
// version an early rollback put back clears the rollback's record here, and
// a command of the version rolled back must still find its migrations
// refused (round 4). A marker with neither is removed, as ClearState
// removes it.
func ClearInstall(dataDir string, st State) error {
	kept := State{RejectedVersion: st.RejectedVersion, RejectedReason: st.RejectedReason,
		RejectedConfigDigest: st.RejectedConfigDigest, MigrationVeto: st.MigrationVeto}
	if kept == (State{}) {
		return ClearState(dataDir)
	}
	return SaveState(dataDir, kept)
}

// BootAction is the decision the startup housekeeping should take
// based on the current State + ServerVersion + binary file presence.
type BootAction int

const (
	// BootNoop means no marker is present (or the marker is stale
	// and abandoned) — nothing for boot to do.
	BootNoop BootAction = iota

	// BootInstallSucceeded means the marker says we just installed
	// X.Y.Z and ServerVersion matches. Caller should mark
	// installed=now and retain .bak for one more boot.
	BootInstallSucceeded

	// BootInstallFailed means the marker says we just installed
	// X.Y.Z but ServerVersion doesn't match. Caller should restore
	// .bak.
	BootInstallFailed

	// BootCleanupBak means a previous boot already marked installed
	// successfully; this boot is the "one more" that retains .bak.
	// Caller should delete .bak (we've now had a known-good run on
	// the new binary) and clear the marker.
	BootCleanupBak

	// BootClearAbandoned means the marker is older than the recency
	// window and never reached "installed" — treat as abandoned,
	// clear it and leave .bak alone (operator can roll back manually).
	BootClearAbandoned

	// BootClearNotSwapped means the marker was armed but the install
	// never reached its first destructive filesystem operation
	// (State.SwapStarted is false), so .bak does NOT hold the binary
	// we are running — restoring it would be a downgrade, not a
	// rollback. Caller clears the marker and leaves .bak alone.
	//
	// The reachable trigger is a kill during the Windows swap's SCM
	// stop (up to 15 s of marker-armed, nothing-mutated); see
	// State.SwapStarted.
	BootClearNotSwapped

	// BootRollbackOvertaken means the marker is the early rollback's own
	// record (State.RollbackStarted, Status ""), left by a start of the
	// install's version that was stopped before the backup was back, and
	// this boot is of that version: the housekeeping runs only once serve
	// got past its settings, so the rollback will not be finished. Caller
	// confirms the install as for BootInstallSucceeded, whatever the record's
	// age, on the record its admission made of this version opening the data
	// dir and on no less (ConfirmOverRollback: installed now, RollbackStarted
	// and a hold and a veto of this version cleared), and retains .bak for
	// one more boot.
	BootRollbackOvertaken

	// BootRollbackFinished means the marker is that record and the version
	// the backup held runs: the rollback was finished. Caller clears the
	// install the record describes and keeps its hold and its veto
	// (ClearInstall); nothing is restored.
	BootRollbackFinished
)

// DecideBootAction returns what the startup path should do given
// the current state and observed ServerVersion. Pure function — no
// side effects, easy to unit-test.
func DecideBootAction(st State, currentServerVersion string, now time.Time) BootAction {
	switch st.Status {
	case "":
		// The early rollback's own record is its own case, never an install
		// marker and never a hold alone (backlog B613's round 3). Read before
		// any window: the housekeeping runs only once serve got past its
		// settings, so the install's version booting is the install working,
		// however old the record, and an expired one is confirmed, not
		// cleared as an abandoned install marker would be; the backup's
		// version booting is the rollback done. Any other version, and a hold
		// with no rollback, is nothing to do.
		if !st.RollbackStarted {
			return BootNoop
		}
		switch {
		case SameVersion(currentServerVersion, st.TargetVersion):
			return BootRollbackOvertaken
		case SameVersion(currentServerVersion, st.PreviousVersion):
			return BootRollbackFinished
		}
		return BootNoop
	case "installing":
		if now.Sub(st.AttemptedAt) > recencyWindow {
			return BootClearAbandoned
		}
		// Version match wins BEFORE the SwapStarted check on purpose:
		// we are demonstrably running TargetVersion, so the swap
		// obviously happened, and a marker written by a pre-SwapStarted
		// binary (flag absent → false after unmarshal) must still be
		// stamped installed across the upgrade boundary.
		if normalizeTag(currentServerVersion) == normalizeTag(st.TargetVersion) {
			return BootInstallSucceeded
		}
		if !st.SwapStarted {
			// Marker armed, filesystem untouched: .bak belongs to an
			// EARLIER cycle, so restoring it would downgrade rather than
			// roll back. Clear and leave the binaries alone.
			return BootClearNotSwapped
		}
		return BootInstallFailed
	case "installed":
		// One-more-boot retention. installedAt is set on the first
		// post-install boot; this is the second.
		if !st.InstalledAt.IsZero() {
			return BootCleanupBak
		}
		// Defensive: malformed Status=installed without InstalledAt
		// — treat as noop so we don't accidentally trash the .bak.
		return BootNoop
	default:
		logger.Warn("unknown state.Status (treating as noop)", "status", st.Status)
		return BootNoop
	}
}

// ConfirmOverRollback is the marker BootRollbackOvertaken writes over st, the
// early rollback's record of an install of current that a start stopped
// before the backup was back: the install confirmed as BootInstallSucceeded
// confirms one (installed at now, so the boot after it removes the backup),
// the rollback's mark gone, and its hold and its veto gone only where they
// name current, a refusal of another version recorded since staying. The
// install and the record of the start that went on past it stay as they are.
//
// It confirms only on that record: st must name the install's version as the
// one that opened the data dir (State.DataDirOpenedBy), which serve's
// admission writes before the store opens (AdmitBoot), and current must be
// that version (backlog B613's round 4). Otherwise it returns an error and st
// as it was, for the caller to leave the marker as it is: a start that cannot
// show its own admission has not shown that the rollback is over.
func ConfirmOverRollback(st State, current string, now time.Time) (State, error) {
	if st.DataDirOpenedBy == "" || !SameVersion(current, st.TargetVersion) || !SameVersion(st.DataDirOpenedBy, st.TargetVersion) {
		return st, fmt.Errorf("the record does not name %s as the version that opened the data dir, as its admission records before the store opens",
			current)
	}
	st.Status, st.InstalledAt, st.RollbackStarted = "installed", now, false
	if SameVersion(st.RejectedVersion, current) {
		st.RejectedVersion, st.RejectedReason, st.RejectedConfigDigest = "", "", ""
	}
	if SameVersion(st.MigrationVeto, current) {
		st.MigrationVeto = ""
	}
	return st, nil
}

// CurrentServerVersion exposes the bridge's own version constant
// to test code without leaking the version package import. Production
// callers can read version.ServerVersion directly.
func CurrentServerVersion() string {
	return version.ServerVersion
}
