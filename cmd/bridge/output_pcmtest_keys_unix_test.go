//go:build unix

package main

import (
	"io"
	"os"
	"testing"
	"time"
)

// TestPCMTestKeysStopEndsAReadOfAFile pins the key reader on a file, as
// standard input is (#1303's review: the reader outlived the run): a line
// is read as a key, and stop ends a read that waits for the next line,
// with none written, and waits for the reader, whose keys then end.
func TestPCMTestKeysStopEndsAReadOfAFile(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	keys, stop := pcmTestKeys(r)
	if _, err := io.WriteString(w, " P \n"); err != nil {
		t.Fatal(err)
	}
	watchdog := time.NewTimer(time.Minute)
	defer watchdog.Stop()
	select {
	case k := <-keys:
		if k != "p" {
			t.Errorf("key %q, want p", k)
		}
	case <-watchdog.C:
		t.Fatal("waited a minute for the key")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	awaitRun(t, stopped, "stop to end the reader's wait for a line")
	if k, ok := <-keys; ok {
		t.Errorf("a key %q after stop", k)
	}
}
