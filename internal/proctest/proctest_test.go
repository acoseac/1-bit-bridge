//go:build !windows

package proctest

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestARunningProcessHasNotExited is the control every other answer rests
// on: a process that is plainly still there must never read as exited, on
// Linux, where /proc is asked, as much as elsewhere.
//
// Asked over half a second, as the callers ask: their loops stop at the
// first "exited", so one wrong answer at any poll passes a live process. A
// single ask straight after Start catches the child still being exec'd, in
// state R, and passed a /proc reading that took a sleeping process (S) for
// an exited one. So on Linux the last answer must also rest on /proc having
// seen `sleep` asleep.
func TestARunningProcessHasNotExited(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	var why string
	for i := 0; i < 50; i++ {
		var exited bool
		if exited, why = Exited(pid); exited {
			t.Fatalf("Exited(%d) = true (%s) for a process that is still running", pid, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.GOOS == "linux" && !strings.Contains(why, "state S") {
		t.Errorf("Exited(%d) = false, %q after 500 ms; on Linux the answer should rest on /proc "+
			"reading `sleep` asleep (state S)", pid, why)
	}
}

// TestAReapedProcessHasExited: once its parent has collected it, a process
// is gone, and kill(pid, 0) answers ESRCH on every unix.
func TestAReapedProcessHasExited(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if exited, why := Exited(cmd.Process.Pid); !exited {
		t.Fatalf("Exited(%d) = false (%s) for a process this test has reaped", cmd.Process.Pid, why)
	}
}

// TestExitedAsksNothingAboutAValueThatIsNotAPID: kill(0, 0) and kill(-1, 0)
// ask about process groups, and a value past pid_t truncates into one, so
// none of them may reach kill as though it named the caller's process. The
// two wide values are built from int64 so the file compiles where int is 32
// bits; there they wrap to values that are refused anyway.
func TestExitedAsksNothingAboutAValueThatIsNotAPID(t *testing.T) {
	pastPIDT, wrapsToZero := int64(math.MaxInt32)+1, int64(math.MaxUint32)+1
	for _, pid := range []int{0, -1, int(pastPIDT), int(wrapsToZero)} {
		exited, why := Exited(pid)
		if exited || !strings.Contains(why, "not a process id") {
			t.Errorf("Exited(%d) = %v, %q; want false and a refusal", pid, exited, why)
		}
	}
}

// TestZombieReadsEveryTask drives the /proc reading over a planted tree, so
// the shapes a real process cannot be made to hold on demand (a leader
// thread that exited while another runs, a name that spells a state, a
// /proc that answers nothing) are pinned on every platform.
func TestZombieReadsEveryTask(t *testing.T) {
	const pid = 158
	for _, c := range []struct {
		name string
		// task id -> its stat, where "" plants the task with no stat; nil
		// plants no task directory at all.
		tasks map[string]string
		want  bool
	}{
		{"a zombie: its leader is its only task, in state Z",
			map[string]string{"158": statLine(158, "tailscale-app", "Z")}, true},
		{"a zombie beside a thread still being released",
			map[string]string{"158": statLine(158, "tailscale-app", "Z"), "160": statLine(160, "tailscale-app", "X")}, true},
		{"the x that Linux 2.6.33 to 3.13 wrote for X",
			map[string]string{"158": statLine(158, "tailscale-app", "Z"), "160": statLine(160, "tailscale-app", "x")}, true},
		{"a sleeping process",
			map[string]string{"158": statLine(158, "tailscale-app", "S")}, false},
		{"a running process",
			map[string]string{"158": statLine(158, "tailscale-app", "R")}, false},
		{"a stopped process, which a SIGCONT would let write",
			map[string]string{"158": statLine(158, "tailscale-app", "T")}, false},
		{"a leader that exited while another thread runs",
			map[string]string{"158": statLine(158, "worker", "Z"), "159": statLine(159, "worker", "S")}, false},
		{"a live name that spells a zombie: the state follows the LAST paren",
			map[string]string{"158": statLine(158, "a) Z (b", "S")}, false},
		{"a zombie whose name spells a live state",
			map[string]string{"158": statLine(158, "a) S (b", "Z")}, true},
		{"no task directory: /proc not mounted, or the process hidden", nil, false},
		{"an empty task directory", map[string]string{}, false},
		// Listed, but gone by the time its stat is read: a thread released
		// mid-walk, or the whole process reaped. Not a zombie YET.
		{"a task whose stat has gone", map[string]string{"158": ""}, false},
		{"a stat with no state after the name",
			map[string]string{"158": "158 (tailscale-app)\n"}, false},
		{"a stat with no name at all",
			map[string]string{"158": "158 Z\n"}, false},
		{"a state that is not one letter",
			map[string]string{"158": "158 (tailscale-app) ZZ 1\n"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := plantProc(t, strconv.Itoa(os.Getpid()), pid, c.tasks)
			if got, read := zombieUnder(proc, pid); got != c.want {
				t.Errorf("zombieUnder = %v (%s), want %v", got, read, c.want)
			}
		})
	}
}

// TestZombieIgnoresAProcOfAnotherPIDNamespace: a /proc mounted for another
// pid namespace numbers every process differently, so its <pid> is some
// unrelated process, and even a zombie there says nothing about ours. The
// same planted zombie under this process's /proc is the control.
func TestZombieIgnoresAProcOfAnotherPIDNamespace(t *testing.T) {
	const pid = 158
	zombie := map[string]string{"158": statLine(158, "tailscale-app", "Z")}
	for _, c := range []struct {
		name, self string
		want       bool
	}{
		// Measured: under `unshare --pid --fork` without --mount-proc, a
		// process whose own pid is 1 reads /proc/self as 480456.
		{"its self names another process", "480456", false},
		{"it has no self", "", false},
		{"the control: its self is this process", strconv.Itoa(os.Getpid()), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := plantProc(t, c.self, pid, zombie)
			if got, read := zombieUnder(proc, pid); got != c.want {
				t.Errorf("zombieUnder = %v (%s), want %v", got, read, c.want)
			}
		})
	}
}

// statLine returns a /proc stat line for task tid, named name, in state.
func statLine(tid int, name, state string) string {
	return fmt.Sprintf("%d (%s) %s 1 %d 1 0 -1 4228364 66 92 0 0 0 0 0 0 20 0 1 0 21464915 0 0\n",
		tid, name, state, tid)
}

// plantProc returns a planted /proc: a self link naming self (none when self
// is ""), and under pid the tasks given, each holding its stat. A task whose
// stat is "" is planted with none, and nil tasks plants no task directory.
func plantProc(t *testing.T, self string, pid int, tasks map[string]string) string {
	t.Helper()
	proc := t.TempDir()
	if self != "" {
		if err := os.Symlink(self, filepath.Join(proc, "self")); err != nil {
			t.Fatal(err)
		}
	}
	if tasks == nil {
		return proc
	}
	dir := filepath.Join(proc, strconv.Itoa(pid), "task")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for tid, stat := range tasks {
		if err := os.MkdirAll(filepath.Join(dir, tid), 0o755); err != nil {
			t.Fatal(err)
		}
		if stat == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, tid, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return proc
}
