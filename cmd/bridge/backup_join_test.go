package main

import (
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// awaitVerifyEndOnCleanup hears the end of the worker of the verification of
// dir that begins from now on, dir as its caller passes it
// (backup.SetOnVerifyEndForTest, whose hook a worker takes as its
// verification begins and runs last, once it has let go of everything it
// held), and registers the test's wait for it: registered now, it runs
// before every cleanup registered before it, so the test puts no seam back,
// removes no fixture and ends only once that worker has ended. A
// verification answers its caller before its worker lets go
// (backup.verifyAndThen), so a command that verifies returns while its
// worker still reads the backup package's seams (releaseBound, the lock's
// release hook): backlog B871's round 3, whose CI race was the next test's
// SetReleaseBoundForTest beside the release of a verification bridge backup
// --verify had answered. The verification must begin: a wait for one refused
// before its worker starts runs out, failing the test.
func awaitVerifyEndOnCleanup(t *testing.T, dir string) {
	t.Helper()
	ended := make(chan struct{})
	heard := sync.OnceFunc(func() { close(ended) })
	t.Cleanup(backup.SetOnVerifyEndForTest(func(d string) {
		if d == dir {
			heard()
		}
	}))
	t.Cleanup(func() {
		if onVerifyEndAwait != nil {
			onVerifyEndAwait(dir)
		}
		select {
		case <-ended:
		case <-time.After(30 * time.Second):
			t.Errorf("the worker of the verification of %s never ended", dir)
		}
	})
}

// onVerifyEndAwait, nil but in TestEachHoldWordsCaseWaitsForItsVerificationsEnd,
// runs as a test's cleanup begins to wait for a verification's end
// (awaitVerifyEndOnCleanup), given the snapshot as that verification was
// given it: the test lets a worker it parked go on there, and only there.
var onVerifyEndAwait func(dir string)
