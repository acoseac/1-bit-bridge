package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
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
// No test here opens /dev/snd or reads the box's own /proc or /sys: the
// pcm-test harness's tests drive a fake device, the output and DAC binding
// commands read trees the tests lay out, and hostTrees, which the outputs
// builders of serve and bridge doctor read the cards through and the
// commands fall back to, is two empty directories here, set before any
// test runs and never again. The guard refuses any open that reaches
// alsapcm.Open, and the watch any read of the host's trees through the
// alsa readers, and the binary fails on either, so a test that would play
// on, or read, the box it runs on (the owner's Pi has a DAC) fails instead.
//
// Nor does a test bind the host's LAN addresses: hostLAN, the interfaces
// serve's OpenHome surface serves, is a box with none here, so a serve test
// that turns the surface on without its own box (playerOpenHome) binds
// nothing.
func TestMain(m *testing.M) {
	// The fingerprint stand-in re-execs this binary as fpcalc. It must
	// return before the suite starts, or the child runs every test.
	if os.Getenv(fpcalcStandInEnv) != "" {
		os.Exit(runFpcalcStandIn())
	}
	adminauth.SetTestHashCost(bcrypt.MinCost)
	trees, treesDir, err := emptyHostTrees()
	if err != nil {
		fmt.Fprintln(os.Stderr, "the empty trees the suite reads for the host's:", err)
		os.Exit(1)
	}
	hostTrees = trees
	hostLAN = func() ([]lan.Interface, error) { return nil, nil }
	stopOpens := alsapcm.GuardNodeOpens(recordNodeOpen)
	stopControls := alsactl.GuardNodeOpens(recordNodeOpen)
	stopReads := alsa.WatchHostReads(recordHostRead)
	code := m.Run()
	stopControls()
	stopReads()
	stopOpens()
	_ = os.RemoveAll(treesDir)
	if opened := nodeOpens(); len(opened) > 0 {
		fmt.Fprintf(os.Stderr, "the suite opened PCM nodes, which no test may: %q\n", opened)
		code = 1
	}
	if read := hostReads(); len(read) > 0 {
		fmt.Fprintf(os.Stderr, "the suite read the host's own trees, which no test may (%d reads); each read's root and where it was asked:\n", len(read))
		for _, r := range read {
			fmt.Fprintln(os.Stderr, r)
		}
		code = 1
	}
	os.Exit(code)
}

// emptyHostTrees makes the two empty directories the suite reads in place
// of the host's /proc and /sys, in a directory of their own, dir, which
// TestMain removes by the name MkdirTemp gave it.
func emptyHostTrees() (trees outputTrees, dir string, err error) {
	dir, err = os.MkdirTemp("", "bridge-host-trees-")
	if err != nil {
		return outputTrees{}, "", err
	}
	trees = outputTrees{proc: filepath.Join(dir, "proc"), sysfs: filepath.Join(dir, "sys")}
	for _, d := range []string{trees.proc, trees.sysfs} {
		if err := os.Mkdir(d, 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return outputTrees{}, "", err
		}
	}
	return trees, dir, nil
}

// openedNodes are the nodes the guard refused, for TestMain and the tests
// that assert a command opened none; readRoots are the host trees the
// watch refused, each with the stack that asked for it, since a read names
// no test.
var (
	openedNodesMu sync.Mutex
	openedNodes   []string
	readRootsMu   sync.Mutex
	readRoots     []string
)

// recordHostRead is the watch's report: it refuses the read.
func recordHostRead(root string) error {
	readRootsMu.Lock()
	defer readRootsMu.Unlock()
	readRoots = append(readRoots, root+"\n"+string(debug.Stack()))
	return errors.New("this test binary reads no host tree: " + root)
}

// hostReads returns the host trees the watch refused so far.
func hostReads() []string {
	readRootsMu.Lock()
	defer readRootsMu.Unlock()
	return append([]string(nil), readRoots...)
}

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
