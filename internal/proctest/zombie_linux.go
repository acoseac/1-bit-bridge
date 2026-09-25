//go:build linux

package proctest

// zombie reports whether process pid is a zombie, from what /proc lists
// under it, and says what it read.
func zombie(pid int) (bool, string) { return zombieUnder("/proc", pid) }
