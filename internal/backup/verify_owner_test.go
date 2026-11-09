package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// captureT captures a snapshot of src whose verification the test owes,
// released at the test's end.
func captureT(t *testing.T, src backup.Sources) *backup.Capture {
	t.Helper()
	c, err := backup.CaptureSnapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("CaptureSnapshot: %v", err)
	}
	t.Cleanup(c.Release)
	return c
}

// agePending moves the PENDING time of the snapshot in dir past PendingBound,
// so that no age could keep it from the prune.
func agePending(t *testing.T, dir string) {
	t.Helper()
	m := readManifestT(t, dir)
	m.Verification.PendingSince = time.Now().Add(-backup.PendingBound - time.Hour)
	writeManifestT(t, dir, m)
}

// TestADeadCaptureIsPrunedAndALiveOneIsKept is backlog B653's second
// round's red for the prune's protection of a PENDING: a capture whose
// process died before its verification (its owner lock there, held by
// nobody) reads INCOMPLETE (interrupted) at once, young as it is, and the
// next completed pass prunes it past keep, the newest of them kept as the
// evidence, one at most; a capture whose verification is still owed (its
// owner lock held) reads PENDING and is kept, past keep and past
// PendingBound alike. Red on main, which kept every PENDING by its age
// alone: the dead ones stayed, and the live one, aged, was pruned.
func TestADeadCaptureIsPrunedAndALiveOneIsKept(t *testing.T) {
	src := storeWithTracks(t, 5)
	live := captureT(t, src)
	agePending(t, live.Dir)
	var dead []string
	for i := 0; i < 3; i++ {
		time.Sleep(1100 * time.Millisecond)
		c := captureT(t, src)
		c.Release()
		dead = append(dead, c.Dir)
	}
	for _, dir := range dead {
		requireVerdict(t, "a capture whose owner is gone", listed(t, dir), backup.StateIncomplete, backup.ReasonInterrupted,
			backup.StateIncomplete, backup.ReasonInterrupted)
	}
	requireVerdict(t, "a capture whose owner holds it", listed(t, live.Dir), backup.StatePending, "", backup.StatePending, "")

	time.Sleep(1100 * time.Millisecond)
	newest := verifiedSnapshot(t, src)
	if _, err := backup.Prune(filepath.Dir(newest), 1); err != nil {
		t.Fatal(err)
	}
	for i, dir := range dead {
		evidence := i == len(dead)-1
		if pathExists(t, dir) != evidence {
			t.Errorf("dead capture %d of %d: there %v after the prune; want only the newest, the evidence, kept", i+1, len(dead), pathExists(t, dir))
		}
	}
	if !pathExists(t, live.Dir) {
		t.Error("the prune removed a capture whose verification its owner still owes")
	}
	if !pathExists(t, newest) {
		t.Error("the prune removed the newest snapshot")
	}
}

// TestAQueuedVerificationKeepsItsCaptureFromThePrune is the folded case of
// backlog B653's second round: a capture is protected from its publication
// until its verification ends, not by the folder's lock alone. One
// verification parks between its check and its write, holding the slot and
// the lock; two more captures, made meanwhile, queue behind it for the slot
// (their waits seen, never timed), holding no lock but their owner locks,
// their PENDING aged past PendingBound; the parked one's prune then runs
// under its hold, at keep 1, and so does each queued one's after it. Every
// queued capture is still there when its turn comes, and each is verified.
// Red on main, which kept them by age alone: the first prune removed the
// queued ones, whose verifications then found no manifest.
func TestAQueuedVerificationKeepsItsCaptureFromThePrune(t *testing.T) {
	src := storeWithTracks(t, 5)
	first := captureT(t, src)
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetAfterCheckForTest(func(string) {
		once.Do(func() {
			close(parked)
			<-release
		})
	}))
	waits := make(chan struct{}, 2)
	t.Cleanup(backup.SetOnSlotWaitForTest(func() { waits <- struct{}{} }))
	keep := func() int { return 1 }
	results := make(chan backup.Result, 3)
	go func() { results <- backup.FinishCapture(context.Background(), first, keep, backup.VerifyOptions{}) }()
	<-parked
	var queued []*backup.Capture
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond)
		c := captureT(t, src)
		agePending(t, c.Dir)
		queued = append(queued, c)
		go func() { results <- backup.FinishCapture(context.Background(), c, keep, backup.VerifyOptions{}) }()
		<-waits
	}
	close(release)
	verified := map[string]bool{}
	for i := 0; i < 3; i++ {
		res := <-results
		if res.VerifyErr != nil || !res.Verify.Recorded {
			t.Errorf("%s: verification answered %v, recorded %v; want each capture verified in its turn",
				filepath.Base(res.Dir), res.VerifyErr, res.Verify.Recorded)
			continue
		}
		verified[res.Dir] = true
	}
	for _, c := range queued {
		if !verified[c.Dir] {
			t.Errorf("the queued capture %s was not verified", filepath.Base(c.Dir))
		}
	}
}

// TestTheReaperSparesACaptureUnderWay: a capture's folder holds no manifest
// until the capture's end, and the orphan sweep reaps a folder with none
// once its grace has passed; a capture under way past the grace (a large
// database on a slow disk) is its owner's, and the sweep leaves it, then
// reaps it once its owner is gone. Red on main, which asked the grace alone.
func TestTheReaperSparesACaptureUnderWay(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026-01-01T00-00-00Z")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := backup.TakeOwnerForTest(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	aged := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatal(err)
	}
	if n, err := backup.ReapOrphans(root, time.Hour); err != nil || n != 0 || !pathExists(t, dir) {
		t.Fatalf("the sweep beside a capture under way reaped %d (%v); want it left", n, err)
	}
	release()
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatal(err)
	}
	if n, err := backup.ReapOrphans(root, time.Hour); err != nil || n != 1 || pathExists(t, dir) {
		t.Errorf("the sweep once the capture's owner is gone reaped %d (%v); want the orphan reaped", n, err)
	}
}

// TestAClaimTakesOnlyWhatNoLiveOwnerHolds: a claim of a snapshot whose
// owner holds it is refused (ErrClaimedElsewhere), and one of a snapshot
// whose owner is gone takes it, and its check then finishes in place
// (FinishCapture), the record VERIFIED.
func TestAClaimTakesOnlyWhatNoLiveOwnerHolds(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	root := filepath.Dir(c.Dir)
	if _, err := backup.Claim(t.Context(), c.Dir, root); !errors.Is(err, backup.ErrClaimedElsewhere) {
		t.Fatalf("a claim of a snapshot its owner holds answered %v, want ErrClaimedElsewhere", err)
	}
	c.Release()
	claimed, err := backup.Claim(t.Context(), c.Dir, root)
	if err != nil {
		t.Fatalf("a claim of a snapshot whose owner is gone: %v", err)
	}
	requireVerdict(t, "a claimed snapshot", listed(t, c.Dir), backup.StatePending, "", backup.StatePending, "")
	res := backup.FinishCapture(t.Context(), claimed, func() int { return 1 }, backup.VerifyOptions{BackupsRoot: root})
	if res.VerifyErr != nil || !res.Verify.Record.Passes() {
		t.Fatalf("the claimed snapshot's check: %+v, %v", res.Verify.Record, res.VerifyErr)
	}
	requireVerdict(t, "the claimed snapshot once checked", listed(t, c.Dir), backup.StateVerified, "", backup.StateVerified, "")
}
