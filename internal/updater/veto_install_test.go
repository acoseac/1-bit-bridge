//go:build !windows

package updater

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestANewInstallsMarkerCarriesNoVeto: the real Install over the marker a
// finished early rollback left writes a fresh install marker, which carries
// no veto of the version's store migrations, so a reinstall lifts it
// (backlog B613's round 4). Not on Windows, as every install test here: the
// fixture's archive is a tar.gz holding `bridge`.
func TestANewInstallsMarkerCarriesNoVeto(t *testing.T) {
	fix := newInstallFixture(t, "0.2.3")
	dir := t.TempDir()
	finishedRollback(t, dir)
	live := filepath.Join(dir, "bridge")
	if err := os.WriteFile(live, []byte("bridge-binary-0.2.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	upd := New(Options{RepoOverride: "fake/repo", Client: NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL)})
	upd.mu.Lock()
	upd.status.CurrentVersion, upd.status.LatestVersion, upd.status.UpdateAvailable = "0.2.1", "0.2.3", true
	upd.mu.Unlock()
	if _, err := upd.Install(context.Background(), InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
		t.Fatalf("Install = %v", err)
	}
	if st, err := LoadState(dir); err != nil || st.MigrationVeto != "" || st.Status != "installing" {
		t.Errorf("the marker after a reinstall = %+v (%v), want a fresh install marker with no veto", st, err)
	}
}
