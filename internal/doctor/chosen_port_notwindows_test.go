//go:build !windows

package doctor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestChosenPortFailsWhenTheOwnerProbeFails: checkPort degrades an owner
// probe that failed (lsof erroring, or timing out on a wedged mount) to a
// warn, because a broken probe must never break a healthy install whose
// config names the port. Over an install whose config did not load nothing
// names it, so a probe that could not answer leaves a held port with
// nothing to excuse it, and the port is refused.
func TestChosenPortFailsWhenTheOwnerProbeFails(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh(1) on PATH to stand in for an lsof that fails: %v", err)
	}
	origPath, origCmd := lsofPath, lsofCommand
	t.Cleanup(func() { lsofPath, lsofCommand = origPath, origCmd })
	lsofPath = sh
	// Exit status 2: not lsof's "ran, matched nothing" (1), so the probe
	// reports a mechanism failure.
	lsofCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, sh, "-c", "exit 2")
	}
	withPIDAlive(t, true)
	port, pidFile := bindPort(t), writePIDFile(t, os.Getpid())

	if c := checkChosenPort(t.Context(), "port-test", port, pidFile); c.Status != Fail ||
		!strings.Contains(c.Hint, "owner probe failed") {
		t.Errorf("owner probe failed, ports unknown: got %v (%s / %s), want a fail naming the probe",
			c.Status, c.Summary, c.Hint)
	}
	// The control: the same facts through the ordinary ladder warn.
	if c := checkPort(t.Context(), "port-test", port, pidFile); c.Status != Warn {
		t.Errorf("owner probe failed, config read: got %v (%s), want warn", c.Status, c.Summary)
	}
}
