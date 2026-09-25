//go:build !windows

// Package proctest answers one question a test asks about a process it
// started but cannot reap, such as a grandchild: has it exited? Like
// net/http/httptest it is imported only by tests, so none of it reaches the
// binary.
//
// kill(pid, 0) alone cannot answer it. It succeeds for a ZOMBIE, a process
// that has exited and whose exit status nobody has collected, because the
// kernel keeps the process's entry until its parent reaps it. A process
// whose parent has died is handed to the pid namespace's init, and reaping
// those is init's job: systemd, launchd and the tini that `docker run
// --init` adds each do it within milliseconds, which is why a kill(pid, 0)
// poll passed on every host CI has. Without --init, PID 1 in a container is
// the container's own command, `go test` in the stock golang image, and
// `go` collects only the children it started. A killed grandchild then
// stays a zombie until the container exits, and kill(pid, 0) calls it
// running. Measured on Ubuntu 26.04 with golang:1.26.6: the CLI stand-in
// that TestServeLeavesNoTailscaleCLIRunning and
// TestCancelStopsTheWholeCLIProcessTree kill read `State: Z (zombie)` and
// `PPid: 1`, with `go test` as PID 1, and both tests failed without --init
// and passed with it.
//
// A zombie runs no code and holds no files, so it writes nothing, which is
// what those tests guard. On Linux, Exited asks /proc about a process that
// kill(pid, 0) still finds. Elsewhere it keeps kill(pid, 0)'s answer: macOS
// has launchd as init, and CI runs no other unix.
package proctest

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Exited reports whether process pid has exited, and what the answer rests
// on, for a failure message.
//
// A pid that kill(pid, 0) cannot find (ESRCH) has exited and been reaped.
// One it finds has exited only if it is a zombie, which /proc tells apart on
// Linux. Every other answer is "running", including the ones that mean
// "could not tell": the callers poll until a deadline, so an answer that
// errs toward running costs one more poll and never passes a process that
// is still there.
func Exited(pid int) (bool, string) {
	// kill(0, 0) asks about the caller's whole process group and
	// kill(-1, 0) about every process it may signal, and a pid past pid_t
	// truncates into one of those or into another pid. Each would answer
	// for something other than the process the caller means.
	if pid <= 0 || pid > math.MaxInt32 {
		return false, fmt.Sprintf("%d is not a process id", pid)
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, "kill(pid, 0) = ESRCH"
	}
	exited, read := zombie(pid)
	switch {
	case exited:
		return true, fmt.Sprintf("kill(pid, 0) = %v, but %s", err, read)
	case read == "":
		return false, fmt.Sprintf("kill(pid, 0) = %v", err)
	default:
		return false, fmt.Sprintf("kill(pid, 0) = %v; %s", err, read)
	}
}

// zombieUnder reports whether every task (thread) that proc lists for
// process pid has exited, and says what it read. proc is /proc except in a
// test.
//
// Every task, not the process's own stat: a process whose leader thread
// exits while its other threads run shows the leader's Z in
// /proc/<pid>/stat too, and is running. A zombie is what is left once all
// of them have exited. The kernel releases each other thread as it exits,
// which leaves the leader alone, in state Z, until the parent reaps it.
//
// Whatever it cannot read or parse answers "not a zombie", so Exited keeps
// kill(pid, 0)'s "running". A /proc that is not mounted, or that hides the
// process, can then delay an "exited" but never invent one. So can a /proc
// mounted for another pid namespace, which numbers every process
// differently, so that its <pid> is some unrelated process: its self is not
// this process. Measured on Linux 7.0: under `unshare --pid --fork` without
// --mount-proc, a process whose own pid is 1 reads /proc/self as 480456.
func zombieUnder(proc string, pid int) (bool, string) {
	self, err := os.Readlink(filepath.Join(proc, "self"))
	if err != nil {
		return false, err.Error()
	}
	if self != strconv.Itoa(os.Getpid()) {
		return false, fmt.Sprintf("%s/self is %s, not this process (%d): another pid namespace's /proc",
			proc, self, os.Getpid())
	}
	dir := filepath.Join(proc, strconv.Itoa(pid), "task")
	tasks, err := os.ReadDir(dir)
	if err != nil {
		return false, err.Error()
	}
	if len(tasks) == 0 {
		return false, dir + " lists no tasks"
	}
	read := make([]string, 0, len(tasks))
	for _, task := range tasks {
		path := filepath.Join(dir, task.Name(), "stat")
		b, err := os.ReadFile(path)
		if err != nil {
			return false, err.Error()
		}
		state, ok := statState(b)
		if !ok {
			return false, fmt.Sprintf("%s has no state field: %q", path, b)
		}
		if !exitedState(state) {
			return false, fmt.Sprintf("%s: state %s", path, state)
		}
		read = append(read, task.Name()+" "+state)
	}
	return true, fmt.Sprintf("every task in %s has exited (%s)", dir, strings.Join(read, ", "))
}

// statState returns the state field of a /proc stat file, the field after
// the command name. The name is in parentheses and may itself hold spaces
// and parentheses (it is the executable's name, up to 15 bytes), so it ends
// at the LAST ')'.
func statState(stat []byte) (string, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) == 0 || len(fields[0]) != 1 {
		return "", false
	}
	return fields[0], true
}

// exitedState reports whether a /proc state letter is one a task has only
// after it exits: Z (zombie, not yet reaped), X (dead, being released), and
// x, which Linux 2.6.33 to 3.13 used for X.
func exitedState(state string) bool {
	return state == "Z" || state == "X" || state == "x"
}
