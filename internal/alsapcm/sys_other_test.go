//go:build !(linux && (amd64 || arm64))

package alsapcm

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestOpenOffLinuxIsUnsupported pins the stub: off linux/amd64 and
// linux/arm64, Open refuses with ReasonUnsupported and names the platform.
func TestOpenOffLinuxIsUnsupported(t *testing.T) {
	d, err := Open(Endpoint{Card: 0, Device: 0}, Config{RateHz: 48000, Format: format.S16LE})
	if d != nil || ReasonOf(err) != ReasonUnsupported {
		t.Fatalf("Open = %v, %v; want ReasonUnsupported", d, err)
	}
	if !strings.Contains(err.Error(), "linux/amd64 and linux/arm64") {
		t.Errorf("%q does not say where the backend runs", err)
	}
	if Supported() {
		t.Error("Supported() is true on a build that cannot open")
	}
}

// TestProbeOffLinuxIsUnsupported pins the probe's stub (B728 B4), which B5
// and B7 call from packages every platform builds: off linux/amd64 and
// linux/arm64, Probe refuses with ReasonUnsupported, opens nothing, and
// answers every candidate not probed.
func TestProbeOffLinuxIsUnsupported(t *testing.T) {
	cands := []ProbeCandidate{{Format: format.S32LE, RateHz: 176400}, {Format: format.DSDU32BE, RateHz: 88200}}
	rep, err := Probe(Endpoint{Card: 0, Device: 0}, ProbeConfig{Candidates: cands})
	if ReasonOf(err) != ReasonUnsupported || rep.Opened || len(rep.Results) != len(cands) {
		t.Fatalf("Probe = %+v, %v; want ReasonUnsupported and nothing opened", rep, err)
	}
	for _, r := range rep.Results {
		if r.Answer != ProbeNotProbed {
			t.Errorf("%s: %s; want not probed", r.Candidate, r.Answer)
		}
	}
}
