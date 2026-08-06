//go:build !windows

package doctor

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestSignal0Alive pins the classification a signal-0 liveness probe puts
// on each errno, independent of who is running the test.
//
// This exists because the integration assertion in
// TestPIDAlive_SelfAndBounds — pidAlive(1) must be true — reaches the
// EPERM branch only when UNPRIVILEGED. As root, signalling init just
// succeeds, so on a root CI runner (the common container case) that
// assertion passes through the nil-error path and would keep passing if
// the EPERM branch were deleted. Both tests are kept: this one pins the
// rule, that one pins the wiring.
func TestSignal0Alive(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// The process exists and we were allowed to signal it.
			name: "nil means alive",
			err:  nil,
			want: true,
		},
		{
			// The process exists but belongs to someone we may not
			// signal. This is the case the whole fix rests on: a
			// capability-bound bridge is exactly a live process we can be
			// unable to interrogate, and reading it as dead makes doctor
			// report the bridge's own port as a conflict.
			name: "EPERM means alive",
			err:  syscall.EPERM,
			want: true,
		},
		{
			name: "wrapped EPERM still means alive",
			err:  fmt.Errorf("signal: %w", syscall.EPERM),
			want: true,
		},
		{
			name: "ESRCH means dead",
			err:  syscall.ESRCH,
			want: false,
		},
		{
			// Modern Go finds processes via pidfd and hands back a "done"
			// Process for an already-reaped pid, whose Signal reports this
			// rather than ESRCH.
			name: "os.ErrProcessDone means dead",
			err:  os.ErrProcessDone,
			want: false,
		},
		{
			name: "an unrelated error means dead",
			err:  errors.New("something else entirely"),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := signal0Alive(tc.err); got != tc.want {
				t.Errorf("signal0Alive(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPIDAliveReapedChildReadsDead pins the direction checkPort's Fail
// verdict rests on: a stale pidfile naming a process that is really gone
// must NOT read as live, or a genuine port conflict is softened to a Warn
// on the strength of a crashed bridge's leftovers.
//
// Unix-only, and not for portability plumbing — the assertion is only TRUE
// here. kill(pid,0) against a reaped pid answers ESRCH, a kernel contract.
// Windows has no equivalent: a process object outlives termination for as
// long as anything holds a handle to it, so OpenProcess keeps succeeding
// and pidAlive keeps saying alive — which doctor_windows.go documents as
// the deliberate fail-soft choice, and which made this exact assertion
// flake on windows-latest across four unrelated PRs. Recycling compounds
// it: Windows hands PIDs back out quickly, unix allocates them
// sequentially, so the reaped pid is not re-issued mid-test here.
func TestPIDAliveReapedChildReadsDead(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skipf("could not spawn a probe process on this host: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if pidAlive(pid) {
		t.Errorf("pidAlive(%d) = true for a reaped child; a stale pidfile must not read as live", pid)
	}
}

// TestPIDAliveRejectsOutOfRangePID pins the pid_t bound on unix, which is
// tighter than the Windows one (int32 vs DWORD) and guards a sharper
// failure.
//
// An out-of-range pid TRUNCATES into pid_t. math.MaxUint32+1 truncates to
// 0 — and kill(0, 0) does not ask about "pid 0", it signals EVERY process
// in the caller's process group. It succeeds, so an unbounded pidAlive
// reports a plainly bogus pid as ALIVE, which in checkPort means a real
// port conflict is softened to a Warn on the strength of a corrupt
// pidfile. Observed failing on darwin before the bound was added.
func TestPIDAliveRejectsOutOfRangePID(t *testing.T) {
	for _, pid := range []int{
		math.MaxInt32 + 1,  // first value past pid_t
		math.MaxUint32 + 1, // truncates to 0 -> kill(0,0) -> process group
		math.MaxUint32 + 2, // truncates to 1 -> init, which exists
	} {
		if pidAlive(pid) {
			t.Errorf("pidAlive(%d) = true; an out-of-range pid must not be truncated into a live one", pid)
		}
	}
}
