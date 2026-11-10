//go:build linux

package backup_test

import "golang.org/x/sys/unix"

// threadPriority is the calling thread's nice value: Linux's getpriority
// answers 20 less the nice value, which keeps its answer positive.
func threadPriority() int {
	raw, err := unix.Getpriority(unix.PRIO_PROCESS, unix.Gettid())
	if err != nil {
		return -100
	}
	return 20 - raw
}
