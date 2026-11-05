//go:build linux && (amd64 || arm64)

package alsapcm

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// probeAuditSys is the real kernel with a record of what a probe asked of it:
// its opens, its closes and each request.
type probeAuditSys struct {
	linuxSys
	opens, closes int
	calls         []uint32
}

func (a *probeAuditSys) open(path string) (int, error) {
	a.opens++
	return a.linuxSys.open(path)
}

func (a *probeAuditSys) close(fd int) error {
	a.closes++
	return a.linuxSys.close(fd)
}

func (a *probeAuditSys) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	a.calls = append(a.calls, req)
	return a.linuxSys.ioctl(fd, req, arg)
}

func (a *probeAuditSys) ioctlValue(fd int, req uint32, arg uintptr) error {
	a.calls = append(a.calls, req)
	return a.linuxSys.ioctlValue(fd, req, arg)
}

// probeEndpointEnv names the endpoint TestTheSilentProbeOnARealDAC probes,
// "<card>,<device>".
const probeEndpointEnv = "BRIDGE_PROBE_ENDPOINT"

// TestTheSilentProbeOnARealDAC is the silent probe's hardware run (B728 B4):
// nothing in the bridge calls the probe until B7, so this is its entry
// point on a real DAC. It runs only when BRIDGE_PROBE_ENDPOINT names a
// playback endpoint as "<card>,<device>" (the card's index in
// /proc/asound/cards, and the PCM device, 0 for a USB DAC), and is skipped
// otherwise. It probes the design's whole set (ProbeCandidates) through the
// real kernel, recording what was asked of it, and logs one line a
// candidate: accepted, with the significant bits and whether the setup
// offers PAUSE (and "the DoP carrier of DSD64" where S32_LE at 176.4 kHz
// carries 24 to 32 bits, and so on), or refused (EINVAL), after a line
// naming any other of the card's substreams found open, beside which no
// refusal is kept (ProbeReport.Shared). It fails unless
// the node was opened once and closed once
// and the kernel was sent PVERSION, INFO and one HW_REFINE a candidate,
// nothing else: no HW_PARAMS, SW_PARAMS, PREPARE, write or PAUSE.
//
// On a box where MPD (or the bridge's player) holds the DAC the probe is
// refused busy and nothing is learned, and the test fails saying so: stop
// them first (sudo systemctl stop mpd, and the bridge's service), run it,
// and start them again after. Build it for the Pi on any host with
// GOOS=linux GOARCH=arm64 go test -c -o alsapcm.test ./internal/alsapcm, copy
// alsapcm.test across, and run it there as
// BRIDGE_PROBE_ENDPOINT=<card>,0 ./alsapcm.test -test.run
// '^TestTheSilentProbeOnARealDAC$' -test.v.
func TestTheSilentProbeOnARealDAC(t *testing.T) {
	spec := os.Getenv(probeEndpointEnv)
	if spec == "" {
		t.Skip(probeEndpointEnv + " names no endpoint: the hardware run probes a real DAC only when asked")
	}
	cardText, deviceText, ok := strings.Cut(spec, ",")
	card, cerr := strconv.Atoi(strings.TrimSpace(cardText))
	device, derr := strconv.Atoi(strings.TrimSpace(deviceText))
	if !ok || cerr != nil || derr != nil {
		t.Fatalf("%s=%q; want <card>,<device>, as 1,0", probeEndpointEnv, spec)
	}
	ep := Endpoint{Card: card, Device: device}
	a := &probeAuditSys{}
	cands := ProbeCandidates()
	rep, err := probe(a, ep, ProbeConfig{Candidates: cands})
	if ReasonOf(err) == ReasonBusy {
		t.Fatalf("%v\nthe DAC is held by another process, MPD or the bridge's player most often: stop them (sudo systemctl stop mpd, and the bridge's service), run this again, then start them again", err)
	}
	if err != nil {
		t.Fatalf("the probe of %s: %v", ep, err)
	}
	counts := map[string]int{}
	for _, c := range a.calls {
		name, ok := probeIoctls[c]
		if !ok {
			name = fmt.Sprintf("%#x", c)
		}
		counts[name]++
	}
	names := slices.Sorted(maps.Keys(counts))
	var said []string
	for _, n := range names {
		said = append(said, fmt.Sprintf("%s %d", n, counts[n]))
	}
	t.Logf("the probe of %s (%q, subdevice %d): opened %d, closed %d; requests: %s",
		ep, rep.Node.ID, rep.Node.Subdevice, a.opens, a.closes, strings.Join(said, ", "))
	if len(rep.Shared) > 0 {
		t.Logf("no refusal of this run is kept, each refused candidate below not probed: %s", strings.Join(rep.Shared, "; "))
	}
	for _, r := range rep.Results {
		switch r.Answer {
		case ProbeAccepted:
			pause, dop := "PAUSE not offered", ""
			if r.Pause {
				pause = "PAUSE offered"
			}
			if r.DoPCarrier() {
				dop = fmt.Sprintf(": the DoP carrier of DSD%d", r.Candidate.RateHz*16/44100)
			}
			bits := strconv.Itoa(r.Msbits) + " significant bits"
			if r.Msbits == 0 {
				bits = "significant bits not reported"
			}
			t.Logf("%-24s accepted, %s, %s%s", r.Candidate, bits, pause, dop)
		case ProbeRefused:
			t.Logf("%-24s refused (EINVAL)", r.Candidate)
		default:
			t.Logf("%-24s %s", r.Candidate, r.Answer)
		}
	}
	if a.opens != 1 || a.closes != 1 {
		t.Errorf("the node was opened %d times and closed %d; want once each", a.opens, a.closes)
	}
	if want := map[string]int{"PVERSION": 1, "INFO": 1, "HW_REFINE": len(cands)}; !maps.Equal(counts, want) {
		t.Errorf("the kernel was sent %v; want %v and nothing else", counts, want)
	}
}
