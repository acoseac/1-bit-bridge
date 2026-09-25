//go:build linux

package proctest

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAZombieHasExited makes a real zombie, a child that has exited and
// that this test has not reaped yet, which is the state `go test` as a
// container's PID 1 left the killed CLI in. kill(pid, 0) still finds it,
// the premise this package exists for, and Exited must call it exited, from
// /proc, and keep calling it that once it is reaped.
func TestAZombieHasExited(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Wait()
		}
	})

	// WNOWAIT waits for the child to exit and leaves it unreaped.
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			t.Fatalf("waitid(%d, WEXITED|WNOWAIT): %v", pid, err)
		}
	}

	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("kill(%d, 0) = %v for an exited child nobody has reaped; this package's premise is "+
			"that the kernel keeps a zombie findable, and a kernel that does not needs none of it", pid, err)
	}
	exited, why := Exited(pid)
	if !exited {
		t.Fatalf("Exited(%d) = false (%s) for a zombie", pid, why)
	}
	if !strings.Contains(why, "has exited ("+strconv.Itoa(pid)+" Z)") {
		t.Errorf("Exited(%d) = true, %q; want the answer to rest on /proc's Z", pid, why)
	}

	reaped = true
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if exited, why := Exited(pid); !exited || !strings.Contains(why, "ESRCH") {
		t.Errorf("Exited(%d) = %v, %q once reaped; want true on ESRCH", pid, exited, why)
	}
}
