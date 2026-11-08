//go:build !linux && !darwin && !windows

package backup

// lowerThreadPriority has no per-thread priority to lower on this platform:
// the verification's reads are paced all the same.
func lowerThreadPriority() {}
