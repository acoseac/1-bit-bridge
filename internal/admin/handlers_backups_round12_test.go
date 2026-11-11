package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

func TestTheConsoleNamesVerificationAdmissionRefusals(t *testing.T) {
	for _, refusal := range []error{backup.ErrBackupsUnlocked, backup.ErrBackupsBusy, backup.ErrCheckStillRunning} {
		t.Run(refusal.Error(), func(t *testing.T) {
			logs := loggingtest.Record(t)
			reportBackupVerification("snapshot", backup.Result{VerifyErr: refusal})
			lines := logs.Lines("admin: the verification did not start")
			if len(lines) != 1 || !strings.Contains(lines[0], refusal.Error()) {
				t.Errorf("missing admission and its reason: %v", logs.All())
			}
			// Lines matches a record's whole message: name the console's exact words.
			if len(logs.Lines("admin: a snapshot's verdict was not recorded")) != 0 {
				t.Errorf("claimed a finding from a refused check: %v", logs.All())
			}
		})
	}
	t.Run("busy cancellation wrote incomplete", func(t *testing.T) {
		logs := loggingtest.Record(t)
		reportBackupVerification("snapshot", backup.Result{VerifyErr: errors.Join(backup.ErrBackupsBusy, context.Canceled),
			Verify: backup.VerifyResult{Write: backup.WriteRecorded, Recorded: true}})
		if len(logs.Lines("admin: the verification did not start")) != 0 ||
			len(logs.Lines("admin: a snapshot's verification answered an error once its write had ended")) != 1 {
			t.Errorf("lost the cancellation's write: %v", logs.All())
		}
	})
	t.Run("failed veto", func(t *testing.T) {
		logs := loggingtest.Record(t)
		reportBackupVerification("snapshot", backup.Result{VerifyErr: errors.Join(backup.ErrRecheckNotRecorded,
			errors.New("the disproved record could not be vetoed"))})
		if lines := logs.Lines("admin: a snapshot's verdict was not recorded"); len(lines) != 1 ||
			!strings.Contains(lines[0], "disproved record could not be vetoed") {
			t.Errorf("hid the failed veto: %v", logs.All())
		}
	})
}

func TestTheConsoleDropsADisprovedNoBarrierRecordFromItsCache(t *testing.T) {
	for _, damaged := range []string{backup.ManifestDBFileName, "server.crt"} {
		t.Run(damaged, func(t *testing.T) {
			s, _, _ := newTestServer(t)
			s.deps.BackupSources = liveStore(t, t.TempDir())
			root := filepath.Join(s.deps.BackupSources.DataDir, backup.BackupsDirName)
			t.Cleanup(func() { backuptest.AwaitIdle(t) })
			res := backup.SnapshotAndVerify(t.Context(), s.deps.BackupSources, backup.SnapshotOptions{})
			backuptest.AwaitIdle(t)
			if res.VerifyErr != nil || !res.Verify.Record.Passes() {
				t.Fatalf("initial snapshot: %+v", res)
			}
			// A planted old stamp makes a missing invalidation deterministic,
			// including on filesystems with coarse modification times.
			past := time.Now().Add(-time.Hour)
			if err := os.Chtimes(root, past, past); err != nil {
				t.Fatal(err)
			}
			if card := s.getBackupCard(t.Context()); card.lastVerifiedAt == nil || !strings.Contains(card.newestDatabase, "VERIFIED") {
				t.Fatalf("cache was not primed with VERIFIED: %+v", card)
			}
			path := filepath.Join(res.Dir, damaged)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-1] ^= 1
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(backup.SetSyncsParentDirForTest(false))
			t.Cleanup(func() { backuptest.AwaitIdle(t) })
			checked, err := backup.VerifySnapshot(t.Context(), res.Dir, backup.VerifyOptions{BackupsRoot: root})
			backuptest.AwaitIdle(t)
			if !errors.Is(err, backup.ErrRecheckNotRecorded) || checked.Record.DatabaseState != backup.StateIncomplete {
				t.Fatalf("recheck: %+v, %v", checked, err)
			}
			if card := s.getBackupCard(t.Context()); card.lastVerifiedAt != nil || strings.Contains(card.newestDatabase, "VERIFIED") {
				t.Errorf("cached card still trusted the disproved snapshot: %+v", card)
			}
			_, listed := backupsBody(t, s)
			if listed.NewestVerified != nil || len(listed.Backups) != 1 || listed.Backups[0].DatabaseState != string(backup.StateIncomplete) {
				t.Errorf("console still trusted the disproved snapshot: %+v", listed)
			}
		})
	}
}
