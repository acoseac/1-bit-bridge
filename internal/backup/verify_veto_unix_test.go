//go:build unix

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestAVerifiedRecordLeftForAFailedOneIsVetoedWhereItsBundleTakesNoFile is
// TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails over a real
// failure, no seam making it: a verified snapshot whose database and files
// fail when checked again, its folder taking no new file (mode 0500), cannot
// stage its record, which keeps its VERIFIED; the veto goes beside the
// bundle, and no reader reads that VERIFIED after the error, the prune
// keeping the older verified snapshot. Once the folder takes files again the
// next check writes the record and lets the veto go. Red on the round
// before, which put no veto over a record that would hold none. The prune
// runs once the verification's worker has ended: the verification answers
// before that worker lets go of the backups folder's lock, whose unlock is
// held here from the answer until the test waits for its end (unlockHold,
// backlog B851), and a prune skipped for that lock kept the older snapshot
// whatever it read. Those hooks hold the unlock and hear the end; they make
// no failure and change no verdict.
func TestAVerifiedRecordLeftForAFailedOneIsVetoedWhereItsBundleTakesNoFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a folder's mode 0500 refuses root nothing")
	}
	src := storeWithTracks(t, 20)
	anchor := verifiedSnapshot(t, src)
	time.Sleep(1100 * time.Millisecond)
	dir := verifiedSnapshot(t, src)
	flipLastByte(t, dir, backup.ManifestDBFileName)
	flipLastByte(t, dir, "tokens.json")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	unlock := holdUnlockOf(t, filepath.Dir(dir))
	joinBackupCallers(t, unlock.release)
	unlock.arm(t, dir)
	res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	if res.Found.DatabaseState != backup.StateFailed || res.Found.FilesState != backup.StateFailed {
		t.Fatalf("the check found %s/%s, want FAILED/FAILED: the test did not reach the case", res.Found.DatabaseState, res.Found.FilesState)
	}
	if err == nil {
		t.Fatal("a verification whose bundle takes no file answered no error")
	}
	unlock.requireHeld(t)
	unlock.letGoAndAwaitEnd(t)
	if m := readManifestT(t, dir); m.Verification.DatabaseState != backup.StateVerified {
		t.Fatalf("the record holds %s: the staging did not fail", m.Verification.DatabaseState)
	}
	if got := readersVerified(t, dir); len(got) > 0 {
		t.Errorf("after the error, %s", strings.Join(got, "; "))
	}
	if !pathExists(t, backup.RootVetoPath(dir)) {
		t.Error("no veto beside a bundle that takes no file")
	}
	// The prune and the next check answer before their workers let go: each
	// worker's end comes before the next step and before the hooks go back
	// (backlog B871's round 3).
	pr, err := backup.Prune(filepath.Dir(dir), 1)
	awaitClean(t)
	if err != nil || pr.Skipped {
		t.Fatalf("the prune answered %+v, %v; want it run: a skipped prune keeps the anchor whatever it reads", pr, err)
	}
	if !pathExists(t, anchor) {
		t.Error("the prune let the older verified snapshot go")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	awaitClean(t)
	if err != nil {
		t.Fatalf("the next check: %v", err)
	}
	requireVerdict(t, "the record after the next check", listed(t, dir), backup.StateFailed, backup.ReasonHash, backup.StateFailed, backup.ReasonHash)
	if pathExists(t, backup.RootVetoPath(dir)) {
		t.Error("the next check's write left the veto beside the bundle")
	}
}
