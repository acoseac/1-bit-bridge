//go:build linux

package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// leaseHeld is how long the test's lease outlives OpenAsFile's first open.
const leaseHeld = 300 * time.Millisecond

// TestOpenAsFileWaitsOutALeaseBreakAsAPlainOpenDoes: while another open
// holds a write lease on a file (Samba's kernel oplocks take one, an NFS
// server's delegation another), a nonblocking open of it fails with
// EWOULDBLOCK, where a plain open waits for the lease to be broken. OpenAsFile
// answers that failure with the plain open, so a file under a lease is
// served after the break, as it always was, and not refused with a 500.
//
// The lease is this process's own: a lease is held by an open file, and an
// open from the same process breaks it like any other. Taking one needs the
// file to be the caller's and leases enabled (fs.leases-enable); a host that
// refuses skips.
func TestOpenAsFileWaitsOutALeaseBreakAsAPlainOpenDoes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "01 Track.flac")
	if err := os.WriteFile(p, []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	rc, err := holder.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	setLease := func(kind int) (setErr error) {
		if err := rc.Control(func(fd uintptr) { _, setErr = unix.FcntlInt(fd, unix.F_SETLEASE, kind) }); err != nil {
			return err
		}
		return setErr
	}
	if err := setLease(unix.F_WRLCK); err != nil {
		t.Skipf("this host refuses a write lease here: %v", err)
	}
	type release struct {
		at  time.Time
		err error
	}
	released := make(chan release, 1)
	go func() {
		time.Sleep(leaseHeld)
		// Releasing the lease completes the break the open started.
		at := time.Now()
		released <- release{at, setLease(unix.F_UNLCK)}
	}()

	f, _, err := fsutil.OpenAsFile(p)
	opened := time.Now()
	// Wait for the release before anything can end the test, so the
	// goroutine never outlives it.
	rel := <-released
	if rel.err != nil {
		t.Errorf("releasing the lease: %v", rel.err)
	}
	if err != nil {
		t.Fatalf("OpenAsFile under a lease: %v; want it opened once the lease is broken", err)
	}
	_ = f.Close()
	if opened.Before(rel.at) {
		t.Errorf("opened %v before the lease was released; want it to wait for the break", rel.at.Sub(opened))
	}
}
