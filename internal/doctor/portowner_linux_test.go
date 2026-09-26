//go:build linux

package doctor

import (
	"fmt"
	"os"
	"testing"
)

// TestPIDListensOnPortReadsProc drives the /proc attribution against the
// real kernel tables: this process's own listener is attributed to it, and
// not to its parent (alive, and holding no port of ours), nor to a port
// nothing listens on. Each miss carries /proc's account of it, and both
// rule the pid out: the parent's descriptors are readable, and a port with
// no listener has nothing a bridge could hold.
func TestPIDListensOnPortReadsProc(t *testing.T) {
	port := bindPort(t)
	found, _, err := pidListensOnPort(port, os.Getpid())
	if err != nil || !found {
		t.Fatalf("pidListensOnPort(%d, self) = %v, %v; want true — this process holds the listener", port, found, err)
	}
	ppid := os.Getppid()
	found, seen, err := pidListensOnPort(port, ppid)
	if err != nil || found {
		t.Errorf("pidListensOnPort(%d, parent) = %v, %v; want false — the parent holds no port of ours", port, found, err)
	}
	if want := fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port", ppid); seen.saw != want || !seen.ruledOut {
		t.Errorf("parent's sighting = %+v, want %q, ruled out", seen, want)
	}
	found, seen, err = pidListensOnPort(mustFreePort(t), os.Getpid())
	if err != nil || found {
		t.Errorf("a port nothing listens on = %v, %v; want false", found, err)
	}
	if want := "/proc lists no socket listening on this port"; seen.saw != want || !seen.ruledOut {
		t.Errorf("free port's sighting = %+v, want %q, ruled out", seen, want)
	}
}
