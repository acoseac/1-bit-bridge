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
// none of them may reach kill as though it named the caller's process.
func TestExitedAsksNothingAboutAValueThatIsNotAPID(t *testing.T) {
	for _, pid := range []int{0, -1, math.MaxInt32 + 1, math.MaxUint32 + 1} {
		exited, why := Exited(pid)
		if exited || !strings.Contains(why, "not a process id") {
			t.Errorf("Exited(%d) = %v, %q; want false and a refusal", pid, exited, why)
		}
	}
}

// TestZombieReadsEveryTask drives the /proc reading over a planted tree, so
// the shapes a real process cannot be made to hold on demand (a leader
// thread that exited while another runs, a name that spells a state, a
// /proc that answers nothing or numbers another pid namespace) are pinned
// on every platform.
func TestZombieReadsEveryTask(t *testing.T) {
	stat := func(tid int, name, state string) string {
		return fmt.Sprintf("%d (%s) %s 1 %d 1 0 -1 4228364 66 92 0 0 0 0 0 0 20 0 1 0 21464915 0 0\n",
			tid, name, state, tid)
	}
	const pid = 158
	for _, c := range []struct {
		name  string
		tasks map[string]string // task id -> its stat; nil plants no task directory
		want  bool
	}{
		{"a zombie: its leader is its only task, in state Z",
			map[string]string{"158": stat(158, "tailscale-app", "Z")}, true},
		{"a zombie beside a thread still being released",
			map[string]string{"158": stat(158, "tailscale-app", "Z"), "160": stat(160, "tailscale-app", "X")}, true},
		{"the x that Linux 2.6.33 to 3.13 wrote for X",
			map[string]string{"158": stat(158, "tailscale-app", "Z"), "160": stat(160, "tailscale-app", "x")}, true},
		{"a sleeping process",
			map[string]string{"158": stat(158, "tailscale-app", "S")}, false},
		{"a running process",
			map[string]string{"158": stat(158, "tailscale-app", "R")}, false},
		{"a stopped process, which a SIGCONT would let write",
			map[string]string{"158": stat(158, "tailscale-app", "T")}, false},
		{"a leader that exited while another thread runs",
			map[string]string{"158": stat(158, "worker", "Z"), "159": stat(159, "worker", "S")}, false},
		{"a live name that spells a zombie: the state follows the LAST paren",
			map[string]string{"158": stat(158, "a) Z (b", "S")}, false},
		{"a zombie whose name spells a live state",
			map[string]string{"158": stat(158, "a) S (b", "Z")}, true},
		{"no task directory: /proc not mounted, or the process hidden", nil, false},
		{"an empty task directory", map[string]string{}, false},
		{"a stat with no state after the name",
			map[string]string{"158": "158 (tailscale-app)\n"}, false},
		{"a stat with no name at all",
			map[string]string{"158": "158 Z\n"}, false},
		{"a state that is not one letter",
			map[string]string{"158": "158 (tailscale-app) ZZ 1\n"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := plantSelf(t, strconv.Itoa(os.Getpid()))
			if c.tasks != nil {
				taskDir := filepath.Join(proc, fmt.Sprint(pid), "task")
				if err := os.MkdirAll(taskDir, 0o755); err != nil {
					t.Fatal(err)
				}
				for tid, body := range c.tasks {
					if err := os.MkdirAll(filepath.Join(taskDir, tid), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(taskDir, tid, "stat"), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got, read := zombieUnder(proc, pid); got != c.want {
				t.Errorf("zombieUnder = %v (%s), want %v", got, read, c.want)
			}
		})
	}

	// A task listed but gone by the time its stat is read: a thread
	// released mid-walk, or the whole process reaped. Not a zombie YET.
	t.Run("a task whose stat has gone", func(t *testing.T) {
		proc := plantSelf(t, strconv.Itoa(os.Getpid()))
		if err := os.MkdirAll(filepath.Join(proc, fmt.Sprint(pid), "task", "158"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, read := zombieUnder(proc, pid); got {
			t.Errorf("zombieUnder = true (%s) for a task whose stat could not be read", read)
		}
	})

	// A /proc that does not number this process as the caller does: its
	// <pid> is some other process, so even a zombie there says nothing.
	zombieTask := func(proc string) {
		t.Helper()
		dir := filepath.Join(proc, fmt.Sprint(pid), "task", "158")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat(158, "tailscale-app", "Z")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a /proc mounted for another pid namespace: its self is not this process", func(t *testing.T) {
		proc := plantSelf(t, "480456")
		zombieTask(proc)
		if got, read := zombieUnder(proc, pid); got {
			t.Errorf("zombieUnder = true (%s) from a /proc whose self is not this process", read)
		}
	})
	t.Run("a /proc with no self", func(t *testing.T) {
		proc := t.TempDir()
		zombieTask(proc)
		if got, read := zombieUnder(proc, pid); got {
			t.Errorf("zombieUnder = true (%s) from a /proc with no self to check", read)
		}
	})
}

// plantSelf returns a planted /proc whose self link names target, as the
// kernel's names the reading process.
func plantSelf(t *testing.T, target string) string {
	t.Helper()
	proc := t.TempDir()
	if err := os.Symlink(target, filepath.Join(proc, "self")); err != nil {
		t.Fatal(err)
	}
	return proc
}
