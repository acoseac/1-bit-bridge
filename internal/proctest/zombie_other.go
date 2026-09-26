//go:build !linux && !windows

package proctest

// zombie is the non-Linux stub: it never finds a zombie, so Exited keeps
// kill(pid, 0)'s answer. There is no /proc to ask on macOS, and no need:
// launchd is init there and reaps the orphans the callers kill, which is
// why those tests passed on macOS with kill(pid, 0) alone.
func zombie(int) (bool, string) { return false, "" }
