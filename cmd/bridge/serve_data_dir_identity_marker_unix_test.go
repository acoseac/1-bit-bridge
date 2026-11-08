//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// B741's round 3 (the Claude API on #1441): the marker synced where it is was
// opened for writing on every OS, where only Windows needs that
// (FlushFileBuffers); Linux and macOS sync a descriptor opened for reading.

// TestAMarkerThisUserMayNotWriteIsTakenUp: a marker serve may read and may
// not write (another user's, as where root owns the data dir and a member of
// its group serves it, or an immutable one; here mode 0444, which refuses its
// owner a write too, but for root) whose identity the record does not hold
// yet, there being no record or one written before B741, is taken up into the
// record through the real sync. Until round 3 that sync's open asked to
// write, failed, and the identity was never taken up: every start wrote the
// record without it, the data dir protected by its database and its lock file
// alone.
func TestAMarkerThisUserMayNotWriteIsTakenUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a file whose mode refuses its owner a write")
	}
	const id = "0123456789abcdef0123456789abcdef"
	for _, record := range []string{"no record", "a record written before B741"} {
		t.Run(record, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			dataDir := filepath.Join(base, "data")
			mkdirs(t, lib, dataDir)
			touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
			writeIdentity(t, dataDir, id)
			marker := dataDirMarkerFile(dataDir)
			if err := os.Chmod(marker, 0o444); err != nil {
				t.Fatal(err)
			}
			if f, err := os.OpenFile(marker, os.O_RDWR, 0); err == nil {
				_ = f.Close()
				t.Skip("this user may write a file whose mode refuses its owner a write")
			}
			cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
			if record != "no record" {
				writePreIdentityRecord(t, cfg, dataDir)
			}
			stopServed(t, bootServe(t, "--config", cfg))
			if got := recordedIdentity(t, cfg); got != id {
				t.Errorf("the record holds the identity %q, want the marker's %q, taken up through a sync of the marker opened for reading", got, id)
			}
			if got := identityOf(t, dataDir); got != id {
				t.Errorf("the marker holds %q, want %q, as it was", got, id)
			}
		})
	}
}

// TestTheMarkersSyncWaitsForNoNamedPipe: the marker's sync in place opens it
// as a file (fsutil.OpenAsFile), so a named pipe found at its path, swapped in
// after its rename or its read, is refused at once, never waited on for a
// writer: an open for reading alone of a named pipe blocks until one comes,
// and would hold serve's start.
func TestTheMarkersSyncWaitsForNoNamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), dataDirMarkerName)
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this host makes no named pipe here: %v", err)
	}
	answered := make(chan error, 1)
	go func() { answered <- syncMarkerFile(fifo) }()
	select {
	case err := <-answered:
		if err == nil {
			t.Error("the marker's sync took a named pipe for the marker")
		}
	case <-time.After(10 * time.Second):
		// Let the open waiting for a writer go before the test ends.
		if w, err := os.OpenFile(fifo, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		<-answered
		t.Fatal("the marker's sync waited for a writer to a named pipe at the marker's path")
	}
}
