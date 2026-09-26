//go:build linux

package doctor

import (
	"os"
	"testing"
)

// TestPIDListensOnPortReadsProc drives the /proc attribution against the
// real kernel tables: this process's own listener is attributed to it, and
// not to its parent (alive, and holding no port of ours), nor to a port
// nothing listens on.
func TestPIDListensOnPortReadsProc(t *testing.T) {
	port := bindPort(t)
	found, err := pidListensOnPort(port, os.Getpid())
	if err != nil || !found {
		t.Fatalf("pidListensOnPort(%d, self) = %v, %v; want true — this process holds the listener", port, found, err)
	}
	if found, err := pidListensOnPort(port, os.Getppid()); err != nil || found {
		t.Errorf("pidListensOnPort(%d, parent) = %v, %v; want false — the parent holds no port of ours", port, found, err)
	}
	if found, err := pidListensOnPort(mustFreePort(t), os.Getpid()); err != nil || found {
		t.Errorf("a port nothing listens on = %v, %v; want false", found, err)
	}
}
