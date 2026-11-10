package doctor

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
)

func TestTheDoctorReplacesAnUnsyncedRecordWithoutInferringDamage(t *testing.T) {
	for _, cause := range []string{"disproved files", "successful check with leftover veto"} {
		t.Run(cause, func(t *testing.T) {
			dataDir := t.TempDir()
			cert := filepath.Join(dataDir, "server.crt")
			if err := os.WriteFile(cert, []byte("captured"), 0o600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dataDir, backup.BackupsDirName)
			src := backup.Sources{DataDir: dataDir, ServerCert: cert}
			prev := backupsSyncsParentDir
			backupsSyncsParentDir = false
			t.Cleanup(func() { backupsSyncsParentDir = prev })
			t.Cleanup(backup.SetSyncsParentDirForTest(false))
			if cause == "successful check with leftover veto" {
				t.Cleanup(backup.SetRemoveVetoForTest(func(string) error { return errors.New("removal refused") }))
			}
			t.Cleanup(func() { backuptest.AwaitIdle(t) })
			res := backup.SnapshotAndVerify(context.Background(), src, backup.SnapshotOptions{})
			backuptest.AwaitIdle(t)
			if !res.Verify.Found.Passes() {
				t.Fatalf("initial check did not pass: %+v", res)
			}
			if cause == "disproved files" {
				if res.VerifyErr != nil {
					t.Fatal(res.VerifyErr)
				}
				if err := os.WriteFile(filepath.Join(res.Dir, "server.crt"), []byte("damaged!"), 0o600); err != nil {
					t.Fatal(err)
				}
				checked, err := backup.VerifySnapshot(context.Background(), res.Dir, backup.VerifyOptions{BackupsRoot: root})
				backuptest.AwaitIdle(t)
				if !errors.Is(err, backup.ErrRecheckNotRecorded) || checked.Found.FilesState != backup.StateFailed {
					t.Fatalf("disproof: %+v, %v", checked, err)
				}
			}
			st, err := backup.ReadStatus(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			sn, ok := st.Newest()
			if !ok || sn.Verdict.FilesState != backup.StateIncomplete {
				t.Fatalf("expected a vetoed record: %+v", st)
			}
			line := newestLine(sn, time.Now())
			if !strings.Contains(line.Hint, "cannot be trusted or re-recorded on this platform") || !strings.Contains(line.Hint, "fresh snapshot with bridge backup --config <bridge.yaml>") {
				t.Errorf("no actionable replacement advice: %q", line.Hint)
			}
			for _, falseClaim := range []string{"did not finish", "disproved", "damaged", "--verify"} {
				if strings.Contains(line.Hint, falseClaim) {
					t.Errorf("hint inferred %q from unsynced: %q", falseClaim, line.Hint)
				}
			}
			backupsSyncsParentDir = true
			if hint := newestLine(sn, time.Now()).Hint; !strings.Contains(hint, "--verify") {
				t.Errorf("barrier platform lost its recheck advice: %q", hint)
			}
		})
	}
}
