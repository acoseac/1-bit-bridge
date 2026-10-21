package main

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

// TestMain lowers the bcrypt work factor for this package's suite, guards
// every open of a PCM node, and returns first when this process was
// re-exec'd as the fpcalc stand-in.
//
// `bridge admin reset-password` and the public-mode startup paths hash real
// passwords, and bcrypt's deliberate ~250 ms is multiplied by roughly eight
// under the race detector. Set once, before any test runs.
//
// The shipped cost is unchanged: adminauth.TestShippedHashCostIsUnchanged pins
// it, and TestNoProductionCodeLowersTheHashCost sweeps every non-test file to
// make sure nothing outside a suite calls this.
//
// No test here opens /dev/snd: the pcm-test harness's tests drive a fake
// device, and the DAC binding commands open nothing. The guard refuses any
// open that reaches alsapcm.Open and fails the binary on it, so a test that
// would play on the box it runs on (the owner's Pi has a DAC) fails instead.
func TestMain(m *testing.M) {
	// The fingerprint stand-in re-execs this binary as fpcalc. It must
	// return before the suite starts, or the child runs every test.
	if os.Getenv(fpcalcStandInEnv) != "" {
		os.Exit(runFpcalcStandIn())
	}
	adminauth.SetTestHashCost(bcrypt.MinCost)
	stop := alsapcm.GuardNodeOpens(recordNodeOpen)
	code := m.Run()
	stop()
	if opened := nodeOpens(); len(opened) > 0 {
		fmt.Fprintf(os.Stderr, "the suite opened PCM nodes, which no test may: %q\n", opened)
		code = 1
	}
	os.Exit(code)
}

// openedNodes are the nodes the guard refused, for TestMain and the tests
// that assert a command opened none.
var (
	openedNodesMu sync.Mutex
	openedNodes   []string
)

// recordNodeOpen is the guard's report.
func recordNodeOpen(path string) {
	openedNodesMu.Lock()
	defer openedNodesMu.Unlock()
	openedNodes = append(openedNodes, path)
}

// nodeOpens returns the nodes the guard refused so far.
func nodeOpens() []string {
	openedNodesMu.Lock()
	defer openedNodesMu.Unlock()
	return append([]string(nil), openedNodes...)
}
