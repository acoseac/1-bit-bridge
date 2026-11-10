package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The rename publishes FAILED over PENDING, with no VERIFIED and no veto.
// A failed directory sync must not be reported as a failure to publish.
func TestTheConsoleSaysAVerdictWasWrittenButNotDurable(t *testing.T) {
	s, _, _ := newTestServer(t)
	src := liveStore(t, t.TempDir())
	s.deps.BackupSources = src
	capture, err := backup.CaptureSnapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(capture.Release)
	for _, name := range []string{backup.ManifestDBFileName, "server.crt"} {
		if err := os.WriteFile(filepath.Join(capture.Dir, name), []byte("damaged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mpath := filepath.Join(capture.Dir, backup.ManifestFile)
	// The sync reports the path the verdict was published at; on macOS a
	// temporary directory under /var is reached through /private/var.
	samePath := func(a, b string) bool {
		if a == b {
			return true
		}
		ra, errA := filepath.EvalSymlinks(filepath.Dir(a))
		rb, errB := filepath.EvalSymlinks(filepath.Dir(b))
		return errA == nil && errB == nil && ra == rb && filepath.Base(a) == filepath.Base(b)
	}
	published := false
	t.Cleanup(backup.SetSyncVerdictForTest(func(path string) error {
		if !samePath(path, mpath) {
			t.Errorf("directory sync for %s, want %s", path, mpath)
		}
		data, err := os.ReadFile(mpath)
		var m backup.Manifest
		if err == nil {
			err = json.Unmarshal(data, &m)
		}
		published = err == nil && m.Verification != nil && m.Verification.DatabaseState == backup.StateFailed && m.Verification.FilesState == backup.StateFailed
		return errors.New("injected: the verdict's directory sync failed")
	}))
	logs := loggingtest.Record(t)
	t.Cleanup(func() { s.WaitForBackupJobs(); backuptest.AwaitIdle(t) })
	s.verifyInBackground(capture, filepath.Dir(capture.Dir), 0)
	s.WaitForBackupJobs()
	if !published {
		t.Fatal("the sync seam did not see the FAILED record already published")
	}
	lines := logs.Lines("admin: a snapshot's verdict was written, not known durable")
	if len(lines) != 1 || !strings.Contains(lines[0], "write=not-durable") || !strings.Contains(lines[0], "recordRead=true") {
		t.Errorf("the job logged %q; want written, not durable, and read back\nall: %q", lines, logs.All())
	}
	for _, line := range logs.All() {
		if strings.Contains(line, "not recorded") {
			t.Errorf("published verdict described as not recorded: %s", line)
		}
	}
	_, body := backupsBody(t, s)
	if len(body.Backups) != 1 || body.Backups[0].DatabaseState != string(backup.StateFailed) || body.Backups[0].FilesState != string(backup.StateFailed) {
		t.Errorf("the console did not read the published FAILED record: %+v", body.Backups)
	}
}
