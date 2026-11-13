//go:build windows

package updater

// EXPERIMENT (not for merge): what Windows does to a backup binary just
// executed, and whether an open with DELETE access predicts a rename's
// refusal. Evidence for a later change to the crash rollback.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// expBuildExitingExe builds a program that exits at once, as a backup's
// version probe and settings check do.
func expBuildExitingExe(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module helper\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "exiting.exe")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	return out
}

func expCopy(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// expProbe opens path with DELETE access and full sharing, the access a
// rename's own open of its source takes, and closes it at once.
func expProbe(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return err
	}
	return windows.CloseHandle(h)
}

func expHeld(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// TestExperimentHoldAfterExec: a quiet backup, executed twice, then at once
// the probe and the rollback's own renames, as recoverCrashedInstall does.
// It logs; it fails only on a broken setup.
func TestExperimentHoldAfterExec(t *testing.T) {
	dir := t.TempDir()
	exe := expBuildExitingExe(t, dir)
	const rounds = 30
	var probeHeld, renameRefused, agree int
	var longest time.Duration
	for i := 0; i < rounds; i++ {
		d := filepath.Join(dir, fmt.Sprintf("r%02d", i))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		dst, bak := filepath.Join(d, "bridge.exe"), filepath.Join(d, "bridge.exe.bak")
		expCopy(t, exe, dst)
		expCopy(t, exe, bak)
		// Quiet first: wait until a fresh write's scan has let go of bak.
		quiet := time.Now()
		for expHeld(expProbe(bak)) && time.Since(quiet) < 20*time.Second {
			time.Sleep(20 * time.Millisecond)
		}
		writeHold := time.Since(quiet)
		for k := 0; k < 2; k++ {
			if err := exec.Command(bak).Run(); err != nil {
				t.Fatalf("run %s: %v", bak, err)
			}
		}
		perr := expProbe(bak)
		aside := dst + ".aside"
		if err := os.Rename(dst, aside); err != nil {
			t.Logf("round %d: vacate refused: %v", i, err)
			continue
		}
		rerr := os.Rename(bak, dst)
		if rerr != nil {
			_ = os.Rename(aside, dst)
		}
		held := 0 * time.Second
		if rerr != nil {
			start := time.Now()
			for expHeld(expProbe(bak)) && time.Since(start) < 20*time.Second {
				time.Sleep(10 * time.Millisecond)
			}
			held = time.Since(start)
			if held > longest {
				longest = held
			}
		}
		if expHeld(perr) {
			probeHeld++
		}
		if rerr != nil {
			renameRefused++
		}
		if expHeld(perr) == (rerr != nil) {
			agree++
		}
		t.Logf("round %d: write hold %v; probe after exec: %v; rename bak->dst: %v; held for %v more", i, writeHold.Round(time.Millisecond), perr, rerr, held.Round(time.Millisecond))
	}
	// Fails on purpose so CI prints the rounds (go test hides a passing test's log).
	t.Errorf("SUMMARY (fails on purpose to print): %d rounds; probe saw a hold %d; rename refused %d; probe agreed with the rename %d; longest hold after a refusal %v",
		rounds, probeHeld, renameRefused, agree, longest.Round(time.Millisecond))
}

// TestExperimentHeldBackupAtMain: a handle without delete sharing on the
// backup, let go after 500 ms, against main's rollback (no wait). It is
// expected to FAIL at main: the rollback should complete, and main's
// does not wait.
func TestExperimentHeldBackupAtMain(t *testing.T) {
	dir := t.TempDir()
	dst, bak := filepath.Join(dir, "bridge.exe"), filepath.Join(dir, "bridge.exe.bak")
	if err := os.WriteFile(dst, []byte("CRASHED"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := windows.UTF16PtrFromString(bak)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe while held: %v", expProbe(bak))
	released := make(chan struct{})
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = windows.CloseHandle(h)
		close(released)
	}()
	rerr := replaceRunningBinary(dst, bak)
	<-released
	t.Logf("probe after release: %v", expProbe(bak))
	got, _ := os.ReadFile(dst)
	if rerr != nil || string(got) != "KNOWN-GOOD" {
		t.Fatalf("main's rollback over a backup held 500 ms: err %v, binary %q; want it put back (expected RED at main: no wait)", rerr, got)
	}
}
