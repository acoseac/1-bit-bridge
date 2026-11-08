//go:build linux

package backup

import "golang.org/x/sys/unix"

// ioprio_set's words (linux/ioprio.h): a thread's own I/O priority, in the
// idle class, which the kernel's BFQ scheduler serves only while no other
// I/O waits; other schedulers take it as a hint or not at all.
const (
	ioprioWhoProcess = 1
	ioprioClassIdle  = 3
	ioprioClassShift = 13
)

// lowerThreadPriority lowers the CPU and I/O priority of the calling OS
// thread alone, which its caller has locked and never unlocks, so the
// thread ends with the goroutine (runLowPriority): Linux's setpriority and
// ioprio_set take a thread's ID for PRIO_PROCESS and IOPRIO_WHO_PROCESS.
// Best effort: a refusal leaves the thread as it was.
func lowerThreadPriority() {
	tid := unix.Gettid()
	_ = unix.Setpriority(unix.PRIO_PROCESS, tid, lowPriorityNice)
	_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassIdle<<ioprioClassShift)
}

// lowPriorityNice is the nice value the verification's thread takes: the
// lowest.
const lowPriorityNice = 19
