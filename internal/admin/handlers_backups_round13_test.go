package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
)

// The backup package's caller tests prove a bundle veto can survive give-up
// without a root stamp. Exercise that disk state against the real console
// cache: an uncached listing sees it immediately; the card sees it at expiry.
func TestTheConsoleCacheExpiresAfterAnUnstampedBundleVeto(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.BackupSources = liveStore(t, t.TempDir())
	root := filepath.Join(s.deps.BackupSources.DataDir, backup.BackupsDirName)
	t.Cleanup(func() { backuptest.AwaitIdle(t) })
	res := backup.SnapshotAndVerify(t.Context(), s.deps.BackupSources, backup.SnapshotOptions{})
	backuptest.AwaitIdle(t)
	if res.VerifyErr != nil || !res.Verify.Record.Passes() {
		t.Fatalf("initial snapshot: %+v", res)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(root, past, past); err != nil {
		t.Fatal(err)
	}
	if card := s.getBackupCard(t.Context()); card.lastVerifiedAt == nil {
		t.Fatalf("cache was not primed: %+v", card)
	}
	stamp := folderStamp(root)
	if err := os.WriteFile(filepath.Join(res.Dir, ".verdict-unsynced"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !folderStamp(root).Equal(stamp) {
		t.Fatal("fixture unexpectedly invalidated the root stamp")
	}
	_, listed := backupsBody(t, s)
	if listed.NewestVerified != nil || len(listed.Backups) != 1 || listed.Backups[0].DatabaseState != string(backup.StateIncomplete) {
		t.Errorf("uncached listing ignored the veto: %+v", listed)
	}
	if card := s.getBackupCard(t.Context()); card.lastVerifiedAt == nil || !strings.Contains(card.newestDatabase, "VERIFIED") {
		t.Errorf("unstamped veto unexpectedly evicted the cached reading: %+v", card)
	}
	s.lastBackupMu.Lock()
	s.lastBackupAt = time.Now().Add(-lastBackupCacheTTL - time.Second)
	s.lastBackupMu.Unlock()
	if card := s.getBackupCard(t.Context()); card.lastVerifiedAt != nil || !strings.Contains(card.newestDatabase, "INCOMPLETE") {
		t.Errorf("expired cache still trusted the vetoed record: %+v", card)
	}
}
