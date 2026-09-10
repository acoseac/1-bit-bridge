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
}
