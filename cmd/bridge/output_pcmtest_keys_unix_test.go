//go:build unix

package main

import (
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// TestPCMTestKeysLeaveTheFilesModeAlone pins that the key reader changes
// nothing about the file it reads (#1303's review: it took the descriptor
// with Fd, which puts a file Go made non-blocking back into blocking mode,
// on the open file description every holder of the descriptor shares). An
// os.Pipe read end is such a file: after a key and a stop it is non-blocking
// still. Its mode is read through SyscallConn, since Fd would change it.
func TestPCMTestKeysLeaveTheFilesModeAlone(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	nonblocking := func() bool {
		t.Helper()
		rc, err := r.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var flags int
		var ferr error
		if err := rc.Control(func(fd uintptr) { flags, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
			t.Fatal(err)
		}
		if ferr != nil {
			t.Fatal(ferr)
		}
		return flags&unix.O_NONBLOCK != 0
	}
	if !nonblocking() {
		t.Fatal("os.Pipe's read end is blocking: the test needs a file Go made non-blocking")
	}
	keys, stop := pcmTestKeys(r)
	if _, err := io.WriteString(w, "p\n"); err != nil {
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
	awaitRun(t, stopped, "stop to end the reader")
	if !nonblocking() {
		t.Error("the key reader left the pipe's read end blocking: it changed the mode of a file it only reads")
	}
}
