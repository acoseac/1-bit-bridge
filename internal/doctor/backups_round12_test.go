package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

func TestTheNewestBackupHintBlamesOnlyAFailedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		db, files backup.State
		want      string
	}{
		{"database failed", backup.StateFailed, backup.StateVerified, "its database failed its check"},
		{"database failure precedes incomplete files", backup.StateFailed, backup.StateIncomplete, "its database failed its check"},
		{"database incomplete", backup.StateIncomplete, backup.StateFailed, "its check did not finish"},
		{"files incomplete", backup.StateVerified, backup.StateIncomplete, "its check did not finish"},
		{"no database and incomplete files", backup.StateUnverified, backup.StateIncomplete, "its check did not finish"},
		{"verified database and failed files", backup.StateVerified, backup.StateFailed, "its database is still a recovery point"},
		{"no database and failed files", backup.StateUnverified, backup.StateFailed, "a file of the snapshot is not the one it captured; the next snapshot captures the files again"},
		{"neutral", backup.StateUnverified, backup.StateUnverified, "to take and check a new snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := backup.Verification{DatabaseState: tc.db, FilesState: tc.files}
			if tc.db == backup.StateUnverified {
				v.DatabaseReasons = []backup.Reason{backup.ReasonNoDatabase}
			}
			line := newestLine(backup.SnapshotStatus{DirName: "snapshot", CreatedAt: time.Now(), Verdict: v}, time.Now())
			if !strings.Contains(line.Hint, tc.want) {
				t.Errorf("hint %q, want %q", line.Hint, tc.want)
			}
			if tc.db != backup.StateFailed && strings.Contains(line.Hint, "database failed") {
				t.Errorf("blamed a database that did not fail: %q", line.Hint)
			}
			if tc.db != backup.StateVerified && strings.Contains(line.Hint, "still a recovery point") {
				t.Errorf("invented a database recovery point: %q", line.Hint)
			}
		})
	}
}
