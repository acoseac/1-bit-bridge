package main

import "github.com/acoseac/1-bit-bridge/internal/alsa"

// outputTrees are a /proc tree and a sysfs tree, the two the bridge's ALSA
// readers read.
type outputTrees struct {
	proc, sysfs string
}

// cards is one reading of the sound cards in t's /proc tree.
func (t outputTrees) cards() (alsa.Snapshot, error) {
	return alsa.Reader{ProcRoot: t.proc}.Read()
}

// hostTrees are the trees this package reads where nothing names another:
// the box's own /proc and /sys. GET /v1/outputs, the console's player
// checklist and bridge doctor's read the cards through hostTrees.cards
// (serve's and the doctor's outputs builders), and bridge output dac,
// pcm-test, and dac inspect and bind read hostTrees on Linux when no flag
// names a tree. Nothing else here names the host's trees
// (TestOnlyHostTreesNamesTheHostsTrees), and the bridge never writes it.
//
// This package's TestMain points it at two empty directories before any
// test runs and never again, so no test reads the host's /proc or /sys, on
// Linux too: the localOutputs gate opens the builders' path there alone,
// so a test that reached it with the host's trees passed on macOS and read
// the host on Linux (B470 PR 4a round 5).
var hostTrees = outputTrees{proc: alsa.DefaultProcRoot, sysfs: alsa.DefaultSysfsRoot}
