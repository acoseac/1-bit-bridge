//go:build !windows

package main

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// serveProcess is this tree's serve running as a process of its own over r's
// install, as a service manager runs it.
type serveProcess struct {
	cmd            *exec.Cmd
	stdout, stderr *safeBuffer
	exited         chan struct{}
	waitErr        error
	addr           string
}

// startServeProcess starts this tree's serve over r's install as a process,
// its probation asking whether it is ready every ask, and returns once it
// listens; the test's end kills it.
func startServeProcess(t *testing.T, r *refusedInstall, ask string) *serveProcess {
	t.Helper()
	if err := os.Remove(r.bin); err != nil {
		t.Fatal(err)
	}
	// The install's binary is this tree's bridge, a copy of the test binary
	// that says it is this version.
	installBridgeCopy(t, r.bin, version.ServerVersion)
	t.Setenv(runAsBridgeProbationAskEnv, ask)
	p := &serveProcess{cmd: exec.Command(r.bin, "serve", "--config", r.cfg), stdout: &safeBuffer{}, stderr: &safeBuffer{},
		exited: make(chan struct{})}
	p.cmd.Stdout, p.cmd.Stderr = p.stdout, p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.exited
	})
	p.waitFor(t, func() bool {
		if _, a, ok := strings.Cut(p.stdout.String(), "listening on https://"); ok {
			if line, _, ok := strings.Cut(a, "\n"); ok {
				p.addr = strings.TrimSpace(line)
				return true
			}
		}
		return false
	}, "listen")
	return p
}

// waitFor waits for done to hold, failing where the process exits first or
// the test's own bound passes; what names the wait.
func (p *serveProcess) waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(withinTheTest)
	for !done() {
		select {
		case <-p.exited:
			t.Fatalf("the serve process exited before it would %s (%v); stderr=%s", what, p.waitErr, p.stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serve process did not %s; stderr=%s", what, p.stderr.String())
		}
	}
}

// holdItsDrain opens a connection serve's drain waits for: the handshake
// done, so serve has taken it, and a request begun and never finished.
func (p *serveProcess) holdItsDrain(t *testing.T) net.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", p.addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("GET /v1/health HTTP/1.1\r\nHost: " + p.addr + "\r\n")); err != nil {
		t.Fatal(err)
	}
	return conn
}

// stopAndKillInItsDrain stops the process with SIGTERM as a service manager
// does, waits for its drain to begin, runs inDrain while it drains, and kills
// it with SIGKILL, as launchd does past its ExitTimeOut and systemd past
// TimeoutStopSec, requiring the kill to be what ended it.
func (p *serveProcess) stopAndKillInItsDrain(t *testing.T, inDrain func()) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	p.waitFor(t, func() bool { return strings.Contains(p.stdout.String(), "Shutting down") }, "begin its drain")
	inDrain()
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v: the serve process was gone before its kill, so the test shows nothing", err)
	}
	<-p.exited
	var exit *exec.ExitError
	if !errors.As(p.waitErr, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("the serve process ended %v, not by the kill: its drain was over before it, so the test shows nothing; stderr=%s",
			p.waitErr, p.stderr.String())
	}
}

// TestAServeKilledWhileItDrainsHasTakenItsCountBack runs this tree's serve as
// a process over an install on probation, waits for it to answer as ready
// (its line on stderr), holds a connection that keeps its drain going (a TLS
// client whose request never ends), stops it with SIGTERM and kills it with
// SIGKILL while it still drains: the stop's count is taken back already
// (backlog B660's second round; since the third, once the start has answered
// as ready). Until then the take-back came after the whole teardown, and a
// stop killed in it stayed counted. Not on Windows, whose stop is the service
// manager's and whose processes take no SIGTERM.
func TestAServeKilledWhileItDrainsHasTakenItsCountBack(t *testing.T) {
	r := newRefusedInstall(t, "mdns:\n  enabled: false\n")
	p := startServeProcess(t, r, "20ms")
	p.waitFor(t, func() bool { return strings.Contains(p.stderr.String(), "has answered as ready (its API listener") },
		"answer as ready")
	r.requireCount(t, 1, "while the serve process runs")
	p.holdItsDrain(t)
	tookBack := false
	p.stopAndKillInItsDrain(t, func() {
		// Well inside the five seconds the held connection keeps the drain going.
		tookBack = r.waitForCountWithin(t, 0, 2500*time.Millisecond)
	})
	if !tookBack {
		t.Errorf("the marker after a stop killed while it drained = %+v, want the stop's count taken back", markerOf(t, r.dataDir))
	}
}

// TestAServeKilledInItsTeardownBeforeReadyStaysCounted: the same stop and
// kill of a serve process that has not answered as ready leaves its count
// standing (backlog B660's third round): a version that hangs before it is
// ready, which its supervisor stops and kills, counts toward the install's
// crash rollback. The second round took the count back as the stop arrived,
// so such a version never reached its rollback.
func TestAServeKilledInItsTeardownBeforeReadyStaysCounted(t *testing.T) {
	r := newRefusedInstall(t, "mdns:\n  enabled: false\n")
	p := startServeProcess(t, r, "1h")
	r.requireCount(t, 1, "while the serve process runs, not ready yet")
	p.holdItsDrain(t)
	tookBack := false
	p.stopAndKillInItsDrain(t, func() {
		tookBack = r.waitForCountWithin(t, 0, time.Second)
	})
	if tookBack || strings.Contains(p.stderr.String(), "has answered as ready (its API listener") {
		t.Errorf("the marker after a stop before ready, killed while it drained = %+v, want the count to stand; stderr=%s",
			markerOf(t, r.dataDir), p.stderr.String())
	}
	r.requireCount(t, 1, "after the kill")
}
