package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestTheBackupCheckLineNamesWhatHoldsTheFolderAndWhatEndsIt (backlog B826's
// second round, Codex on its brief): the backup-check line names a restore
// given up that holds the backups folder as a restore of its snapshot, and
// says the bridge restore command that runs it ends it, never a restart of
// the bridge, which runs none; a verification's and a prune's, a marker
// written before restores were marked among them, keep their words.
func TestTheBackupCheckLineNamesWhatHoldsTheFolderAndWhatEndsIt(t *testing.T) {
	now := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		stuck   backup.StuckCheck
		summary string
		hint    string
		notInIt string
	}{
		{"a restore", backup.StuckCheck{Snapshot: "S", Step: "its read of the snapshot it restores", Since: now.Add(-2 * time.Minute), Restore: true},
			"a restore of snapshot S did not end (its read of the snapshot it restores)", "the end of the bridge restore command that runs it ends it", "restart the bridge"},
		{"a verification", backup.StuckCheck{Snapshot: "S", Step: "its check", Since: now.Add(-2 * time.Minute)},
			"a verification of snapshot S did not end (its check)", "restart the bridge, or stop the backup --verify command that runs it", "bridge restore"},
		{"a prune", backup.StuckCheck{Step: "its removals", Since: now.Add(-2 * time.Minute)},
			"a prune of the snapshots did not end (its removals)", "restart the bridge, or stop the backup --verify command that runs it", "bridge restore"},
		// A probe of the lock whose release did not end (backlog B871's item 3)
		// is said as the probe it is, never a prune or a restore.
		{"a probe", backup.StuckCheck{Probe: "the doctor's probe of the snapshots' lock", Step: "letting go of the lock it tried", Since: now.Add(-2 * time.Minute)},
			"the doctor's probe of the snapshots' lock did not end (letting go of the lock it tried)", "the storage that holds the snapshots", "restore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stuck := tc.stuck
			var line *Check
			for _, c := range BackupChecks(backup.Status{}, nil, nil, &stuck, true, now) {
				if c.Name == checkNameBackupCheck {
					line = &c
					break
				}
			}
			if line == nil {
				t.Fatal("no backup-check line beside a hold given up")
			}
			if !strings.Contains(line.Summary, tc.summary) || !strings.Contains(line.Hint, tc.hint) || strings.Contains(line.Hint, tc.notInIt) {
				t.Errorf("backup-check = %q, hint %q; want %q and %q, and no %q", line.Summary, line.Hint, tc.summary, tc.hint, tc.notInIt)
			}
		})
	}
}
