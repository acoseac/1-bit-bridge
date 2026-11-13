package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// TestTheSweepsSkipTheDirectoryTheCensusSkips: the sweeps from the root that
// read this checkout's Go files (the citation guard's walks, the hash-cost
// and flac-handle guards) skip make test's TMPDIR by sweeptest.MakeTestTmpDir,
// a copy of the census's makeTestTmpDir, which neither side can share: the
// census is a program, and sweeptest is imported by tests alone. So a rename
// of either copy alone fails here (B567), as a rename of the census's or of
// the Makefile's TMPDIR alone fails TestTheCensusSkipsTheMakefilesTMPDIR.
func TestTheSweepsSkipTheDirectoryTheCensusSkips(t *testing.T) {
	if sweeptest.MakeTestTmpDir != makeTestTmpDir {
		t.Fatalf("the sweeps skip %s at the root as make test's TMPDIR, and the census skips %s", sweeptest.MakeTestTmpDir, makeTestTmpDir)
	}
}
