//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/proctest"
)

// wrappedTailscaleScript is the "app" behind the fake wrapper: `status`
// answers at once with a MagicDNS name, so the startup pass goes on to
// mint; `cert` records its pid, waits to be released, and only then
// writes the file it was given, as `tailscale cert` does. PIDFILE and
// RELEASE are replaced with paths.
const wrappedTailscaleScript = `#!/bin/sh
case "$1" in
status)
	printf '%s' '{"Self":{"HostName":"bridge","DNSName":"bridge.example.ts.net.","TailscaleIPs":["100.64.0.1"]},"MagicDNSSuffix":"example.ts.net"}'
	exit 0 ;;
cert)
	for arg in "$@"; do
		case "$arg" in --cert-file=*) cert="${arg#--cert-file=}" ;; esac
	done
	echo $$ > 'PIDFILE.tmp' && mv 'PIDFILE.tmp' 'PIDFILE'
	while [ ! -e 'RELEASE' ]; do sleep 0.02; done
	: > "$cert"
	exit 0 ;;
esac
exit 1
`

// TestServeLeavesNoTailscaleCLIRunning is the flaky boot test's defect,
// made deterministic and run on the real exec path, with no seam: serve
// finds a fake `tailscale` on PATH and drives it through internal/tailscale
// exactly as it drives the real one.
//
// The fake is shaped like the CLI helper the standalone macOS app
// installs at /usr/local/bin/tailscale, a shell script that runs the
// app binary WITHOUT exec. That shape is the whole defect: the context's
// kill reached the shell and never the app, which went on to write
// <dataDir>/tls after serve had returned. Measured with a stand-in:
// Wait took 2369 ms to return from a cancel at 200 ms, the full length
// of the orphan's work, and the orphan's file appeared regardless.
//
// So the assertion is about processes, not timing: once runServe has
// returned, the CLI process it started must be gone, and releasing it
// must write nothing.
func TestServeLeavesNoTailscaleCLIRunning(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	release := filepath.Join(bin, "release")
	inner := filepath.Join(bin, "tailscale-app")
	script := strings.NewReplacer("PIDFILE", pidFile, "RELEASE", release).Replace(wrappedTailscaleScript)
	if err := os.WriteFile(inner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\n'"+inner+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	let := func() {
		if f, err := os.OpenFile(release, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
	}

	cfgPath := writeValidConfig(t)
	certPath := filepath.Join(filepath.Dir(cfgPath), "data", "tls", "tailscale.crt")
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0"}, &safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Runs before the drain: whatever state a failure left, the fake is
	// released rather than left waiting past the test.
	t.Cleanup(let)

	pid := waitForCLIPid(t, pidFile, done, stderr)

	cancel()
	select {
	case <-exited:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatalf("runServe did not return after the cancel; stderr=%s", stderr.String())
	}

	// Serve has returned. The `tailscale cert` it started must have
	// exited: a killed process has within milliseconds, a live one never
	// does. Asked through proctest, not kill(pid, 0): nothing in this test
	// reaps the CLI, serve's grandchild, and where init does not either (a
	// container run without --init) it stays a zombie, which kill(pid, 0)
	// calls running.
	for deadline := time.Now().Add(5 * time.Second); ; {
		exited, why := proctest.Exited(pid)
		if exited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve has returned, but the `tailscale cert` it started (pid %d) is still "+
				"running (%s). It will write into the data dir whenever it "+
				"finishes, after serve is gone. stderr=%s", pid, why, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	let()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(certPath); err == nil {
		t.Fatal("the `tailscale cert` serve had cancelled wrote its cert after serve returned")
	}
}

// waitForCLIPid waits for the fake `tailscale cert` to record its pid. A
// serve that exits first is reported with its exit code, not as a
// timeout.
func waitForCLIPid(t *testing.T, pidFile string, done <-chan int, stderr *safeBuffer) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatalf("pid file %q: %v", b, err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the auto-pilot never started `tailscale cert` within 30s; stderr=%s", stderr.String())
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited with code %d before its auto-pilot started `tailscale cert`; stderr=%s",
				code, stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
