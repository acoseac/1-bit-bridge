//go:build windows

package proctest

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// exitedHelperSleepEnv is set on the re-exec of this test binary that must
// stay alive until the parent kills it. Unset, TestHelperSleep returns at
// once, so a normal run of the package does not wait.
const exitedHelperSleepEnv = "PROCTEST_EXITED_HELPER_SLEEP"

// TestHelperSleep is the child the Exited cases re-exec, via
// -test.run=^TestHelperSleep$. With PROCTEST_EXITED_HELPER_SLEEP set it
// sleeps until the parent kills it. Otherwise it returns at once, which is
// a normal run of this package and the reaped-process case.
func TestHelperSleep(t *testing.T) {
	if os.Getenv(exitedHelperSleepEnv) == "" {
		return
	}
	time.Sleep(time.Hour)
}

// TestARunningProcessHasNotExited polls a live child for 500 ms, as the
// callers poll. One true answer at any poll would pass a process that is
// still there. The last reason names WAIT_TIMEOUT, so the check rests on
// the wait.
func TestARunningProcessHasNotExited(t *testing.T) {
	cmd, _ := helperSleep(t, true)
	pid := startUntilKilled(t, cmd)
	why := assertStaysRunning(t, pid)
	if !strings.Contains(why, "WAIT_TIMEOUT") {
		t.Errorf("Exited(%d) = false, %q after 500 ms; the answer should rest on WAIT_TIMEOUT", pid, why)
	}
}

// TestAReapedProcessHasExited: once Wait has collected it, Exited answers
// true. Go's Wait closes its own handle, so the process object may still
// be there (WAIT_OBJECT_0) or already gone (ERROR_INVALID_PARAMETER).
func TestAReapedProcessHasExited(t *testing.T) {
	cmd, out := helperSleep(t, false)
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.Bytes())
	}
	exited, why := Exited(cmd.Process.Pid)
	if !exited {
		t.Fatalf("Exited(%d) = false (%s) for a process this test has reaped", cmd.Process.Pid, why)
	}
	if !strings.Contains(why, "WAIT_OBJECT_0") && !strings.Contains(why, "ERROR_INVALID_PARAMETER") {
		t.Fatalf("Exited(%d) = true, %q; want WAIT_OBJECT_0 or ERROR_INVALID_PARAMETER", cmd.Process.Pid, why)
	}
}

// TestExitedAsksNothingAboutAValueThatIsNotAPID: a Windows pid is a DWORD.
// Zero, a negative value, and a value past MaxUint32 are refused before
// any call, so the cast cannot truncate one into a pid that exists. The
// refusal text is that early return's, which is what shows no system call
// was made. On a 32-bit int the wide value wraps to a value that is
// refused anyway.
func TestExitedAsksNothingAboutAValueThatIsNotAPID(t *testing.T) {
	tooWide := int(uint64(math.MaxUint32) + 1)
	for _, pid := range []int{0, -1, tooWide} {
		exited, why := Exited(pid)
		if exited || !strings.Contains(why, "is not a process id") {
			t.Errorf("Exited(%d) = %v, %q; want false and a refusal", pid, exited, why)
		}
	}
}

// helperSleep re-execs this test binary as TestHelperSleep. sleep keeps
// the child alive until the caller kills it. out is what the child prints.
func helperSleep(t *testing.T, sleep bool) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Stdout, cmd.Stderr = out, out
	if sleep {
		cmd.Env = append(os.Environ(), exitedHelperSleepEnv+"=1")
	}
	return cmd, out
}
