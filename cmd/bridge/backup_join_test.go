package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// awaitVerifyEndOnCleanup hears the end of the worker of the verification of
// dir that begins from now on, dir as its caller passes it
// (backup.SetOnVerifyEndForTest, whose hook a worker takes as its
// verification begins and runs last, once it has let go of everything it
// held), and registers the test's wait for it: registered now, it runs
// before every cleanup registered before it, so the test puts no seam back,
// removes no fixture and ends only once that worker has ended. A
// verification answers its caller before its worker lets go
// (backup.verifyAndThen), so a command that verifies returns while its
// worker still reads the backup package's seams (releaseBound, the lock's
// release hook): backlog B871's round 3, whose CI race was the next test's
// SetReleaseBoundForTest beside the release of a verification bridge backup
// --verify had answered. The verification must begin: a wait for one refused
// before its worker starts runs out, failing the test.
func awaitVerifyEndOnCleanup(t *testing.T, dir string) {
	t.Helper()
	ended := make(chan struct{})
	heard := sync.OnceFunc(func() { close(ended) })
	t.Cleanup(backup.SetOnVerifyEndForTest(func(d string) {
		if d == dir {
			heard()
		}
	}))
	t.Cleanup(func() {
		if onVerifyEndAwait != nil {
			onVerifyEndAwait(dir)
		}
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Errorf("the worker of the verification of %s never ended", dir)
		}
	})
}

// onVerifyEndAwait, nil but in TestEachHoldWordsCaseWaitsForItsVerificationsEnd,
// runs as a test's cleanup begins to wait for a verification's end
// (awaitVerifyEndOnCleanup), given the snapshot as that verification was
// given it: the test lets a worker it parked go on there, and only there.
var onVerifyEndAwait func(dir string)

// joinBoundedReadsOnCleanup hears every read readBounded starts from now on
// (readBoundedStart) and registers the test's wait for each to end, those the
// ticker's stop left to end on their own included, for 30 s at most: such a
// read goes on reading the backup package's seams past its caller's answer,
// and nothing else waits for it (backlog B871's round 3). Registered now, the
// wait runs before every cleanup registered before it, and after those
// registered after it, which let a stalled read go and stop the ticker.
// t.Errorf, never t.Fatalf, as a cleanup must.
func joinBoundedReadsOnCleanup(t *testing.T) {
	t.Helper()
	var reads sync.WaitGroup
	start := func() func() {
		reads.Add(1)
		return reads.Done
	}
	prev := readBoundedStart.Swap(&start)
	t.Cleanup(func() { readBoundedStart.Store(prev) })
	t.Cleanup(func() {
		ended := make(chan struct{})
		go func() {
			reads.Wait()
			close(ended)
		}()
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Error("a read the backup ticker left to end on its own never ended")
		}
	})
}

// TestADrainWaitsForTheBackupWorkerItsLoopLeft is backlog B871's round 3 for
// the drains (drainLoopOnCleanup, drainServeOnCleanup, stopServed): a loop, as
// serve's backup ticker is one, returns once its verification has answered,
// and the verification's worker lets go of the backups folder's lock only then
// (backup.verifyAndThen). Here that worker is parked as it lets the lock go
// (backup.SetOnLockReleaseForTest), after its answer and before its hold
// drops, and let go on only once the drain waits for the backup holds
// (onBackupHoldsAwait): a drain that returned once the loop had would end the
// subtest with the worker still parked. Red on the round before, whose drains
// waited for the loop alone.
func TestADrainWaitsForTheBackupWorkerItsLoopLeft(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := fsutil.ResolveLinks(root)
	if err != nil {
		t.Fatal(err)
	}
	parked, letGo := make(chan struct{}), make(chan struct{})
	park, release := sync.OnceFunc(func() { close(parked) }), sync.OnceFunc(func() { close(letGo) })
	t.Cleanup(backup.SetOnLockReleaseForTest(func(d string) {
		if d == folder {
			park()
			<-letGo
		}
	}))
	var waited atomic.Bool
	prevAwait := onBackupHoldsAwait
	onBackupHoldsAwait = func(string) {
		waited.Store(true)
		release()
	}
	t.Cleanup(func() { onBackupHoldsAwait = prevAwait })
	// A drain that does not wait leaves the worker parked: it is let go here,
	// and its end waited for, before the hooks and the folder go.
	t.Cleanup(func() {
		release()
		backuptest.AwaitIdle(t)
	})
	t.Run("a loop that verifies", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the verifying loop")
		go func() {
			defer close(done)
			_, _ = backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{BackupsRoot: root})
		}()
		select {
		case <-parked:
		case <-time.After(30 * time.Second):
			t.Fatal("the verification's worker never came to let the lock go")
		}
	})
	if !waited.Load() {
		t.Fatal("the drain returned with the loop's verification worker parked as it let the lock go: it never waited for the backup holds")
	}
}
