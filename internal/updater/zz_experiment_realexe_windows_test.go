//go:build windows

package updater

// EXPERIMENT (not for merge; B872 part 2): the hold Windows keeps on a
// freshly written, real-sized bridge binary, and on one just executed as the
// crash rollback executes its backup (`version`, twice). #1468 found no hold
// after executing a small binary that had been left to go quiet. This asks
// whether the CI reds' holder is the host's scan of a fresh large binary.
// It logs; it fails only on a broken setup.

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

func rxBuildBridge(t *testing.T, dir string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "built.exe")
	cmd := exec.Command("go", "build", "-tags", "ts_omit_webclient,ts_omit_debugeventbus", "-o", out, "./cmd/bridge")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("built the bridge: %d bytes", fi.Size())
	return out
}

func rxCopy(t *testing.T, from, to string) {
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

// rxProbe opens path with DELETE access and full sharing, the access a
// rename's own open of its source takes, and closes it at once.
func rxProbe(path string) error {
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

func rxHeld(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// rxHoldFor polls the probe until it stops answering held, up to 30 s.
func rxHoldFor(path string) time.Duration {
	start := time.Now()
	for rxHeld(rxProbe(path)) && time.Since(start) < 30*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

type rxTally struct {
	rounds, probeHeld, renameRefused, agree int
	longest                                 time.Duration
}

// rxRound writes dst and bak fresh, optionally waits for bak to go quiet,
// executes bak twice as recoverCrashedInstall does, then probes and renames
// at once as the rollback does.
func rxRound(t *testing.T, built, d string, quietFirst bool, tally *rxTally) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	dst, bak := filepath.Join(d, "bridge.exe"), filepath.Join(d, "bridge.exe.bak")
	rxCopy(t, built, dst)
	rxCopy(t, built, bak)
	writeHeld := rxHeld(rxProbe(bak))
	var writeHold time.Duration
	if quietFirst {
		writeHold = rxHoldFor(bak)
	}
	execStart := time.Now()
	for k := 0; k < 2; k++ {
		if b, err := exec.Command(bak, "version").CombinedOutput(); err != nil {
			t.Logf("%s: run %d of %s version: %v (%s)", d, k, bak, err, b)
		}
	}
	execTime := time.Since(execStart)
	perr := rxProbe(bak)
	aside := dst + ".aside"
	vacateStart := time.Now()
	var verr error
	for i := 0; i < 7; i++ {
		if verr = os.Rename(dst, aside); verr == nil {
			break
		}
		time.Sleep(450 * time.Millisecond)
	}
	if verr != nil {
		t.Logf("%s: vacate refused for %v: %v", d, time.Since(vacateStart).Round(time.Millisecond), verr)
		return
	}
	rerr := os.Rename(bak, dst)
	if rerr != nil {
		_ = os.Rename(aside, dst)
	}
	var held time.Duration
	if rerr != nil {
		held = rxHoldFor(bak)
		if held > tally.longest {
			tally.longest = held
		}
	}
	tally.rounds++
	if rxHeld(perr) {
		tally.probeHeld++
	}
	if rerr != nil {
		tally.renameRefused++
	}
	if rxHeld(perr) == (rerr != nil) {
		tally.agree++
	}
	t.Logf("%s: held right after the write %v; write hold %v; two execs %v; probe after: %v; rename bak->dst: %v; held for %v more",
		filepath.Base(d), writeHeld, writeHold.Round(time.Millisecond), execTime.Round(time.Millisecond), perr, rerr,
		held.Round(time.Millisecond))
}

func TestExperimentRealExeHold(t *testing.T) {
	dir := t.TempDir()
	built := rxBuildBridge(t, dir)
	const rounds = 30
	for _, quiet := range []bool{true, false} {
		var tally rxTally
		for i := 0; i < rounds; i++ {
			rxRound(t, built, filepath.Join(dir, fmt.Sprintf("q%v-r%02d", quiet, i)), quiet, &tally)
		}
		t.Logf("SUMMARY quietFirst=%v: %d rounds; probe saw a hold %d; rename refused %d; probe agreed with the rename %d; longest hold after a refusal %v",
			quiet, tally.rounds, tally.probeHeld, tally.renameRefused, tally.agree, tally.longest.Round(time.Millisecond))
	}
}
