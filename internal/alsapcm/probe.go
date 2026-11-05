package alsapcm

import (
	"fmt"
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The silent probe (B728's design, section 5) asks the kernel whether the
// driver of an endpoint takes each of a list of combinations, and commits
// none: it opens the node as every open does, runs PVERSION and INFO, hands
// the node to the check after open, asks HW_REFINE of each candidate, and
// closes the node. It sends no HW_PARAMS, SW_PARAMS or PREPARE, writes
// nothing, writes no control and sends no HW_FREE, since nothing was set.
// HW_REFINE is computation in the kernel and selects no altset; the open can
// still wake a device from USB autosuspend, which is why a probe runs only
// at the moments the design names and never on a timer. What an answer
// establishes is the driver's acceptance (driver_accepted), never that the
// DAC decodes anything: for DoP, only that the kernel takes the PCM carrier.

// ProbeAnswer is the kernel's answer to one candidate of a probe.
type ProbeAnswer string

const (
	// ProbeNotProbed is a candidate the kernel was not asked about, or
	// answered with neither of the two: the node busy (EBUSY, another
	// process holding it, MPD most often), missing, closed to this process
	// or refused by the check after open, the probe ended before it, or a
	// refine that failed with any errno but EINVAL. It says nothing of the
	// candidate, and is never a refusal.
	ProbeNotProbed ProbeAnswer = "not_probed"
	// ProbeAccepted is a candidate HW_REFINE took: the driver accepts that
	// access, format, subformat, channel count and rate on the endpoint,
	// given the device's descriptors and the kernel's quirks.
	ProbeAccepted ProbeAnswer = "accepted"
	// ProbeRefused is a candidate HW_REFINE refused with EINVAL: a parameter
	// had no value left.
	ProbeRefused ProbeAnswer = "refused"
)

// ProbeCandidate is one combination a probe asks about: Format at RateHz
// frames a second, in two channels, with interleaved read-write access and
// the standard subformat, as an open asks them (exactParams). Format is a
// PCM format this backend packs or a native DSD format.
type ProbeCandidate struct {
	Format format.ALSAFormat
	RateHz int
}

func (c ProbeCandidate) String() string { return fmt.Sprintf("%s at %d Hz", c.Format, c.RateHz) }

// ProbeResult is the kernel's answer to one candidate.
type ProbeResult struct {
	Candidate ProbeCandidate
	Answer    ProbeAnswer
	// Msbits is the significant bits of a sample in an accepted candidate's
	// refined setup, the kernel's msbits: 0 when the kernel reported none,
	// which is unknown, never the format's width.
	Msbits int
	// Pause is an accepted candidate's refined setup offering the kernel's
	// PAUSE (SNDRV_PCM_INFO_PAUSE): its tuple pauses with PAUSE where it is
	// set, and with silence where it is not (the design's section 7.1).
	Pause bool
}

// DoPCarrier reports whether r establishes the DoP carrier of a DSD rate
// the player plays: S32_LE at that one-bit rate over 16 (176.4 kHz for
// DSD64, 352.8 for DSD128, 705.6 for DSD256), accepted with 24 to 32
// significant bits, what the DoP sink requires of its open (dopBits). It
// says only that the kernel takes the PCM carrier; nothing in it says the
// DAC decodes DSD.
func (r ProbeResult) DoPCarrier() bool {
	return r.Answer == ProbeAccepted && r.Candidate.Format == format.S32LE &&
		slices.Contains(probeBitRates, r.Candidate.RateHz*16) && dopBits(r.Msbits)
}

// dopBits reports whether a setup's significant bits carry DoP: 24 to 32,
// the bits a DoP word's marker and payload take. The DoP sink's open
// (dopPrecision) and the probe's carrier (DoPCarrier) ask it alike.
func dopBits(msbits int) bool { return msbits >= 24 && msbits <= 32 }

// MaxProbeCandidates bounds a probe: one open, PVERSION, INFO, one HW_REFINE
// a candidate, and a close.
const MaxProbeCandidates = 64

// ProbeConfig is what Probe asks.
type ProbeConfig struct {
	// Candidates are asked in order, each once: one at least, at most
	// MaxProbeCandidates, none repeated.
	Candidates []ProbeCandidate
	// Admit, when set, is the check after open (SinkConfig.Admit's): it is
	// given what the kernel says the opened node is, before any candidate is
	// asked, and a refusal closes the node with nothing asked of it
	// (ReasonWrongDevice), so no answer is ever given for a unit that took
	// the endpoint of the one the binding named.
	Admit func(NodeInfo) error
}

// ProbeReport is what one probe established.
type ProbeReport struct {
	Endpoint Endpoint
	// Opened reports whether the probe opened the node, and so closed it,
	// once: false when it was refused before the open or at it, a busy node
	// among them.
	Opened bool
	// Node is what the kernel said the opened node is (INFO), zero when it
	// was not opened or not read.
	Node NodeInfo
	// Results are the candidates' answers, in the order asked: every one
	// ProbeNotProbed when the probe asked nothing.
	Results []ProbeResult
	// Shared says why the run kept no refusal, each refused candidate
	// answered not probed, its accepted ones standing: another of the card's
	// substreams was open before the first refine or before the close (by
	// its /proc/asound name, "pcm0c/sub0"), or the card's substreams could
	// not be read, or did not show the probe's own open. A USB device's
	// interfaces can share a clock, which snd-usb-audio holds to the rate one
	// of them runs at, so beside another open substream a refine refuses
	// rates the DAC takes; the clock only narrows what the kernel takes, so
	// an acceptance stands. Empty for a run that kept its refusals.
	Shared []string
}

// Result is c's answer in r, ProbeNotProbed when r does not hold it.
func (r ProbeReport) Result(c ProbeCandidate) ProbeResult {
	for _, x := range r.Results {
		if x.Candidate == c {
			return x
		}
	}
	return ProbeResult{Candidate: c, Answer: ProbeNotProbed}
}

// notAsked is the report of a probe that asked nothing of ep.
func notAsked(ep Endpoint, cfg ProbeConfig) ProbeReport {
	rep := ProbeReport{Endpoint: ep, Results: make([]ProbeResult, len(cfg.Candidates))}
	for i, c := range cfg.Candidates {
		rep.Results[i] = ProbeResult{Candidate: c, Answer: ProbeNotProbed}
	}
	return rep
}

// probeBitRates are the DSD rates a probe asks about, one-bit rates a
// channel: DSD64, DSD128 and DSD256. DSD512 is refused in every mode
// (rate_not_supported) and never probed.
var probeBitRates = []int{2822400, 5644800, 11289600}

// probeTargets are the conversion targets of the design's section 4.3: the
// 44.1 kHz family to 176.4 kHz, best first, each in the 24-bit packings in
// the backend's order and then in 16 bits, for a DAC that takes no 24-bit
// target.
var probeTargets = struct {
	rates   []int
	formats []format.ALSAFormat
}{[]int{176400, 88200, 44100}, []format.ALSAFormat{format.S243LE, format.S24LE, format.S32LE, format.S16LE}}

// ProbeCandidates is the design's section 5 set, for DSD64, DSD128 and
// DSD256: each native DSD format at the one-bit rate over its width
// (DSD_U32_BE and DSD_U32_LE at r/32, DSD_U16_LE at r/16), in the order the
// player prefers them; S32_LE at each DoP carrier, r/16, whose answer carries
// the significant bits; and the conversion targets of section 4.3, 176.4,
// 88.2 and 44.1 kHz in S24_3LE, S24_LE, S32_LE and S16_LE. A candidate two of
// those name (S32_LE at 176.4 kHz, DSD64's carrier and a target) is listed
// once, in its first place. Each call returns a list of its own.
func ProbeCandidates() []ProbeCandidate {
	var out []ProbeCandidate
	seen := map[ProbeCandidate]bool{}
	add := func(c ProbeCandidate) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, r := range probeBitRates {
		for _, f := range []format.ALSAFormat{format.DSDU32BE, format.DSDU32LE, format.DSDU16LE} {
			add(ProbeCandidate{Format: f, RateHz: r / (8 * format.DSDWordBytes(f))})
		}
	}
	for _, r := range probeBitRates {
		add(ProbeCandidate{Format: format.S32LE, RateHz: r / 16})
	}
	for _, rate := range probeTargets.rates {
		for _, f := range probeTargets.formats {
			add(ProbeCandidate{Format: f, RateHz: rate})
		}
	}
	return out
}

// resolve checks the candidates and returns each one's format code.
func (cfg ProbeConfig) resolve(ep Endpoint) ([]uint32, error) {
	bad := func(detail string) ([]uint32, error) {
		return nil, &Error{Reason: ReasonBadConfig, Op: "probe", Endpoint: ep, Detail: detail}
	}
	switch n := len(cfg.Candidates); {
	case n == 0:
		return bad("a probe asks one candidate at least")
	case n > MaxProbeCandidates:
		return bad(fmt.Sprintf("%d candidates: a probe asks %d at most", n, MaxProbeCandidates))
	}
	codes := make([]uint32, len(cfg.Candidates))
	seen := make(map[ProbeCandidate]bool, len(cfg.Candidates))
	for i, c := range cfg.Candidates {
		sf, ok := sampleFormats[c.Format]
		if !ok {
			sf, ok = nativeFormats[c.Format]
		}
		switch {
		case !ok:
			return bad(fmt.Sprintf("format %q is neither a PCM format this backend packs nor a native DSD format", c.Format))
		case c.RateHz < MinRateHz || c.RateHz > MaxRateHz:
			return bad(fmt.Sprintf("rate %d Hz is outside %d..%d", c.RateHz, MinRateHz, MaxRateHz))
		case seen[c]:
			return bad(c.String() + " is asked twice")
		}
		seen[c] = true
		codes[i] = sf.code
	}
	return codes, nil
}

// probe is Probe over a kernel seam: the node opened as Open opens it, once;
// PVERSION and INFO, as an open checks them; the check after open; one
// HW_REFINE a candidate, its answer recorded; and the close, once, on every
// way out, a panic's included. Nothing else is sent: no HW_PARAMS, so the
// kernel commits nothing and the stream stays OPEN, and so no SW_PARAMS,
// PREPARE, write, PAUSE, DROP or HW_FREE. An open refused EBUSY is answered
// at once (ReasonBusy, every candidate not probed): nothing waits for a busy
// node, and its holder is left alone. Only EINVAL refuses a candidate; any
// other error ends the probe, typed, and leaves that candidate and the rest
// not probed. The card's open substreams are read after the check after
// open and again before the close, and a run beside another of them, or one
// whose reading cannot tell, keeps no refusal (ProbeReport.Shared). The
// report holds what was established whatever the error.
func probe(s sys, ep Endpoint, cfg ProbeConfig) (rep ProbeReport, err error) {
	rep = notAsked(ep, cfg)
	if err := ep.validate(); err != nil {
		return rep, err
	}
	codes, err := cfg.resolve(ep)
	if err != nil {
		return rep, err
	}
	fd, err := s.open(ep.Path())
	if err != nil {
		return rep, openError(ep, err)
	}
	rep.Opened = true
	// A Device that never set the hardware: its release is the close alone.
	d := &Device{sys: s, fd: fd, params: Params{Endpoint: ep}}
	defer func() {
		if cerr := d.release(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err := d.checkProtocol(); err != nil {
		return rep, err
	}
	if err := d.checkInfo(); err != nil {
		return rep, err
	}
	rep.Node = NodeInfo{Card: ep.Card, Device: ep.Device, Subdevice: d.params.Subdevice, Playback: true, ID: d.params.ID, Name: d.params.Name}
	if cfg.Admit != nil {
		if err := cfg.Admit(rep.Node); err != nil {
			return rep, &Error{Reason: ReasonWrongDevice, Op: "probe", Endpoint: ep,
				Detail: "the check after open refused the node: nothing was asked of it", Err: err}
		}
	}
	// The card's other substreams, read before the first refine and again
	// before the close (this defer runs before the release's): beside
	// another one open, or a reading that cannot tell, the run keeps no
	// refusal (sharedWith; probe_substreams.go).
	own := fmt.Sprintf("pcm%dp/sub%d", ep.Device, rep.Node.Subdevice)
	shared := sharedWith(s, ep.Card, own)
	defer func() {
		for _, why := range sharedWith(s, ep.Card, own) {
			if !slices.Contains(shared, why) {
				shared = append(shared, why)
			}
		}
		if len(shared) == 0 {
			return
		}
		rep.Shared = shared
		for i := range rep.Results {
			if rep.Results[i].Answer == ProbeRefused {
				rep.Results[i].Answer = ProbeNotProbed
			}
		}
	}()
	for i, code := range codes {
		hw := exactParams(code, uint32(cfg.Candidates[i].RateHz))
		// The kernel fills both in its answer (HW_REFINE resets msbits and
		// computes it, and gives the setup's info); zeroed, a kernel that
		// did not would read as unknown bits and no PAUSE, never as the
		// request's.
		hw.info, hw.msbits = 0, 0
		err := d.refine(&hw)
		res := &rep.Results[i]
		if err == nil {
			res.Answer, res.Msbits, res.Pause = ProbeAccepted, int(hw.msbits), hw.info&infoPause != 0
			continue
		}
		if n, _ := errnoOf(err); n == errnoEINVAL {
			res.Answer = ProbeRefused
			continue
		}
		return rep, d.kernelError("hw_refine", err)
	}
	return rep, nil
}
