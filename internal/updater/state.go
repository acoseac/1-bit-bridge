package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
// The 5 min "recent attempt" window keeps a long-stale marker (e.g.
// an attempted install whose service-manager respawn never happened)
// from triggering rollback hours later when the user finally
// restarts. If the marker is older than the window AND no
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
	AttemptedAt time.Time `json:"attemptedAt,omitempty"`

	// InstalledAt is set on the first successful boot after an
	// install, when Status transitions installing → installed. The
	// boot AFTER that uses InstalledAt to decide whether to delete
	// the now-stale .bak.
	InstalledAt time.Time `json:"installedAt,omitempty"`
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
	if err := os.Rename(tmpName, StatePath(dataDir)); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = ""
	// fsync the parent directory so the rename's new dentry survives a
	// sudden power loss. os.Rename is atomic at the directory-entry
	// level, but POSIX doesn't guarantee the entry itself is durable
	// until the parent dir is fsynced — a crash in that window could
	// leave the marker missing and the boot-time rollback logic would
	// skip. Mirrors swapBinary in swap_unix.go. Best-effort: on Windows
	// FlushFileBuffers on a directory handle fails and is ignored (NTFS
	// journaling + the temp fsync above already cover durability there).
	if d, err := os.Open(dataDir); err == nil {
		_ = d.Sync()
		_ = d.Close()
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
)

// DecideBootAction returns what the startup path should do given
// the current state and observed ServerVersion. Pure function — no
// side effects, easy to unit-test.
func DecideBootAction(st State, currentServerVersion string, now time.Time) BootAction {
	switch st.Status {
	case "":
		return BootNoop
	case "installing":
		if now.Sub(st.AttemptedAt) > recencyWindow {
			return BootClearAbandoned
		}
		if normalizeTag(currentServerVersion) == normalizeTag(st.TargetVersion) {
			return BootInstallSucceeded
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

// CurrentServerVersion exposes the bridge's own version constant
// to test code without leaking the version package import. Production
// callers can read version.ServerVersion directly.
func CurrentServerVersion() string {
	return version.ServerVersion
}
