//go:build !windows

package main

import (
	"crypto/tls"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// TestAServeKilledWhileItDrainsHasTakenItsCountBack runs this tree's serve as
// a process over an install on probation, holds a connection that keeps its
// drain going (a TLS client whose request never ends), stops it with SIGTERM
// as a service manager does, and kills it with SIGKILL while it still drains,
// as launchd does past its ExitTimeOut and systemd past TimeoutStopSec: the
// stop's count is taken back already (backlog B660's second round). Until
// then the take-back came after the whole teardown, and a stop killed in it
// stayed counted. Not on Windows, whose stop is the service manager's and
// whose processes take no SIGTERM.
func TestAServeKilledWhileItDrainsHasTakenItsCountBack(t *testing.T) {
	r := newRefusedInstall(t, "mdns:\n  enabled: false\n")
	if err := os.Remove(r.bin); err != nil {
		t.Fatal(err)
	}
	// The install's binary is this tree's bridge, a copy of the test binary
	// that says it is this version.
	installBridgeCopy(t, r.bin, version.ServerVersion)
	cmd := exec.Command(r.bin, "serve", "--config", r.cfg)
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	addr := ""
	deadline := time.Now().Add(withinTheTest)
	for addr == "" {
		if _, a, ok := strings.Cut(stdout.String(), "listening on https://"); ok {
			if line, _, ok := strings.Cut(a, "\n"); ok {
				addr = strings.TrimSpace(line)
				break
			}
		}
		select {
		case <-exited:
			t.Fatalf("the serve process exited before it listened (%v); stderr=%s", waitErr, stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serve process did not listen; stderr=%s", stderr.String())
		}
	}
	r.requireCount(t, 1, "while the serve process runs")
	// A connection serve's drain waits for: the handshake done, so serve has
	// taken it, and a request begun and never finished.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/health HTTP/1.1\r\nHost: " + addr + "\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(stdout.String(), "Shutting down") {
		select {
		case <-exited:
			t.Fatalf("the serve process exited before its drain (%v); stderr=%s", waitErr, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Well inside the five seconds the held connection keeps the drain going.
	tookBack := r.waitForCountWithin(t, 0, 2500*time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v: the serve process was gone before its kill, so the test shows nothing", err)
	}
	<-exited
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the serve process ended %v, not by the kill: its drain was over before it, so the test shows nothing; stderr=%s",
			waitErr, stderr.String())
	}
	if !tookBack {
		t.Errorf("the marker after a stop killed while it drained = %+v, want the stop's count taken back", markerOf(t, r.dataDir))
	}
}
