//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// lsofLate is how late the case's lsof answers: past the doctor's own 2 s
// bound, which a loaded runner's lsof outlived.
const lsofLate = 2500 * time.Millisecond

// TestTheReinitsLsofProbeTakesTheTestsDeadline: bridge init's preflight over
// an install whose config does not load asks the real lsof whether the
// bridge recorded in the data dir holds the ports the run writes
// (checkChosenPort), and a probe cut short there fails the port, so the
// re-init exits 1, its own bridge's ports read as another process's
// (backlog B872 part 2; a loaded runner's lsof outlived the doctor's 2 s
// bound). Every cmd/bridge test that reaches the real lsof gives it the
// test's deadline (lsofTakesTheTestsDeadline, here through
// reinitBrokenInstall): an lsof that answers 2.5 s late, its first spawn
// held before it starts (doctor.SetLsofCommandForTest), still finds this
// process on the ports, and the re-init goes through. The control is the
// deadline dropped: the probe is cut short at 2 s and the re-init exits 1.
func TestTheReinitsLsofProbeTakesTheTestsDeadline(t *testing.T) {
	if doctor.LsofForTest() == "" {
		t.Skip("this host has no lsof: the doctor reads /proc in its place, and spawns nothing to answer late")
	}
	var spawns atomic.Int32
	t.Cleanup(doctor.SetLsofCommandForTest(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if spawns.Add(1) == 1 {
			// Held before it starts, or until its probe is cut short.
			select {
			case <-time.After(lsofLate):
			case <-ctx.Done():
			}
		}
		return exec.CommandContext(ctx, name, args...)
	}))
	cfgDir, lib := brokenInstall(t)
	recordBridgePID(t, cfgDir, os.Getpid())
	api, admin := holdLoopbackPortAsBridge(t), holdLoopbackPortAsBridge(t)

	code, out := reinitBrokenInstall(t, cfgDir, lib, publicReinitArgs(api, admin)...)
	if n := spawns.Load(); n < 1 {
		t.Fatalf("the re-init spawned no lsof, so none answered late:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("public init exited %d replacing a config that does not load, with its own bridge on the ports it writes, "+
			"its lsof answering %s late:\n%s", code, lsofLate, out)
	}
	assertConfigReplaced(t, cfgDir)
}
