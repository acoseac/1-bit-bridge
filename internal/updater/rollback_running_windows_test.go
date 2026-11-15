//go:build windows

package updater

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// helperReadyReserve is what the ready wait leaves of the test
// deadline for the kill, the Wait, and the temp-dir cleanup.
const helperReadyReserve = 5 * time.Second

// helperReadyGiveUp fires at the test deadline less helperReadyReserve.
// With no deadline it is nil, and a select on it never fires.
func helperReadyGiveUp(t *testing.T) <-chan time.Time {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(time.Until(deadline.Add(-helperReadyReserve)))
}

// TestRollbackBinaryReplacesARunningExe is the rollback the console and
// the boot path both run inside the bridge process, which stops no
// service (backlog B898), so the rename replaces the mapped image.
// Windows refuses that ("Access is denied"). The helper is a real
// process started from dst, so the image is mapped the way bridge.exe is
// during those callers. In the second case the backup is held as well,
// sharing nothing, as a scanner holds the backup a start has just run,
// and let go once its refusal is heard.
func TestRollbackBinaryReplacesARunningExe(t *testing.T) {
	for _, backupHeld := range []bool{false, true} {
		dir := t.TempDir()
		dst := buildSleepingExe(t, dir)
		bak := dst + ".bak"
		const good = "KNOWN-GOOD-ROLLBACK"
		if err := os.WriteFile(bak, []byte(good), 0o755); err != nil {
			t.Fatal(err)
		}
		runUntilReady(t, dst, dir)
		var released atomic.Bool
		realHolds(t, &released)
		var heard *atomic.Int32
		if backupHeld {
			heard = letGoAtRefusalOf(t, bak, holdShareNone(t, bak), &released)
		}

		if err := RollbackBinary(dst, ".bak"); err != nil {
			t.Fatalf("backup held %v: RollbackBinary: %v", backupHeld, err)
		}
		if heard != nil && heard.Load() < 1 {
			t.Error("the held backup was never refused")
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != good {
			t.Fatalf("dst = %q, want the .bak bytes", got)
		}
		if _, err := os.Stat(bak); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf(".bak should be consumed; stat = %v", err)
		}
	}
}

// buildSleepingExe compiles a tiny process that writes a ready file and
// then sleeps. The test starts that binary at the path RollbackBinary
// replaces, so the image is mapped the way a running bridge.exe is.
func buildSleepingExe(t *testing.T, dir string) string {
	t.Helper()
	srcDir := filepath.Join(dir, "src")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const src = `package main

import (
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		os.Exit(1)
	}
	f.Close()
	time.Sleep(time.Hour)
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module helper\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "bridge.exe")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	return exe
}
