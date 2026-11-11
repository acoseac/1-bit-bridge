package backup

import (
	"context"
	"slices"
	"time"
)

// SnapshotStatus is one snapshot as a reader reads it, from its manifest
// alone: when it was written, by which bridge, what it holds, and the two
// verdicts of its record (readVerdict). It names the snapshot by its folder's
// name, a time stamp, and no path.
type SnapshotStatus struct {
	DirName       string
	CreatedAt     time.Time
	BridgeVersion string
	Files         []string
	Verdict       Verification
	// Legacy says it was written before verification: no baselines, no
	// record, both verdicts UNVERIFIED.
	Legacy bool
	// Owned says a live owner holds its PENDING, whose verification is still
	// to come or under way, in this process or another (owner.go).
	Owned bool
}

// Status is the backups folder as the doctor and the console read it
// (backlog B653): every snapshot, newest first, read from the manifests
// alone, never a verification and never the live database.
type Status struct {
	Snapshots []SnapshotStatus
}

// Newest is the newest snapshot written, whatever its verdicts.
func (s Status) Newest() (SnapshotStatus, bool) {
	if len(s.Snapshots) == 0 {
		return SnapshotStatus{}, false
	}
	return s.Snapshots[0], true
}

// NewestVerified is the newest snapshot whose database's verdict is
// VERIFIED: the newest recovery point, whatever its other files' verdict.
func (s Status) NewestVerified() (SnapshotStatus, bool) {
	for _, sn := range s.Snapshots {
		if sn.Verdict.DatabaseState == StateVerified {
			return sn, true
		}
	}
	return SnapshotStatus{}, false
}

// ReadStatus is the Status of the backups folder at backupsRoot: List's
// reading of each manifest, and nothing else. A folder not made yet holds no
// snapshot.
func ReadStatus(ctx context.Context, backupsRoot string) (Status, error) {
	entries, err := ListContext(ctx, backupsRoot)
	if err != nil {
		return Status{}, err
	}
	out := Status{Snapshots: make([]SnapshotStatus, 0, len(entries))}
	for _, e := range entries {
		out.Snapshots = append(out.Snapshots, SnapshotStatus{
			DirName:       e.DirName,
			CreatedAt:     e.CreatedAt,
			BridgeVersion: e.BridgeVersion,
			Files:         slices.Clone(e.Files),
			Verdict:       e.Verdict,
			Legacy:        e.legacy(),
			Owned:         e.Owner == OwnerLive,
		})
	}
	return out, nil
}
