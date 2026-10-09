//go:build windows

package updater

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRollbackBinaryReplacesARunningExe is the rollback the console and
// the boot path both run inside the bridge process. The SCM stop is
// skipped when this process is the service, and the rename then replaces
// the mapped image. Windows refuses that ("Access is denied"). The
// helper is a real process started from dst, so the image is mapped the
// way bridge.exe is during those two callers.
func TestRollbackBinaryReplacesARunningExe(t *testing.T) {
	dir := t.TempDir()
	dst := buildSleepingExe(t, dir)
	bak := dst + ".bak"
	const good = "KNOWN-GOOD-ROLLBACK"
	if err := os.WriteFile(bak, []byte(good), 0o755); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(dst, ready)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	// One Wait, in the watcher. Cleanup kills and then joins that Wait.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-exited
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-exited:
			t.Fatal("helper exited before it became ready")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := RollbackBinary(dst, ".bak"); err != nil {
		t.Fatalf("RollbackBinary: %v", err)
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

	select {
	case <-exited:
		t.Fatal("helper exited during rollback")
	case <-time.After(500 * time.Millisecond):
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
