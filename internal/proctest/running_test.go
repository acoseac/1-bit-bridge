package proctest

import (
	"os/exec"
	"testing"
	"time"
)

// startUntilKilled starts cmd, which must run until it is killed, and has
// the test kill and reap it when it ends. It returns the child's pid. The
// Unix and Windows running cases share it, with the child each platform
// can rely on.
func startUntilKilled(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// assertStaysRunning asks Exited about a live pid fifty times over half a
// second, as the callers ask: their loops stop at the first "exited", so
// one wrong answer at any poll passes a live process. It returns the last
// answer's reason, so the caller can check what the answer rests on.
func assertStaysRunning(t *testing.T, pid int) string {
	t.Helper()
	var why string
	for i := 0; i < 50; i++ {
		var exited bool
		if exited, why = Exited(pid); exited {
			t.Fatalf("Exited(%d) = true (%s) for a process that is still running", pid, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return why
}
