//go:build linux

package hotplug

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestErrnoValuesAreLinuxs: the errno values the source's logic is written
// against are the kernel's.
func TestErrnoValuesAreLinuxs(t *testing.T) {
	for _, c := range []struct {
		ours errno
		unix unix.Errno
	}{{errEINTR, unix.EINTR}, {errEAGAIN, unix.EAGAIN}, {errENOBUFS, unix.ENOBUFS}} {
		if uintptr(c.ours) != uintptr(c.unix) {
			t.Errorf("%v is %d here and %d in the kernel", c.unix, c.ours, c.unix)
		}
		if !errors.Is(errnoFrom(c.unix), c.ours) {
			t.Errorf("errnoFrom(%v) is not %v", c.unix, c.ours)
		}
	}
}

// refusedHere reports whether a polling source's reason is this host
// refusing the netlink socket outright (a container, a sandbox, seccomp),
// which the test skips on, rather than a defect.
func refusedHere(reason string) bool {
	for _, s := range []string{"operation not permitted", "permission denied", "address family not supported", "protocol not supported"} {
		if strings.Contains(reason, s) {
			return true
		}
	}
	return false
}

// TestTheKernelSocketOpensOrSkipsCleanly: on Linux the real uevent socket
// opens and joins the kernel's group, and reports no events until the
// kernel speaks, or the host refuses it and the source polls, saying why;
// either way the source stops at once when its context ends.
func TestTheKernelSocketOpensOrSkipsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := openKernelSource(ctx)
	st := src.Status()
	if st.Mode == ModePolling {
		if refusedHere(st.Reason) {
			t.Skipf("this host refuses the netlink socket: %s", st.Reason)
		}
		t.Fatalf("the socket did not open: %s", st)
	}
	if st.Mode != ModeNetlinkQuiet && st.Mode != ModeNetlink {
		t.Fatalf("the socket opened in mode %s", st)
	}
	cancel()
	select {
	case <-src.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the source did not stop within 5s of its cancel")
	}
}

// TestLinuxWaitEndsAtOnceWhenItsContextDoes: a wait on the real socket with
// no message coming blocks in poll(2) until its context ends, and ends
// then, through the eventfd (not a poll in time slices, and not the next
// message); the socket then closes.
func TestLinuxWaitEndsAtOnceWhenItsContextDoes(t *testing.T) {
	const deadline = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	sock, _, err := openLinuxSocket(ctx)
	if err != nil {
		if refusedHere(err.Error()) {
			t.Skipf("this host refuses the netlink socket: %v", err)
		}
		t.Fatal(err)
	}
	start := time.Now()
	buf := make([]byte, maxUeventBytes)
	for {
		err = sock.wait(ctx)
		if err != nil {
			break
		}
		// The kernel sent something meanwhile: read it and wait again.
		for {
			if _, err := sock.recv(buf); err != nil {
				break
			}
		}
	}
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the wait ended with %v", err)
	}
	if took < deadline/2 || took > deadline+time.Second {
		t.Errorf("the wait took %v for a context that ended after %v", took, deadline)
	}
	if err := sock.close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

var (
	captureDir = flag.String("capture-uevents", "", "TestCaptureKernelUevents: write each uevent the kernel sends into this directory")
	captureFor = flag.Duration("capture-for", time.Minute, "TestCaptureKernelUevents: how long to listen")
)

// TestCaptureKernelUevents is the capture behind testdata/uevent, run by
// hand on a Linux box with -capture-uevents DIR (and -capture-for, a
// minute by default) while a DAC is plugged in and out: it writes every
// message the kernel sends, as it came, one file each, named for its order,
// action and subsystem. Without the flag it is skipped. It opens nothing
// but the uevent socket.
func TestCaptureKernelUevents(t *testing.T) {
	if *captureDir == "" {
		t.Skip("run with -capture-uevents DIR to capture the kernel's uevents")
	}
	if err := os.MkdirAll(*captureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *captureFor)
	defer cancel()
	sock, passcred, err := openLinuxSocket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	src := startKernelSource(ctx, sock, passcred, func(msg []byte) {
		mu.Lock()
		defer mu.Unlock()
		n++
		name := fmt.Sprintf("%04d", n)
		if u, err := ParseUevent(msg); err == nil {
			name += "-" + u.Action + "-" + u.Subsystem
		}
		if err := os.WriteFile(filepath.Join(*captureDir, name+".bin"), msg, 0o644); err != nil {
			t.Errorf("write %s: %v", name, err)
		}
	})
	t.Logf("listening for %v; plug and unplug the DAC now", *captureFor)
	<-src.Done()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d messages written to %s; %+v", n, *captureDir, src.Status().Counts)
}
