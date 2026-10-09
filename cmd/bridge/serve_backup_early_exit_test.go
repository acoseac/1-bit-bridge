package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnEarlyExitDoesNotWaitOutTheBackupTicker is backlog B311. B309's
// backup wait is a defer registered after `defer scanCancel()`, so it runs
// first. On a stop the parent context has already cancelled the ticker, but
// on an early error return nothing had, and the wait sat out the whole
// backupShutdownWait before teardown went on. Bridge #1166's CI hit it when
// an admin port collision exited serve: the case took 45.66 s and logged
// that the backup snapshot "did not close its files".
//
// Here the test holds the admin port, so serve fails at the admin bind,
// after the backup ticker has started. The wait has no bound of its own:
// with the defect, serve still exits, 45 s later, with the line asserted
// below.
func TestAnEarlyExitDoesNotWaitOutTheBackupTicker(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "Music")
	dataDir := filepath.Join(root, "data")
	for _, dir := range []string{lib, dataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	listenAddr := fmt.Sprintf("127.0.0.1:%d", freeLoopbackPort(t))
	configPath := filepath.Join(root, "bridge.yaml")
	yamlText := "libraryRoots:\n  - " + lib + "\ndataDir: " + dataDir +
		"\nadminAddress: " + held.Addr().String() + "\n"
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: configPath, addrOverride: listenAddr}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)

	var code int
	select {
	case code = <-done:
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit with its admin port held\n%s", stderr.String())
	}
	out := stderr.String()
	if code == 0 {
		t.Fatalf("serve exited 0 with its admin port held\n%s", out)
	}
	if !strings.Contains(out, "admin listen") {
		t.Fatalf("serve did not exit at the admin bind, so this did not test the early exit\n%s", out)
	}
	if strings.Contains(out, "did not close its files") {
		t.Fatalf("an early exit waited out the backup ticker\n%s", out)
	}
}
