package alsapcm

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// dsdDAC is a fake node whose hardware is a USB DAC's two streaming
// altsets: PCM in S16_LE, S24_3LE and S32_LE at its eight PCM rates, and
// native DSD in DSD_U32_BE at the DSD64 and DSD128 frame rates alone (88.2
// and 176.4 kHz), as a DAC whose kernel quirk names DSD_U32_BE and stops at
// DSD128. It offers the kernel's PAUSE.
func dsdDAC(t *testing.T) *fakeKernel {
	k := newFake(t)
	k.hw.altsets = []fakeAltset{
		{formats: []uint32{formatS16LE, formatS243LE, formatS32LE}, rates: []uint32{44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000}},
		{formats: []uint32{formatDSDU32BE}, rates: []uint32{88200, 176400}},
	}
	k.hw.info |= infoPause
	return k
}

// dsdDACTakes is what dsdDAC takes of ProbeCandidates: DSD64 and DSD128 in
// DSD_U32_BE, the DSD64 and DSD128 DoP carriers, and every conversion target
// but S24_LE, which it does not offer. Every other candidate is refused.
var dsdDACTakes = map[ProbeCandidate]bool{
	{format.DSDU32BE, 88200}: true, {format.DSDU32BE, 176400}: true,
	{format.S32LE, 176400}: true, {format.S32LE, 352800}: true,
	{format.S243LE, 176400}: true, {format.S16LE, 176400}: true,
	{format.S243LE, 88200}: true, {format.S32LE, 88200}: true, {format.S16LE, 88200}: true,
	{format.S243LE, 44100}: true, {format.S32LE, 44100}: true, {format.S16LE, 44100}: true,
}

// codeOf is a candidate format's snd_pcm_format_t.
func codeOf(t *testing.T, f format.ALSAFormat) uint32 {
	t.Helper()
	if sf, ok := sampleFormats[f]; ok {
		return sf.code
	}
	if sf, ok := nativeFormats[f]; ok {
		return sf.code
	}
	t.Fatalf("no code for %s", f)
	return 0
}

// probeIoctls names the requests a probe's tests count.
var probeIoctls = map[uint32]string{
	ioctlPVersion: "PVERSION", ioctlInfo: "INFO", ioctlHWRefine: "HW_REFINE", ioctlHWParams: "HW_PARAMS",
	ioctlHWFree: "HW_FREE", ioctlSWParams: "SW_PARAMS", ioctlStatus: "STATUS", ioctlDelay: "DELAY",
	ioctlPrepare: "PREPARE", ioctlStart: "START", ioctlDrop: "DROP", ioctlDrain: "DRAIN",
	ioctlWriteIFrames: "WRITEI_FRAMES", ioctlPause: "PAUSE",
}

// ioctlsOf counts the requests k was sent, by name. k.mu is held.
func ioctlsOf(k *fakeKernel) map[string]int {
	out := map[string]int{}
	for _, c := range k.calls {
		name, ok := probeIoctls[c]
		if !ok {
			name = fmt.Sprintf("%#x", c)
		}
		out[name]++
	}
	return out
}

// TestTheProbeRefinesAndNeverCommits pins the silent probe's calls (B728's
// design, section 5): one open of the endpoint's node, PVERSION and INFO as
// an open runs them, the check after open given the node before anything is
// asked, one HW_REFINE a candidate asking exactly what an open asks (access,
// format, subformat, channels and rate), and one close; never HW_PARAMS,
// SW_PARAMS, PREPARE or WRITEI, nor any other request, so the kernel commits
// nothing and the stream never leaves OPEN. Each candidate's answer is the
// kernel's: accepted where an altset holds its format at its rate, refused
// with EINVAL where none does.
func TestTheProbeRefinesAndNeverCommits(t *testing.T) {
	k := dsdDAC(t)
	cands := ProbeCandidates()
	if len(cands) == 0 {
		t.Fatal("ProbeCandidates lists nothing to ask")
	}
	var admitted []NodeInfo
	callsAtAdmit := -1
	rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: cands, Admit: func(n NodeInfo) error {
		admitted = append(admitted, n)
		k.inspect(func(k *fakeKernel) { callsAtAdmit = len(k.calls) })
		return nil
	}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	node := NodeInfo{Card: 5, Device: 0, Subdevice: 0, Playback: true, ID: "USB Audio", Name: "USB Audio"}
	if len(admitted) != 1 || admitted[0] != node || callsAtAdmit != 2 {
		t.Errorf("the check after open was given %+v after %d calls; want the node once, after PVERSION and INFO alone", admitted, callsAtAdmit)
	}
	if !rep.Opened || rep.Node != node || rep.Endpoint != testEndpoint {
		t.Errorf("the report: opened %v, node %+v, endpoint %v", rep.Opened, rep.Node, rep.Endpoint)
	}
	k.inspect(func(k *fakeKernel) {
		if len(k.opened) != 1 || k.opens != 1 || k.closes != 1 || k.isOpen {
			t.Errorf("%d opens asked, %d opened, %d closes, open now %v; want one open and one close", len(k.opened), k.opens, k.closes, k.isOpen)
		}
		if len(k.afterClose) > 0 {
			t.Errorf("requests after the close: %#x", k.afterClose)
		}
		got := ioctlsOf(k)
		for _, never := range []string{"HW_PARAMS", "SW_PARAMS", "PREPARE", "WRITEI_FRAMES"} {
			if got[never] != 0 {
				t.Errorf("the probe sent %s %d times", never, got[never])
			}
		}
		if want := map[string]int{"PVERSION": 1, "INFO": 1, "HW_REFINE": len(cands)}; !maps.Equal(got, want) {
			t.Errorf("the probe's requests %v; want %v and nothing else", got, want)
		}
		if k.committed != nil || k.sw != nil || k.state != stateOpen || len(k.played) != 0 || len(k.pauses) != 0 || len(k.pollEvents) != 0 {
			t.Errorf("the kernel was left committed %v, software parameters %v, state %s, %d bytes written, %d pauses, %d polls; want nothing set and the stream OPEN",
				k.committed != nil, k.sw != nil, State(k.state), len(k.played), len(k.pauses), len(k.pollEvents))
		}
		if len(k.refines) != len(cands) {
			t.Fatalf("%d refines for %d candidates", len(k.refines), len(cands))
		}
		for i, hw := range k.refines {
			c := cands[i]
			access, aok := hw.maskOf(hwParamAccess).single()
			f, fok := hw.maskOf(hwParamFormat).single()
			sub, sok := hw.maskOf(hwParamSubformat).single()
			ch, cok := hw.intervalOf(hwParamChannels).exact()
			r, rok := hw.intervalOf(hwParamRate).exact()
			if !aok || access != accessRWInterleaved || !fok || f != codeOf(t, c.Format) || !sok || sub != subformatStd ||
				!cok || ch != Channels || !rok || r != uint32(c.RateHz) {
				t.Errorf("the refine for %s asked access %v format %v subformat %v channels %s rate %s; want what an open asks",
					c, hw.maskOf(hwParamAccess).values(), hw.maskOf(hwParamFormat).values(), hw.maskOf(hwParamSubformat).values(),
					hw.intervalOf(hwParamChannels).raw(), hw.intervalOf(hwParamRate).raw())
			}
		}
	})
	if len(rep.Results) != len(cands) {
		t.Fatalf("%d results for %d candidates", len(rep.Results), len(cands))
	}
	for i, r := range rep.Results {
		want := ProbeRefused
		if dsdDACTakes[cands[i]] {
			want = ProbeAccepted
		}
		if r.Candidate != cands[i] || r.Answer != want {
			t.Errorf("result %d: %s %s; want %s %s", i, r.Candidate, r.Answer, cands[i], want)
		}
	}
}

// TestTheProbeLeavesABusyNodeAlone pins a busy node (B728's design, section
// 5): the open is refused EBUSY, as while MPD holds the DAC, and the probe
// answers at once, typed busy, every candidate not probed, having asked for
// one open and sent nothing else: no request, no poll, and no second open,
// although the node would open now. The holder is left alone. A busy probe
// is never kept as a refusal, and leaves an answer kept before standing.
func TestTheProbeLeavesABusyNodeAlone(t *testing.T) {
	k := dsdDAC(t)
	k.openErrs = []error{errnoEBUSY}
	cands := ProbeCandidates()
	if len(cands) == 0 {
		t.Fatal("ProbeCandidates lists nothing to ask")
	}
	done := make(chan struct{})
	var rep ProbeReport
	var err error
	go func() {
		defer close(done)
		rep, err = probe(k, testEndpoint, ProbeConfig{Candidates: cands})
	}()
	await(t, done, "the probe of a busy node to answer")
	wantReason(t, err, ReasonBusy)
	if rep.Opened || rep.Node != (NodeInfo{}) || len(rep.Results) != len(cands) {
		t.Errorf("a busy node's report: opened %v, node %+v, %d results", rep.Opened, rep.Node, len(rep.Results))
	}
	for _, r := range rep.Results {
		if r.Answer != ProbeNotProbed || r.Msbits != 0 || r.Pause {
			t.Errorf("%s on a busy node: %+v; want not probed", r.Candidate, r)
		}
	}
	k.inspect(func(k *fakeKernel) {
		if len(k.opened) != 1 || k.opens != 0 || k.closes != 0 || len(k.calls) != 0 || len(k.pollEvents) != 0 {
			t.Errorf("a busy node: %d opens asked, %d opened, %d closes, %d requests, %d polls; want the one open asked and nothing else",
				len(k.opened), k.opens, k.closes, len(k.calls), len(k.pollEvents))
		}
	})

	cache := NewProbeCache()
	u := ProbeUnit{Model: "dacKey/1\n262a\n9023\n=1\n=Brooklyn DAC+", Firmware: "0102", Kernel: "6.12.47+rpt-rpi-v8", Enumeration: "feed 1, generation 4"}
	dsd64 := ProbeCandidate{format.DSDU32BE, 88200}
	cache.Record(u, ProbeReport{Results: []ProbeResult{{Candidate: dsd64, Answer: ProbeAccepted, Msbits: 32, Pause: true}}})
	cache.Record(u, rep)
	for _, c := range cands {
		want := ProbeResult{Candidate: c, Answer: ProbeNotProbed}
		if c == dsd64 {
			want = ProbeResult{Candidate: c, Answer: ProbeAccepted, Msbits: 32, Pause: true}
		}
		if got := cache.Answer(u, c); got != want {
			t.Errorf("after a busy probe the cache answers %s with %+v; want %+v", c, got, want)
		}
	}
}

// TestTheProbeReadsTheDoPCarriersSignificantBits pins what a probe says of
// a DoP carrier: the significant bits the kernel's refined setup reports for
// S32_LE, never the format's width, and a carrier only with 24 to 32 of them
// (DoPCarrier, the DoP sink's own requirement), accepted, in S32_LE, at a
// DSD rate's carrier. Bits the kernel did not report are 0, unknown, and no
// carrier; a refused S32_LE, another format with 24 bits, and S32_LE at a
// rate that carries no DSD rate the player plays (44.1 kHz) are none either.
func TestTheProbeReadsTheDoPCarriersSignificantBits(t *testing.T) {
	carrier := ProbeCandidate{format.S32LE, 176400}
	for _, c := range []struct {
		name string
		bits uint32
		dop  bool
	}{
		{"24 significant bits", 24, true},
		{"32 significant bits", 32, true},
		{"20 significant bits", 20, false},
		{"16 significant bits", 16, false},
		{"bits the kernel did not report", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := dsdDAC(t)
			k.hw.bits = map[uint32]uint32{formatS32LE: c.bits}
			rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: []ProbeCandidate{carrier}})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			r := rep.Result(carrier)
			if r.Answer != ProbeAccepted || r.Msbits != int(c.bits) || r.DoPCarrier() != c.dop {
				t.Errorf("the carrier: %+v, a DoP carrier %v; want accepted, %d bits, a DoP carrier %v", r, r.DoPCarrier(), c.bits, c.dop)
			}
		})
	}
	t.Run("a carrier refused, another format, a rate no DSD rate has", func(t *testing.T) {
		k := dsdDAC(t)
		k.hw.bits = map[uint32]uint32{formatS32LE: 24, formatS243LE: 24}
		refused, other, pcm := ProbeCandidate{format.S32LE, 705600}, ProbeCandidate{format.S243LE, 176400}, ProbeCandidate{format.S32LE, 44100}
		rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: []ProbeCandidate{refused, other, pcm}})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if r := rep.Result(refused); r.Answer != ProbeRefused || r.DoPCarrier() {
			t.Errorf("S32_LE at a rate the DAC does not take: %+v, a DoP carrier %v; want refused and none", r, r.DoPCarrier())
		}
		if r := rep.Result(other); r.Answer != ProbeAccepted || r.Msbits != 24 || r.DoPCarrier() {
			t.Errorf("S24_3LE with 24 bits: %+v, a DoP carrier %v; want accepted and none", r, r.DoPCarrier())
		}
		if r := rep.Result(pcm); r.Answer != ProbeAccepted || r.Msbits != 24 || r.DoPCarrier() {
			t.Errorf("S32_LE at 44.1 kHz with 24 bits: %+v, a DoP carrier %v; want accepted and none", r, r.DoPCarrier())
		}
	})
}

// TestTheProbeReadsWhetherTheSetupOffersPause pins the pause flag a probe
// reads (B728's design, sections 5 and 7.1): an accepted candidate's refined
// setup offers the kernel's PAUSE or it does not, and the answer says which;
// a refused candidate says nothing of it.
func TestTheProbeReadsWhetherTheSetupOffersPause(t *testing.T) {
	for _, pause := range []bool{true, false} {
		t.Run(fmt.Sprintf("PAUSE offered %v", pause), func(t *testing.T) {
			k := dsdDAC(t)
			if !pause {
				k.hw.info &^= infoPause
			}
			cands := ProbeCandidates()
			if len(cands) == 0 {
				t.Fatal("ProbeCandidates lists nothing to ask")
			}
			rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: cands})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			for _, r := range rep.Results {
				if want := pause && r.Answer == ProbeAccepted; r.Pause != want {
					t.Errorf("%s (%s): pause %v; want %v", r.Candidate, r.Answer, r.Pause, want)
				}
			}
		})
	}
}

// TestAProbeTheCheckAfterOpenRefusesAsksNothing pins the check after open: a
// node it refuses, another unit at the bound endpoint, is closed with no
// candidate asked, typed wrong_device, every candidate not probed.
func TestAProbeTheCheckAfterOpenRefusesAsksNothing(t *testing.T) {
	k := dsdDAC(t)
	another := errors.New("card 5 is another unit now")
	rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: []ProbeCandidate{{format.S32LE, 176400}},
		Admit: func(NodeInfo) error { return another }})
	if e := wantReason(t, err, ReasonWrongDevice); !errors.Is(e, another) {
		t.Errorf("the refusal %v does not carry the check's", e)
	}
	if r := rep.Results[0]; r.Answer != ProbeNotProbed || !rep.Opened {
		t.Errorf("a node the check refused: %+v, opened %v; want not probed, opened", r, rep.Opened)
	}
	k.inspect(func(k *fakeKernel) {
		if got := ioctlsOf(k); k.opens != 1 || k.closes != 1 || k.isOpen || got["HW_REFINE"] != 0 {
			t.Errorf("a node the check refused: %d opens, %d closes, open %v, requests %v; want it closed with nothing refined", k.opens, k.closes, k.isOpen, got)
		}
	})
}

// TestAProbeStopsAtAKernelErrorThatIsNotEINVAL pins the line between a
// refusal and a failure: only EINVAL refuses a candidate. Any other errno
// from a refine (here ENODEV, the DAC unplugged under the probe) ends the
// probe, typed, the node closed once, and leaves that candidate and the rest
// not probed, never refused.
func TestAProbeStopsAtAKernelErrorThatIsNotEINVAL(t *testing.T) {
	k := dsdDAC(t)
	k.refineErrs = map[int]error{3: errnoENODEV}
	cands := []ProbeCandidate{{format.S32LE, 176400}, {format.DSDU32LE, 88200}, {format.S32LE, 352800}, {format.S16LE, 44100}}
	rep, err := probe(k, testEndpoint, ProbeConfig{Candidates: cands})
	wantReason(t, err, ReasonDisconnected)
	want := []ProbeAnswer{ProbeAccepted, ProbeRefused, ProbeNotProbed, ProbeNotProbed}
	for i, r := range rep.Results {
		if r.Answer != want[i] {
			t.Errorf("%s: %s; want %s", r.Candidate, r.Answer, want[i])
		}
	}
	k.inspect(func(k *fakeKernel) {
		if k.opens != 1 || k.closes != 1 || k.isOpen {
			t.Errorf("%d opens, %d closes, open %v; want the node closed once", k.opens, k.closes, k.isOpen)
		}
	})
}

// TestTheProbeRefusesWhatItWouldNotAsk pins the candidates a probe takes: a
// format that is neither a PCM format this backend packs nor a native DSD
// one, a rate outside MinRateHz..MaxRateHz, a candidate asked twice, none at
// all, and more than MaxProbeCandidates are refused before the node is
// opened, as bad_config, and so is an endpoint no card can have.
func TestTheProbeRefusesWhatItWouldNotAsk(t *testing.T) {
	ok := ProbeCandidate{format.S32LE, 176400}
	many := make([]ProbeCandidate, MaxProbeCandidates+1)
	for i := range many {
		many[i] = ProbeCandidate{format.S32LE, MinRateHz + i}
	}
	for _, c := range []struct {
		name   string
		ep     Endpoint
		cands  []ProbeCandidate
		reason Reason
	}{
		{"an unknown format", testEndpoint, []ProbeCandidate{ok, {"FLOAT_LE", 48000}}, ReasonBadConfig},
		{"a rate below the floor", testEndpoint, []ProbeCandidate{{format.S16LE, MinRateHz - 1}}, ReasonBadConfig},
		{"a rate past the ceiling", testEndpoint, []ProbeCandidate{{format.DSDU16LE, MaxRateHz + 1}}, ReasonBadConfig},
		{"a candidate twice", testEndpoint, []ProbeCandidate{ok, ok}, ReasonBadConfig},
		{"no candidate", testEndpoint, nil, ReasonBadConfig},
		{"too many candidates", testEndpoint, many, ReasonBadConfig},
		{"an endpoint no card has", Endpoint{Card: -1}, []ProbeCandidate{ok}, ReasonBadEndpoint},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := dsdDAC(t)
			rep, err := probe(k, c.ep, ProbeConfig{Candidates: c.cands})
			wantReason(t, err, c.reason)
			if rep.Opened || slices.ContainsFunc(rep.Results, func(r ProbeResult) bool { return r.Answer != ProbeNotProbed }) {
				t.Errorf("a probe refused: %+v", rep)
			}
			k.inspect(func(k *fakeKernel) {
				if len(k.opened) != 0 {
					t.Errorf("%d opens asked; want none", len(k.opened))
				}
			})
		})
	}
}

// TestTheProbesCandidatesAreTheDesigns pins ProbeCandidates to the design's
// section 5 set for DSD64, DSD128 and DSD256: each native format at the
// one-bit rate over its width, in the order the player prefers them
// (DSD_U32_BE, DSD_U32_LE, DSD_U16_LE); S32_LE at each DoP carrier, the rate
// over 16; and the conversion targets of section 4.3, 176.4, 88.2 and 44.1
// kHz in the 24-bit packings then S16_LE, the DSD64 carrier, S32_LE at 176.4
// kHz, listed once. DSD512 is never probed.
func TestTheProbesCandidatesAreTheDesigns(t *testing.T) {
	want := []ProbeCandidate{
		{format.DSDU32BE, 88200}, {format.DSDU32LE, 88200}, {format.DSDU16LE, 176400},
		{format.DSDU32BE, 176400}, {format.DSDU32LE, 176400}, {format.DSDU16LE, 352800},
		{format.DSDU32BE, 352800}, {format.DSDU32LE, 352800}, {format.DSDU16LE, 705600},
		{format.S32LE, 176400}, {format.S32LE, 352800}, {format.S32LE, 705600},
		{format.S243LE, 176400}, {format.S24LE, 176400}, {format.S16LE, 176400},
		{format.S243LE, 88200}, {format.S24LE, 88200}, {format.S32LE, 88200}, {format.S16LE, 88200},
		{format.S243LE, 44100}, {format.S24LE, 44100}, {format.S32LE, 44100}, {format.S16LE, 44100},
	}
	got := ProbeCandidates()
	if !slices.Equal(got, want) {
		t.Errorf("ProbeCandidates:\n got %v\nwant %v", got, want)
	}
	if len(got) > MaxProbeCandidates {
		t.Errorf("%d candidates, past MaxProbeCandidates", len(got))
	}
	if len(got) > 0 {
		got[0].RateHz = 1
		if ProbeCandidates()[0].RateHz == 1 {
			t.Error("ProbeCandidates hands out a list a caller can change for the next")
		}
	}
}
