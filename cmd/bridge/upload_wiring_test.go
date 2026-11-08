package main

import (
	"os"
	"strings"
	"testing"
)

// TestUploadReclaimableIsWiredInProduction exists because the behavioural test
// for `reclaimableBytes` cannot see this defect.
//
// That test constructs an upload.Manager and passes WithReclaimable itself —
// which is exactly how the option came to have NO production caller while two
// tests asserted the field worked. A fixture that wires a dependency describes
// a different bridge than production, and the only thing that can notice is a
// check on the wiring.
//
// Source scan rather than behaviour, because the question is about the call
// site: does runServe hand the upload manager a way to answer "how much would
// emptying the trash free?". Comments are stripped first — this package's
// commentary names what it discusses — and the anchor is the identifier, which
// no comment here spells.
func TestUploadReclaimableIsWiredInProduction(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	// CRLF-normalised at the read: nothing pins eol, so a Windows checkout
	// would otherwise make this scan find nothing and pass vacuously.
	src := stripGoComments(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if !strings.Contains(src, "upload.WithReclaimable(") {
		t.Error("cmd/bridge does not pass upload.WithReclaimable — every 507 will answer " +
			"reclaimableBytes: 0, which is the one value the field exists to be non-zero for")
	}
	if !strings.Contains(src, "trash.WithDestinationLock(uploadMgr.LockDestination)") {
		t.Error("cmd/bridge does not bind the upload destination lock onto trash — " +
			"a restore and a commit of one path can both pass the existence check and rename")
	}
}

// TestTheConsolesLibraryWritersAreGuardedInProduction: the upload and trash
// managers refuse a write into a library root that reads as a volume not
// mounted only where serve hands them the scanner's guard (backlog B776). The
// console's tests wire the guard themselves, as serve does, so only a check on
// the call site sees serve leave it out: TestUploadReclaimableIsWiredInProduction's
// reason, by its method.
func TestTheConsolesLibraryWritersAreGuardedInProduction(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := stripGoComments(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if !strings.Contains(src, "writeGuard := scanner.WriteGuard(scanCtx)") {
		t.Fatal("runServe builds no root guard from its scanner (scanner.WriteGuard), on the context its writers' looks run on")
	}
	for _, want := range []string{"upload.WithRootGuard(writeGuard)", "trash.WithRootGuard(writeGuard)"} {
		if !strings.Contains(src, want) {
			t.Errorf("runServe does not pass %s: that writer writes into a root whose volume is away", want)
		}
	}
}
