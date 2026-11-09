//go:build unix

package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// fifoWriter waits until a reader has the named pipe at path open, and
// answers a writer of it, which the reader's read then waits on: a read of
// the manifest that does not answer, as a read of a hung mount does not.
func fifoWriter(t *testing.T, path string) *os.File {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("open the manifest's pipe for writing: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing read the manifest's pipe")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAStopWhileTheManifestReadHangsReturnsWithinItsBoundAndKeepsTheLock is
// backlog B653's second round's red for the bound over a verification's
// reads: a verification stopped while its read of the snapshot's manifest
// does not answer (the manifest a named pipe no one writes, as a read of a
// hung mount waits) answers its caller within StopWriteBound, and the step
// it abandons keeps its hold until it ends, given up and marked since the
// fourth round (hold.go), so a prune meanwhile is skipped for it, and runs
// once the read has answered and the verification has ended, the record then
// saying the check did not end. Red on the round before, whose caller read
// the manifest itself, outside any bound.
func TestAStopWhileTheManifestReadHangsReturnsWithinItsBoundAndKeepsTheLock(t *testing.T) {
	src := storeWithTracks(t, 5)
	c := captureT(t, src)
	mpath := filepath.Join(c.Dir, backup.ManifestFile)
	data := readBytes(t, mpath)
	if err := os.Remove(mpath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(mpath, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan backup.Result, 1)
	go func() { done <- backup.FinishCapture(ctx, c, keepOne, backup.VerifyOptions{}) }()
	w := fifoWriter(t, mpath)
	fed := false
	feed := func() {
		if !fed {
			fed = true
			_, _ = w.Write(data)
			_ = w.Close()
		}
	}
	// Whatever fails, the read is answered before the test's end.
	t.Cleanup(feed)
	stoppedAt := time.Now()
	cancel()
	requireReturnedWithin(t, done, stoppedAt, backup.StopWriteBound+time.Second, "a verification stopped in its manifest's read")
	pr, err := backup.Prune(filepath.Dir(c.Dir), 7)
	if err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
		t.Errorf("a prune beside the abandoned verification: %+v, %v; want it skipped for the hold given up", pr, err)
	}
	// The manifest a file again for every read after this one; the reader's
	// open of the pipe is answered by the writer.
	if err := os.Remove(mpath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	feed()
	awaitVerificationEnd(t, c.Dir)
	pr, err = backup.Prune(filepath.Dir(c.Dir), 7)
	if err != nil || pr.Skipped {
		t.Errorf("a prune once the abandoned verification ended: %+v, %v; want it run", pr, err)
	}
	if v := listed(t, c.Dir); v.DatabaseState != backup.StateIncomplete {
		t.Errorf("the stopped verification's snapshot reads %s %v, want INCOMPLETE", v.DatabaseState, v.DatabaseReasons)
	}
}
