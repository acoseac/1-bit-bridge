//go:build !windows

package atomicwrite

func retryableReadError(error) bool { return false }
